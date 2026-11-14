package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Claude's discovery inputs and small Git control files are batched. Its main
// UI thread also stats the project root and Git files synchronously, so their
// metadata must not wait for the workstation on every ancestor lookup.
func cachedConfigPath(p string) bool {
	return extraConfigPath(p) || baseConfigPath(p)
}

func baseConfigPath(p string) bool {
	if p == "" || p == ".git" || path.Dir(p) == ".git" || p == ".claude" || p == ".mcp.json" || p == "CLAUDE.md" || p == "CLAUDE.local.md" || p == ".claude/settings.json" || p == ".claude/settings.local.json" {
		return true
	}
	for _, root := range []string{".git/refs", ".claude/agents", ".claude/commands", ".claude/skills", ".claude/rules", ".claude/output-styles"} {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

func extraConfigPath(p string) bool {
	// Claude probes these additional instruction/task files synchronously.
	// The snapshot already stats immediate .claude children, including helpers
	// and nested repository markers; their contents and listings stay live.
	return extraConfigContent(p) || path.Dir(p) == ".claude"
}

func extraConfigContent(p string) bool {
	switch p {
	case "AGENTS.md", ".claude/CLAUDE.md", ".claude/CLAUDE.local.md", ".claude/scheduled_tasks.json", ".claude/settings.json", ".claude/settings.local.json", ".gitignore", ".rgignore", ".ignore", ".git/info/exclude":
		return true
	}
	return false
}

func cachedConfigContent(p string) bool {
	if extraConfigPath(p) {
		return extraConfigContent(p)
	}
	if p == ".git" || strings.HasPrefix(p, ".git/") {
		switch p {
		case ".git", ".git/HEAD", ".git/config", ".git/commondir", ".git/gitdir", ".git/packed-refs":
			return true
		}
		return p == ".git/refs" || strings.HasPrefix(p, ".git/refs/")
	}
	return cachedConfigPath(p)
}

func cachedConfigListing(p string) bool {
	return p == ".git/info" || baseConfigListing(p)
}

func baseConfigListing(p string) bool {
	if p == "" {
		return false // Ordinary project listings remain live.
	}
	if p == ".git" || p == ".git/refs" || strings.HasPrefix(p, ".git/refs/") {
		return true
	}
	if path.Dir(p) == ".claude" {
		for _, name := range []string{"agents", "commands", "skills", "rules", "output-styles"} {
			if p == ".claude/"+name {
				return true
			}
		}
		return false
	}
	return !strings.HasPrefix(p, ".git/") && baseConfigPath(p)
}

type configEntry struct {
	Path    string      `json:"path"`
	Bytes   int64       `json:"size"`
	Bits    os.FileMode `json:"mode"`
	MtimeNS int64       `json:"mtime_ns"`
	Data    []byte      `json:"data,omitempty"`
}

func (e configEntry) Name() string       { return path.Base(e.Path) }
func (e configEntry) Size() int64        { return e.Bytes }
func (e configEntry) Mode() os.FileMode  { return e.Bits }
func (e configEntry) ModTime() time.Time { return time.Unix(0, e.MtimeNS) }
func (e configEntry) IsDir() bool        { return e.Bits.IsDir() }
func (e configEntry) Sys() any           { return nil }

type configSnapshot struct {
	Digest  string                 `json:"digest"`
	Entries map[string]configEntry `json:"entries"`
}

const maxConfigSnapshotBytes = 64 << 20

func (f *projectFS) sendConfigSnapshot(q ExecRequest, out io.Writer) error {
	entries := make(map[string]configEntry)
	var total int64
	var walk func(string, bool) error
	walk = func(p string, descend bool) error {
		st, e := f.root.Lstat(fsPath(p))
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		entry := configEntry{Path: p, Bytes: st.Size(), Bits: st.Mode(), MtimeNS: st.ModTime().UnixNano()}
		if st.Mode().IsRegular() && cachedConfigContent(p) {
			if st.Size() > 32<<20 || total+st.Size() > 32<<20 {
				return fmt.Errorf("Claude configuration exceeds 32 MiB; use --fresh-metadata to disable its cache")
			}
			entry.Data, e = f.read(p)
			if e != nil {
				return e
			}
			entry.Bytes = int64(len(entry.Data))
			total += entry.Bytes
			if total > 32<<20 {
				return fmt.Errorf("projected Claude configuration exceeds 32 MiB")
			}
		}
		entries[p] = entry
		if len(entries) > 10000 {
			return fmt.Errorf("Claude configuration exceeds 10000 entries; use --fresh-metadata")
		}
		if !descend || !st.IsDir() {
			return nil
		}
		d, e := f.root.Open(fsPath(p))
		if e != nil {
			return e
		}
		children, e := d.ReadDir(-1)
		d.Close()
		if e != nil {
			return e
		}
		for _, child := range children {
			name := path.Join(p, child.Name())
			if e := walk(name, cachedConfigListing(name)); e != nil {
				return e
			}
		}
		return nil
	}
	for _, p := range []string{"", ".git", ".claude", ".mcp.json", "CLAUDE.md", "CLAUDE.local.md", "AGENTS.md", ".gitignore", ".rgignore", ".ignore"} {
		if e := walk(p, p == ".claude" || p == ".git"); e != nil {
			return e
		}
	}
	b, e := json.Marshal(entries)
	if e != nil {
		return e
	}
	if len(b) > maxConfigSnapshotBytes {
		return fmt.Errorf("encoded Claude configuration exceeds 64 MiB")
	}
	h := sha256.Sum256(b)
	snapshot := configSnapshot{Digest: hex.EncodeToString(h[:]), Entries: entries}
	if q.Spec["digest"] == snapshot.Digest {
		snapshot.Entries = nil
	}
	// Compression is explicitly requested by the receiver. Older servers keep
	// getting JSON, and older workers ignore the request and return plain JSON.
	// An unchanged digest is tiny, so avoid a gzip stream for revalidation.
	if q.Spec["compression"] == "gzip" && snapshot.Entries != nil {
		compressed, e := gzip.NewWriterLevel(out, gzip.BestSpeed)
		if e != nil {
			return e
		}
		e = json.NewEncoder(compressed).Encode(snapshot)
		closeErr := compressed.Close()
		if e != nil {
			return e
		}
		return closeErr
	}
	return json.NewEncoder(out).Encode(snapshot)
}

func decodeConfigSnapshot(data []byte) (configSnapshot, error) {
	var snapshot configSnapshot
	if len(data) > maxConfigSnapshotBytes {
		return snapshot, fmt.Errorf("encoded Claude configuration exceeds 64 MiB")
	}
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		compressed, e := gzip.NewReader(bytes.NewReader(data))
		if e != nil {
			return snapshot, e
		}
		defer compressed.Close()
		// Read through EOF to verify gzip's checksum, but bound expansion before
		// unmarshalling. Partial/corrupt snapshots must not replace a good cache.
		data, e = io.ReadAll(io.LimitReader(compressed, maxConfigSnapshotBytes+1))
		if e != nil {
			return snapshot, e
		}
		if len(data) > maxConfigSnapshotBytes {
			return snapshot, fmt.Errorf("expanded Claude configuration exceeds 64 MiB")
		}
	}
	if e := json.Unmarshal(data, &snapshot); e != nil {
		return snapshot, e
	}
	if snapshot.Digest == "" {
		return snapshot, fmt.Errorf("configuration snapshot missing digest")
	}
	return snapshot, nil
}

type cachedSFTP struct {
	ssh         *ssh.Client
	files       *sftp.Client
	gitSnapshot bool
	extraConfig bool
	mu          sync.Mutex
	current     atomic.Pointer[configCacheState]
	refreshing  atomic.Bool
	readSlots   chan struct{}
	missMu      sync.Mutex
	misses      map[string]time.Time
	missEpoch   uint64
}

// Claude's UI stats every word of a proposed Bash command as a possible path,
// synchronously and repeatedly. Absent names would otherwise cost a workstation
// round trip each time. Remember them as briefly as the mount trusts present
// entries, and forget them whenever a command or a write may have created one.
const missLifetime = time.Second

func (f *cachedSFTP) recentMiss(p string) bool {
	f.missMu.Lock()
	defer f.missMu.Unlock()
	t, ok := f.misses[p]
	return ok && time.Since(t) < missLifetime
}

func (f *cachedSFTP) missEpochNow() uint64 {
	f.missMu.Lock()
	defer f.missMu.Unlock()
	return f.missEpoch
}

// A lookup that overlapped a command or write may describe the old tree.
func (f *cachedSFTP) rememberMiss(p string, epoch uint64) {
	f.missMu.Lock()
	defer f.missMu.Unlock()
	if epoch != f.missEpoch {
		return
	}
	if f.misses == nil || len(f.misses) >= 4096 {
		f.misses = map[string]time.Time{}
	}
	f.misses[p] = time.Now()
}

func (f *cachedSFTP) forgetMisses() {
	f.missMu.Lock()
	defer f.missMu.Unlock()
	f.missEpoch++
	f.misses = nil
}

func (f *cachedSFTP) stat(p string, follow bool) (os.FileInfo, error) {
	if f.recentMiss(p) {
		return nil, os.ErrNotExist
	}
	epoch := f.missEpochNow()
	var st os.FileInfo
	var e error
	if follow {
		st, e = f.files.Stat("/" + p)
	} else {
		st, e = f.files.Lstat("/" + p)
	}
	if os.IsNotExist(e) {
		f.rememberMiss(p, epoch)
	}
	return st, e
}

type configCacheState struct {
	snapshot configSnapshot
	checked  time.Time
}

func (f *cachedSFTP) snapshot() (configSnapshot, error) {
	if s := f.current.Load(); s != nil && !s.checked.IsZero() {
		age := time.Since(s.checked)
		if age < time.Second {
			return s.snapshot, nil
		}
		if age < 5*time.Second {
			// Claude's UI uses synchronous stat calls. Revalidate independently
			// so a network round trip does not pause keyboard processing.
			if f.refreshing.CompareAndSwap(false, true) {
				go func() {
					defer f.refreshing.Store(false)
					f.refresh()
				}()
			}
			return s.snapshot, nil
		}
	}
	// Initial loads, own writes and an excessively old cache require a fresh
	// response. Failed refreshes cannot keep serving old data indefinitely.
	return f.refresh()
}

func (f *cachedSFTP) refresh() (configSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var current configSnapshot
	if s := f.current.Load(); s != nil {
		if !s.checked.IsZero() && time.Since(s.checked) < time.Second {
			return s.snapshot, nil
		}
		current = s.snapshot
	}
	var out, errout bytes.Buffer
	r, e := bridgeExecClient(f.ssh, ExecRequest{Kind: "config-snapshot", Spec: map[string]any{"digest": current.Digest, "compression": "gzip"}}, nil, &out, &errout)
	if e != nil || r.Code != 0 {
		return configSnapshot{}, fmt.Errorf("refresh Claude configuration: %v (exit %d)", e, r.Code)
	}
	next, e := decodeConfigSnapshot(out.Bytes())
	if e != nil {
		return configSnapshot{}, fmt.Errorf("invalid configuration snapshot: %v", e)
	}
	if next.Entries != nil {
		current = next
	} else if current.Digest != next.Digest || current.Entries == nil {
		return configSnapshot{}, fmt.Errorf("configuration snapshot missing its initial contents")
	}
	f.current.Store(&configCacheState{snapshot: current, checked: time.Now()})
	return current, nil
}

func (f *cachedSFTP) invalidate() {
	f.forgetMisses()
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.current.Load(); s != nil {
		f.current.Store(&configCacheState{snapshot: s.snapshot})
	}
}

func (f *cachedSFTP) lookup(p string) (configEntry, bool, error) {
	if !f.extraConfig && extraConfigPath(p) && !baseConfigPath(p) {
		return configEntry{}, false, nil
	}
	if !f.gitSnapshot && (p == "" || p == ".git" || strings.HasPrefix(p, ".git/")) {
		return configEntry{}, false, nil
	}
	if !cachedConfigPath(p) {
		return configEntry{}, false, nil
	}
	s, e := f.snapshot()
	if e != nil {
		return configEntry{}, true, e
	}
	// Follow symlinks through the workstation's rooted filesystem. A snapshot
	// never expands links or guesses the contents of a linked directory.
	for a := p; a != "." && a != ""; a = path.Dir(a) {
		if s.Entries[a].Mode()&os.ModeSymlink != 0 {
			return configEntry{}, false, nil
		}
	}
	x, ok := s.Entries[p]
	if !ok {
		return configEntry{}, true, os.ErrNotExist
	}
	return x, true, nil
}

func (f *cachedSFTP) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	p, e := cleanPath(r.Filepath)
	if e != nil {
		return nil, e
	}
	entry, cached, e := f.lookup(p)
	if e != nil {
		return nil, e
	}
	if cached && entry.Mode().IsRegular() && cachedConfigContent(p) {
		return bytes.NewReader(entry.Data), nil
	}
	if f.readSlots != nil && !projectedConfig(p) {
		select {
		case f.readSlots <- struct{}{}:
			var data boundedReadBuffer
			r, err := bridgeExecClient(f.ssh, ExecRequest{Kind: "read-file", Spec: map[string]any{"path": p}}, nil, &data, io.Discard)
			if err == nil && r.Code == 0 && !data.overflow {
				return &snapshotReader{Reader: bytes.NewReader(data.Bytes()), release: func() { <-f.readSlots }}, nil
			}
			<-f.readSlots
		default:
			// Bound per-open memory; busy or large reads use streaming SFTP.
		}
	}
	return f.files.Open("/" + p)
}

