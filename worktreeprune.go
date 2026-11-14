package main

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Claude keeps agent worktrees under .claude/worktrees when they finish. Each
// one costs synchronous metadata calls over the bridge whenever Claude scans
// that directory, and a hundred of them freeze the terminal for seconds.
const worktreeIdleLimit = 24 * time.Hour

// Remove worktrees under .claude/worktrees that are clean, unlocked and idle.
// Branches stay, so commits survive; uncommitted or untracked files keep the
// worktree. The idle limit protects a fresh worktree an agent is still using.
func pruneIdleWorktrees(root string, idle time.Duration) (removed int) {
	top, e := exec.Command("git", "-C", root, "rev-parse", "--show-toplevel").Output()
	if e != nil {
		return 0
	}
	parent := filepath.Join(filepath.Clean(strings.TrimSpace(string(top))), ".claude", "worktrees") + string(filepath.Separator)
	list, e := exec.Command("git", "-C", root, "worktree", "list", "--porcelain").Output()
	if e != nil {
		return 0
	}
	for _, w := range parseWorktreeList(list) {
		if w.locked || !strings.HasPrefix(w.path, parent) {
			continue
		}
		// Check the age first: status may refresh the index and touch it.
		if !worktreeIdle(w.path, idle) {
			continue
		}
		status, e := exec.Command("git", "--no-optional-locks", "-C", w.path, "status", "--porcelain").Output()
		if e != nil || len(status) > 0 {
			continue
		}
		// Without --force Git refuses again if anything changed meanwhile.
		if exec.Command("git", "-C", root, "worktree", "remove", w.path).Run() == nil {
			removed++
		}
	}
	return removed
}

type listedWorktree struct {
	path   string
	locked bool
}

func parseWorktreeList(b []byte) []listedWorktree {
	var a []listedWorktree
	s := bufio.NewScanner(bytes.NewReader(b))
	for s.Scan() {
		line := s.Text()
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			a = append(a, listedWorktree{path: filepath.Clean(filepath.FromSlash(p))})
		} else if len(a) > 0 && (line == "locked" || strings.HasPrefix(line, "locked ")) {
			a[len(a)-1].locked = true
		}
	}
	return a
}

// Git updates HEAD, its reflog and the index on checkout, commit and staging.
func worktreeIdle(path string, idle time.Duration) bool {
	dir, e := exec.Command("git", "-C", path, "rev-parse", "--absolute-git-dir").Output()
	if e != nil {
		return false
	}
	gitDir := strings.TrimSpace(string(dir))
	var latest time.Time
	for _, name := range []string{"HEAD", "index", filepath.Join("logs", "HEAD")} {
		if st, e := os.Stat(filepath.Join(gitDir, name)); e == nil && st.ModTime().After(latest) {
			latest = st.ModTime()
		}
	}
	return !latest.IsZero() && time.Since(latest) > idle
}
