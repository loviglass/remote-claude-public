package main

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"remote-claude/third_party/gitignore"
)

// The replica holds the project's source: everything Git does not ignore. Ignored
// paths (dependencies, build output, secrets), .git and very large files stay on
// the workstation; the replica shows them as symlinks into the live SSHFS view,
// so they remain readable without being copied after every build.
type entryKind uint8

const (
	kindNone entryKind = iota
	kindFile
	kindDir
	kindSymlink
	kindLink
)

const maxReplicaFile = 16 << 20

// Temporary names used for atomic replacement on both machines. Scans skip them
// so a half-written file is never transferred.
const syncTempPrefix = ".rc-sync-"

type fileStat struct {
	Kind   entryKind
	Mode   uint32
	Size   int64
	Mtime  int64
	Ino    uint64
	Target string
}

func (a fileStat) same(b fileStat) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case kindLink, kindDir:
		return true
	case kindSymlink:
		return a.Target == b.Target
	}
	return a.Mode == b.Mode && a.Size == b.Size && a.Mtime == b.Mtime && a.Ino == b.Ino
}

type scanEntry struct {
	Path string
	St   fileStat
}

// scanResult lists entries in pre-order (parents before children).
type scanResult struct {
	list   []scanEntry
	byPath map[string]fileStat
}

type treeScanner struct {
	root string
	// Workstation scans classify ignored paths as links; the server instead
	// recognises its own symlinks into liveRoot.
	ignores  *ignoreRules
	liveRoot string
	// Windows has no permission bits worth propagating.
	noModes bool
	// shallow shows every top-level entry as a link: nothing is copied. It
	// serves directories that are not a reasonably sized Git project.
	shallow bool
	// A measuring scan stops once it has seen more than these limits.
	maxEntries int
	maxBytes   int64
	entries    int
	bytes      int64
}

func (s *treeScanner) exceeded() bool {
	return s.maxEntries > 0 && s.entries > s.maxEntries || s.maxBytes > 0 && s.bytes > s.maxBytes
}

func (s *treeScanner) scan() scanResult {
	r := scanResult{byPath: map[string]fileStat{}}
	var patterns []gitignore.Pattern
	if s.ignores != nil {
		patterns = s.ignores.base
	}
	s.walk("", s.root, patterns, &r)
	return r
}

func (s *treeScanner) walk(rel, abs string, patterns []gitignore.Pattern, r *scanResult) {
	entries, err := os.ReadDir(abs)
	if err != nil {
		return
	}
	var domain []string
	if rel != "" {
		domain = strings.Split(rel, "/")
	}
	if s.ignores != nil {
		if b, e := os.ReadFile(filepath.Join(abs, ".gitignore")); e == nil {
			patterns = append(patterns[:len(patterns):len(patterns)], parseIgnore(b, domain)...)
		}
	}
	var matcher gitignore.Matcher
	if s.ignores != nil {
		matcher = gitignore.NewMatcher(patterns)
	}
	for _, d := range entries {
		name := d.Name()
		if strings.HasPrefix(name, syncTempPrefix) {
			continue
		}
		if s.shallow && rel == "" {
			r.list = append(r.list, scanEntry{name, fileStat{Kind: kindLink}})
			r.byPath[name] = fileStat{Kind: kindLink}
			continue
		}
		if s.exceeded() {
			return
		}
		p := name
		if rel != "" {
			p = rel + "/" + name
		}
		childAbs := filepath.Join(abs, name)
		info, err := d.Info()
		if err != nil {
			continue
		}
		st, ok := s.classify(p, childAbs, info, matcher, domain)
		if !ok {
			continue
		}
		r.list = append(r.list, scanEntry{p, st})
		r.byPath[p] = st
		s.entries++
		s.bytes += st.Size
		if st.Kind == kindDir {
			s.walk(p, childAbs, patterns, r)
		}
	}
}

func (s *treeScanner) classify(p, abs string, info fs.FileInfo, matcher gitignore.Matcher, domain []string) (fileStat, bool) {
	mode := info.Mode()
	isDir := mode.IsDir()
	if matcher != nil {
		name := path.Base(p)
		if name == ".git" || matcher.Match(append(domain[:len(domain):len(domain)], name), isDir) {
			return fileStat{Kind: kindLink}, true
		}
	}
	st := fileStat{Mtime: info.ModTime().UnixNano(), Ino: fileIno(info)}
	if !s.noModes {
		st.Mode = uint32(mode.Perm())
	}
	switch {
	case isDir:
		st.Kind = kindDir
		st.Mode, st.Mtime, st.Ino = 0, 0, 0
	case mode.IsRegular():
		if matcher != nil && info.Size() > maxReplicaFile {
			return fileStat{Kind: kindLink}, true
		}
		st.Kind = kindFile
		st.Size = info.Size()
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(abs)
		if err != nil {
			return st, false
		}
		if s.liveRoot != "" && target == filepath.Join(s.liveRoot, filepath.FromSlash(p)) {
			return fileStat{Kind: kindLink}, true
		}
		st = fileStat{Kind: kindSymlink, Target: target}
	default:
		// Sockets, FIFOs and devices are not project content.
		return st, false
	}
	return st, true
}