type cachedWrite struct {
	*sftp.File
	cache  *cachedSFTP
	failed atomic.Bool
}

func (w *cachedWrite) TransferError(e error) {
	w.failed.Store(true)
	// Closing the upstream SFTP channel makes the workstation discard its
	// incomplete projected configuration handles instead of committing them.
	w.cache.files.Close()
	w.cache.invalidate()
}

func (w *cachedWrite) Close() error {
	if w.failed.Load() {
		return fmt.Errorf("incomplete proxied write was discarded")
	}
	e := w.File.Close()
	w.cache.invalidate()
	return e
}

func (f *cachedSFTP) OpenFile(r *sftp.Request) (sftp.WriterAtReaderAt, error) {
	p, e := cleanPath(r.Filepath)
	if e != nil {
		return nil, e
	}
	flags := os.O_WRONLY
	a := r.Pflags()
	if a.Read {
		flags = os.O_RDWR
	}
	if a.Creat {
		flags |= os.O_CREATE
	}
	if a.Trunc {
		flags |= os.O_TRUNC
	}
	if a.Excl {
		flags |= os.O_EXCL
	}
	// The name may exist before the handle closes.
	f.forgetMisses()
	h, e := f.files.OpenFile("/"+p, flags)
	if e != nil {
		return nil, e
	}
	return &cachedWrite{File: h, cache: f}, nil
}

