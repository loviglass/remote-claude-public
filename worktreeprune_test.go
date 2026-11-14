package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPruneIdleWorktrees(t *testing.T) {
	if _, e := exec.LookPath("git"); e != nil {
		t.Skip("Git is not installed")
	}
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		b, e := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v\n%s", args, e, b)
		}
		return strings.TrimSpace(string(b))
	}
	git(root, "init", "-q")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644)
	git(root, "add", "a.txt")
	git(root, "commit", "-qm", "init")
	trees := filepath.Join(root, ".claude", "worktrees")
	add := func(parent, name string) string {
		p := filepath.Join(parent, name)
		git(root, "worktree", "add", "-q", "-b", name, p)
		return p
	}
	age := func(p string, d time.Duration) {
		gitDir := git(p, "rev-parse", "--absolute-git-dir")
		old := time.Now().Add(-d)
		for _, n := range []string{"HEAD", "index", filepath.Join("logs", "HEAD")} {
			os.Chtimes(filepath.Join(gitDir, n), old, old)
		}
	}
	committed := add(trees, "committed")
	os.WriteFile(filepath.Join(committed, "b.txt"), []byte("b"), 0o644)
	git(committed, "add", "b.txt")
	git(committed, "commit", "-qm", "work")
	fresh := add(trees, "fresh")
	dirty := add(trees, "dirty")
	os.WriteFile(filepath.Join(dirty, "a.txt"), []byte("changed"), 0o644)
	untracked := add(trees, "untracked")
	os.WriteFile(filepath.Join(untracked, "new.txt"), []byte("n"), 0o644)
	locked := add(trees, "locked")
	git(root, "worktree", "lock", "--reason", "busy", locked)
	outside := add(filepath.Join(root, "other"), "outside")
	for _, p := range []string{committed, dirty, untracked, locked, outside} {
		age(p, 48*time.Hour)
	}
	age(fresh, time.Hour)

	if n := pruneIdleWorktrees(filepath.Join(root, ".claude"), worktreeIdleLimit); n != 1 {
		t.Fatalf("removed %d worktrees, want 1", n)
	}
	if _, e := os.Stat(committed); !os.IsNotExist(e) {
		t.Errorf("clean idle worktree was kept: %v", e)
	}
	if git(root, "log", "-1", "--format=%s", "committed") != "work" {
		t.Error("branch of the removed worktree lost its commit")
	}
	for _, p := range []string{fresh, dirty, untracked, locked, outside} {
		if _, e := os.Stat(p); e != nil {
			t.Errorf("%s was removed", filepath.Base(p))
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dirty, "a.txt")); string(b) != "changed" {
		t.Error("uncommitted change lost")
	}
}

func TestParseWorktreeList(t *testing.T) {
	a := parseWorktreeList([]byte("worktree /r\nHEAD 1\nbranch refs/heads/main\n\nworktree /r/.claude/worktrees/x\nHEAD 2\ndetached\nlocked\n\nworktree /r/.claude/worktrees/y\nHEAD 3\nlocked agent running\n\nworktree /r/.claude/worktrees/z\nHEAD 4\nprunable gitdir file points to non-existent location\n"))
	want := []listedWorktree{{filepath.FromSlash("/r"), false}, {filepath.FromSlash("/r/.claude/worktrees/x"), true}, {filepath.FromSlash("/r/.claude/worktrees/y"), true}, {filepath.FromSlash("/r/.claude/worktrees/z"), false}}
	if len(a) != len(want) {
		t.Fatalf("got %v", a)
	}
	for i := range want {
		if a[i] != want[i] {
			t.Errorf("%d: got %v, want %v", i, a[i], want[i])
		}
	}
}