// lstatEntry classifies one path the way a scan of its directory would.
func (s *treeScanner) lstatEntry(p string) fileStat {
	abs := filepath.Join(s.root, filepath.FromSlash(p))
	info, err := os.Lstat(abs)
	if err != nil {
		return fileStat{}
	}
	// Ignore rules are applied by full scans; a single lookup only needs the
	// physical kind.
	st, ok := s.classify(p, abs, info, nil, nil)
	if !ok {
		return fileStat{}
	}
	return st
}

type ignoreRules struct {
	base []gitignore.Pattern
	// git reports whether the project is inside a Git work tree.
	git bool
}

func parseIgnore(data []byte, domain []string) []gitignore.Pattern {
	var ps []gitignore.Pattern
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, `\#`) {
			line = line[1:]
		}
		ps = append(ps, gitignore.ParsePattern(line, domain))
	}
	return ps
}

// Projects outside Git still should not copy dependency trees.
var defaultIgnores = []string{"node_modules/", ".venv/", "venv/", "__pycache__/", ".tox/", ".mypy_cache/", ".pytest_cache/", ".gradle/"}

// loadIgnoreRules gathers the rules Git applies to the project root besides its
// own .gitignore files: the global excludes file, info/exclude and the
// .gitignore files of parent directories when the project is a subdirectory.
// A mismatch with Git only moves a path between the replica and the live view,
// both of which show the workstation's content.
func loadIgnoreRules(root string) *ignoreRules {
	r := &ignoreRules{}
	git := func(args ...string) string {
		b, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	top := git("rev-parse", "--show-toplevel")
	if top == "" {
		for _, p := range defaultIgnores {
			r.base = append(r.base, gitignore.ParsePattern(p, nil))
		}
		return r
	}
	r.git = true
	excludes := git("config", "--path", "core.excludesFile")
	if excludes == "" {
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			excludes = filepath.Join(x, "git", "ignore")
		} else if h, err := os.UserHomeDir(); err == nil {
			excludes = filepath.Join(h, ".config", "git", "ignore")
		}
	}
	if b, err := os.ReadFile(excludes); err == nil {
		r.base = append(r.base, parseIgnore(b, nil)...)
	}
	if common := git("rev-parse", "--path-format=absolute", "--git-common-dir"); common != "" {
		if b, err := os.ReadFile(filepath.Join(common, "info", "exclude")); err == nil {
			r.base = append(r.base, unanchored(parseIgnoreLines(b))...)
		}
	}
	// Rules of parent directories between the repository top and the project
	// apply too; only unanchored ones can be translated without a domain.
	if top = filepath.Clean(top); top != filepath.Clean(root) {
		var dirs []string
		for d := filepath.Dir(root); strings.HasPrefix(d, top); d = filepath.Dir(d) {
			dirs = append([]string{d}, dirs...)
			if d == top || d == filepath.Dir(d) {
				break
			}
		}
		for _, d := range dirs {
			if b, err := os.ReadFile(filepath.Join(d, ".gitignore")); err == nil {
				r.base = append(r.base, unanchored(parseIgnoreLines(b))...)
			}
		}
	}
	return r
}

func parseIgnoreLines(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

func unanchored(lines []string) []gitignore.Pattern {
	var ps []gitignore.Pattern
	for _, l := range lines {
		if !strings.Contains(strings.TrimSuffix(l, "/"), "/") {
			ps = append(ps, gitignore.ParsePattern(l, nil))
		}
	}
	return ps
}

type contentHash [16]byte

func hashBytes(b []byte) contentHash {
	s := sha256.Sum256(b)
	var h contentHash
	copy(h[:], s[:])
	return h
}

func hashString(s string) contentHash { return hashBytes([]byte(s)) }

func noPermissionBits() bool { return runtime.GOOS == "windows" }

// Limits for copying a project to the server; larger trees use the live view.
const (
	replicaMaxEntries = 50000
	replicaMaxBytes   = 1 << 30
)

// replicaSuitable decides whether root is a project worth replicating: a Git
// work tree of reasonable size that is not the home directory or a
// filesystem root. Anything else (running Claude in ~, /, or a large or
// unversioned directory) uses the live view, so nothing is copied and
// personal files never land on the server's disk.
func replicaSuitable(root string, ignores *ignoreRules) (bool, string) {
	clean := filepath.Clean(root)
	if home, err := os.UserHomeDir(); err == nil && clean == filepath.Clean(home) {
		return false, "the home directory"
	}
	if filepath.Dir(clean) == clean {
		return false, "a filesystem root"
	}
	if ignores == nil || !ignores.git {
		return false, "not a Git project"
	}
	m := &treeScanner{root: root, ignores: ignores, maxEntries: replicaMaxEntries, maxBytes: replicaMaxBytes}
	m.scan()
	if m.exceeded() {
		return false, fmt.Sprintf("larger than %d files or %d MiB without Git-ignored paths", replicaMaxEntries, replicaMaxBytes>>20)
	}
	return true, ""
}