func (f *cachedSFTP) Filewrite(r *sftp.Request) (io.WriterAt, error) { return f.OpenFile(r) }

func (f *cachedSFTP) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	p, e := cleanPath(r.Filepath)
	if e != nil {
		return nil, e
	}
	entry, cached, e := f.lookup(p)
	if e != nil {
		return nil, e
	}
	if r.Method == "List" {
		if cached && cachedConfigListing(p) && (f.extraConfig || baseConfigListing(p)) {
			if !entry.IsDir() {
				return nil, fmt.Errorf("not a directory")
			}
			s, e := f.snapshot()
			if e != nil {
				return nil, e
			}
			var a fileList
			for name, child := range s.Entries {
				if name != p && path.Dir(name) == p {
					a = append(a, child)
				}
			}
			sort.Slice(a, func(i, j int) bool { return a[i].Name() < a[j].Name() })
			return a, nil
		}
		a, e := f.files.ReadDir("/" + p)
		return fileList(a), e
	}
	if cached {
		return fileList{entry}, nil
	}
	st, e := f.stat(p, true)
	if e != nil {
		return nil, e
	}
	return fileList{st}, nil
}

func (f *cachedSFTP) Lstat(r *sftp.Request) (sftp.ListerAt, error) {
	p, e := cleanPath(r.Filepath)
	if e != nil {
		return nil, e
	}
	entry, cached, e := f.lookup(p)
	if e != nil {
		return nil, e
	}
	if cached {
		return fileList{entry}, nil
	}
	st, e := f.stat(p, false)
	if e != nil {
		return nil, e
	}
	return fileList{st}, nil
}

