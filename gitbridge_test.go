package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGitShimProcess(t *testing.T) {
	if os.Getenv("REMOTE_CLAUDE_GIT_TEST_HELPER") != "1" {
		return
	}
	var args []string
	if e := json.Unmarshal([]byte(os.Getenv("REMOTE_CLAUDE_GIT_TEST_ARGS")), &args); e != nil {
		os.Exit(99)
	}
	finish(bridgeGit(args))
	os.Exit(0)
}

func TestGitMetadataRunsOnWorkstation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Git shim runs on the Linux VPS")
	}
	if _, e := exec.LookPath("git"); e != nil {
		t.Skip("Git is not installed")
	}
	c, w := testWorker(t)
	c.RemoteRoot = t.TempDir() // Empty VPS mount stand-in: no .git here.
	w.config.RemoteRoot = c.RemoteRoot
	local := c.LocalRoot
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = local
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("fixture Git: %v: %s", e, b)
		}
	}
	git("init", "-q")
	git("config", "user.name", "Bridge test")
	git("config", "user.email", "bridge@example.invalid")
	os.Mkdir(filepath.Join(local, "space dir"), 0700)
	content := "Literal workstation path: " + local + "\n"
	os.WriteFile(filepath.Join(local, "space dir", "file.txt"), []byte(content), 0600)
	git("add", "--", "space dir/file.txt")
	git("-c", "core.hooksPath=/dev/null", "commit", "-qm", "fixture")
	os.WriteFile(filepath.Join(local, "untracked.txt"), []byte("new\n"), 0600)
	config := filepath.Join(t.TempDir(), "session.json")
	if e := writeJSON(config, c); e != nil {
		t.Fatal(e)
	}
	runAt := func(dir string, args ...string) (string, error) {
		b, _ := json.Marshal(args)
		cmd := exec.Command(os.Args[0], "-test.run=^TestGitShimProcess$")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "REMOTE_CLAUDE_GIT_TEST_HELPER=1", "REMOTE_CLAUDE_GIT_TEST_ARGS="+string(b), "REMOTE_CLAUDE_SESSION="+config)
		// Match Claude's background children: stdin is open but never used.
		reader, writer, e := os.Pipe()
		if e != nil {
			t.Fatal(e)
		}
		defer reader.Close()
		defer writer.Close()
		cmd.Stdin = reader
		out, e := cmd.CombinedOutput()
		return string(out), e
	}
	run := func(args ...string) (string, error) { return runAt(c.RemoteRoot, args...) }
	start := time.Now()
	if s, e := run("-c", "core.hooksPath=/dev/null", "--no-optional-locks", "status", "--short"); e != nil || !strings.Contains(s, "?? untracked.txt") {
		t.Fatalf("local status: %q, %v", s, e)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatal("read-only Git waited for an unused open stdin")
	}
	start = time.Now()
	if s, e := run("--version"); e != nil || !strings.HasPrefix(s, "git version ") {
		t.Fatalf("Git version: %q, %v", s, e)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatal("Git version waited for an unused open stdin")
	}
	if s, e := run("rev-parse", "--show-toplevel"); e != nil || strings.TrimSpace(s) != c.RemoteRoot {
		t.Fatalf("mapped root: %q, %v", s, e)
	}
	if s, e := run("-C", c.RemoteRoot+"/space dir", "rev-parse", "--show-toplevel"); e != nil || strings.TrimSpace(s) != c.RemoteRoot {
		t.Fatalf("absolute -C with spaces: %q, %v", s, e)
	}
	if s, e := runAt(t.TempDir(), "-C", c.RemoteRoot, "rev-parse", "--show-toplevel"); e != nil || strings.TrimSpace(s) != c.RemoteRoot {
		t.Fatalf("project Git called from outside the mount: %q, %v", s, e)
	}
	plugin := t.TempDir()
	if b, e := exec.Command("git", "init", "-q", plugin).CombinedOutput(); e != nil {
		t.Fatalf("plugin fixture: %v: %s", e, b)
	}
	if s, e := run("-C", plugin, "rev-parse", "--show-toplevel"); e != nil || strings.TrimSpace(s) != plugin {
		t.Fatalf("VPS plugin Git was sent to the workstation: %q, %v", s, e)
	}
	if s, e := run("show", "HEAD:space dir/file.txt"); e != nil || s != content {
		t.Fatalf("blob bytes were altered: %q, %v", s, e)
	}
	if _, e := run("rev-parse", "--verify", "refs/heads/does-not-exist"); e == nil {
		t.Fatal("Git failure was not propagated")
	}
}