func (f *cachedSFTP) RealPath(p string) (string, error) { return f.files.RealPath(p) }
func (f *cachedSFTP) Readlink(p string) (string, error) { return f.files.ReadLink(p) }
func (f *cachedSFTP) PosixRename(r *sftp.Request) error { return f.Filecmd(r) }
func (f *cachedSFTP) Filecmd(r *sftp.Request) error {
	defer f.invalidate()
	p, e := cleanPath(r.Filepath)
	if e != nil {
		return e
	}
	q, e := cleanPath(r.Target)
	if e != nil {
		return e
	}
	p, q = "/"+p, "/"+q
	switch r.Method {
	case "Rename":
		return f.files.Rename(p, q)
	case "PosixRename":
		return f.files.PosixRename(p, q)
	case "Remove":
		return f.files.Remove(p)
	case "Rmdir":
		return f.files.RemoveDirectory(p)
	case "Mkdir":
		return f.files.Mkdir(p)
	case "Symlink":
		return f.files.Symlink(r.Filepath, q)
	case "Link":
		return f.files.Link(p, q)
	case "Setstat":
		a, st := r.AttrFlags(), r.Attributes()
		if a.Size {
			if e := f.files.Truncate(p, int64(st.Size)); e != nil {
				return e
			}
		}
		if a.Permissions {
			if e := f.files.Chmod(p, os.FileMode(st.Mode).Perm()); e != nil {
				return e
			}
		}
		if a.Acmodtime {
			return f.files.Chtimes(p, time.Unix(int64(st.Atime), 0), time.Unix(int64(st.Mtime), 0))
		}
		return nil
	}
	return sftp.ErrSSHFxOpUnsupported
}

type stdioSFTP struct{}

func (stdioSFTP) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdioSFTP) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdioSFTP) Close() error                { return nil }

func serveCachedSFTP(client *ssh.Client, c SessionConfig) error {
	f := &cachedSFTP{ssh: client, gitSnapshot: c.GitSnapshot, extraConfig: c.ExtraConfig}
	if c.ExecSocket != "" {
		broker, e := startLocalExecutor(c, client, f.forgetMisses)
		if e != nil {
			return e
		}
		defer broker.close()
	}
	// SFTP initialization and the configuration RPC use independent channels.
	// Overlap their round trips before publishing the mounted filesystem.
	type backendResult struct {
		files *sftp.Client
		err   error
	}
	ready := make(chan backendResult, 1)
	go func() {
		files, e := sftp.NewClient(client)
		ready <- backendResult{files, e}
	}()
	if c.BulkRead {
		f.readSlots = make(chan struct{}, 8)
	}
	_, snapshotErr := f.snapshot()
	if snapshotErr != nil {
		client.Close()
	}
	backend := <-ready
	if backend.files != nil {
		defer backend.files.Close()
	}
	if snapshotErr != nil {
		return snapshotErr
	}
	if backend.err != nil {
		return backend.err
	}
	f.files = backend.files
	srv := sftp.NewRequestServer(stdioSFTP{}, sftp.Handlers{FileGet: f, FilePut: f, FileCmd: f, FileList: f}, sftp.WithConcurrentReads())
	defer srv.Close()
	e := srv.Serve()
	if e == io.EOF {
		return nil
	}
	return e
}
