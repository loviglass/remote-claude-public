package main

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestResumeCheckoutPreservesJobBranchAndUncommittedWork(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "job")
	run := func(args ...string) string {
		t.Helper()
		b, e := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v: %s", args, e, b)
		}
		return strings.TrimSpace(string(b))
	}
	if b, e := exec.Command("git", "init", "-q", dir).CombinedOutput(); e != nil {
		t.Fatalf("git init: %v: %s", e, b)
	}
	origin := "https://example.com/team/project.git"
	run("remote", "add", "origin", origin)
	run("config", "user.name", "Developer")
	run("config", "user.email", "developer@example.com")
	file := filepath.Join(dir, "work.txt")
	if e := os.WriteFile(file, []byte("committed\n"), 0600); e != nil {
		t.Fatal(e)
	}
	run("add", "work.txt")
	run("commit", "-qm", "Initial work")
	run("switch", "-qc", "claude/main-20261013-080931")
	if e := os.WriteFile(file, []byte("unfinished\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := prepareCheckout(dir, origin, strings.Repeat("0", 40), "main", "Developer", "developer@example.com", "", true); e != nil {
		t.Fatal(e)
	}
	if got := run("branch", "--show-current"); got != "claude/main-20261013-080931" {
		t.Fatalf("resume switched branch to %q", got)
	}
	if got := run("status", "--porcelain"); got != "M work.txt" {
		t.Fatalf("resume changed unfinished work: %q", got)
	}
	if b, e := os.ReadFile(file); e != nil || string(b) != "unfinished\n" {
		t.Fatalf("resume changed the file: %q, %v", b, e)
	}
}

func TestResumeCheckoutRequiresEarlierJob(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if e := prepareCheckout(dir, "https://example.com/team/project.git", strings.Repeat("0", 40), "main", "", "", "", true); e == nil || !strings.Contains(e.Error(), "no earlier server job") {
		t.Fatalf("missing job: %v", e)
	}
	if _, e := os.Stat(dir); !os.IsNotExist(e) {
		t.Fatalf("resume unexpectedly created a checkout: %v", e)
	}
}

func TestJobContainerFixesEverythingButImageAndClaudeArguments(t *testing.T) {
	u := &user.User{Uid: "2001", Gid: "2001", Username: "dev", HomeDir: "/home/dev"}
	run, e := jobContainer(u, "project", "job-0123456789ab", "/home/dev/server-jobs/app", []string{"--privileged", "--volume", "/:/host"})
	if e != nil {
		t.Fatal(e)
	}
	image := slices.Index(run, "remote-claude-job-u2001:job-0123456789ab")
	if image < 0 || !slices.Equal(run[image+1:], []string{"server", "job-run", "/home/dev/server-jobs/app", "--privileged", "--volume", "/:/host"}) {
		t.Fatalf("developer arguments must follow the image: %q", run)
	}
	options := strings.Join(run[:image], " ")
	for _, want := range []string{"--user 2001:2001", "--cap-drop ALL", "--security-opt no-new-privileges", "--cgroup-parent remoteclaude-u2001.slice", "--entrypoint " + serverBinary} {
		if !strings.Contains(options, want) {
			t.Errorf("missing %q in %q", want, options)
		}
	}
	if strings.Contains(options, "--privileged") || strings.Contains(options, "docker.sock") {
		t.Errorf("unsafe option in %q", options)
	}
	for _, dir := range []string{"/home/other/app", "/home/dev/../other", "/home/dev", "/etc", "relative"} {
		if _, e := jobContainer(u, "base", "job-0123456789ab", dir, nil); e == nil {
			t.Errorf("checkout %q outside the home was accepted", dir)
		}
	}
	if _, e := jobContainer(u, "ubuntu", "job-0123456789ab", "/home/dev/app", nil); e == nil {
		t.Error("an arbitrary image name was accepted")
	}
}

func TestJobCheckoutSeparatesOrigins(t *testing.T) {
	ssh, https := jobCheckout("git@github.com:team/app.git"), jobCheckout("https://github.com/team/app.git")
	if ssh[:4] != "app-" || https[:4] != "app-" || ssh == https {
		t.Fatalf("checkout names %q and %q", ssh, https)
	}
	if other := jobCheckout("https://example.com/odd name/.git"); other[:8] != "project-" {
		t.Fatalf("unsafe name was kept: %q", other)
	}
}

func TestJobBranchNeverReusesTheSourceBranch(t *testing.T) {
	now := time.Date(2026, 10, 10, 9, 8, 7, 0, time.UTC)
	for base, want := range map[string]string{
		"main":         "claude/main-20261010-090807",
		"feature/x.1":  "claude/feature/x.1-20261010-090807",
		"HEAD":         "claude/task-20261010-090807",
		"":             "claude/task-20261010-090807",
		"-c protocol":  "claude/task-20261010-090807",
		"ветка":        "claude/task-20261010-090807",
		"a b; rm -rf ": "claude/task-20261010-090807",
	} {
		if got := jobBranch(base, now); got != want {
			t.Errorf("jobBranch(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestServerJobStartsFromWorkingTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	base := t.TempDir()
	t.Setenv("HOME", base)
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	origin := filepath.Join(base, "origin.git")
	git(base, "init", "-q", "--bare", "-b", "main", origin)
	pc := filepath.Join(base, "pc")
	git(base, "clone", "-q", origin, pc)
	git(pc, "config", "user.name", "Developer")
	git(pc, "config", "user.email", "developer@example.com")
	writeTree(t, pc, map[string]string{"pushed.txt": "pushed\n", "gone.txt": "to delete\n", ".gitignore": "*.secret\n"})
	git(pc, "add", ".")
	git(pc, "commit", "-qm", "pushed")
	git(pc, "push", "-q", "origin", "main")
	writeTree(t, pc, map[string]string{"local.txt": "unpushed commit\n"})
	git(pc, "add", "local.txt")
	git(pc, "commit", "-qm", "unpushed")
	writeTree(t, pc, map[string]string{"pushed.txt": "edited, not committed\n", "new.txt": "untracked\n", "key.secret": "ignored\n"})
	os.Remove(filepath.Join(pc, "gone.txt"))
	head := git(pc, "rev-parse", "HEAD")
	statusBefore := git(pc, "status", "--porcelain")

	bundle, snapshot, err := workingTreeBundle(pc, head, false)
	if err != nil || bundle == nil || snapshot == head {
		t.Fatalf("snapshot: %v (bundle %d bytes, %s)", err, len(bundle), snapshot)
	}
	if got := git(pc, "status", "--porcelain"); got != statusBefore {
		t.Fatalf("snapshot changed the workstation: %q", got)
	}
	if refs := git(pc, "for-each-ref", "refs/remote-claude"); refs != "" {
		t.Fatalf("temporary ref left behind: %s", refs)
	}
	p := uploadedSnapshot(snapshot)
	os.MkdirAll(filepath.Dir(p), 0700)
	if err := os.WriteFile(p, bundle, 0600); err != nil {
		t.Fatal(err)
	}
	job := filepath.Join(base, "server-jobs", "job")
	if err := prepareCheckout(job, origin, head, "main", "Developer", "developer@example.com", snapshot, false); err != nil {
		t.Fatal(err)
	}
	if got := git(job, "rev-parse", "HEAD"); got != head {
		t.Fatalf("job branch starts at %s, want the unpushed HEAD %s", got, head)
	}
	if got := readFile(t, filepath.Join(job, "pushed.txt")); got != "edited, not committed\n" {
		t.Fatalf("uncommitted edit: %q", got)
	}
	if got := readFile(t, filepath.Join(job, "new.txt")); got != "untracked\n" {
		t.Fatalf("untracked file: %q", got)
	}
	assertMissing(t, filepath.Join(job, "gone.txt"))
	assertMissing(t, filepath.Join(job, "key.secret"))
	status := git(job, "status", "--porcelain")
	for _, want := range []string{"M pushed.txt", "D gone.txt", "?? new.txt"} {
		if !strings.Contains(status, want) {
			t.Fatalf("job status %q lacks %q", status, want)
		}
	}
	assertMissing(t, p)
}

func TestServerJobBundleWithStaleOriginRefsFallsBackToFull(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	base := t.TempDir()
	t.Setenv("HOME", base)
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=d", "GIT_AUTHOR_EMAIL=d@x", "GIT_COMMITTER_NAME=d", "GIT_COMMITTER_EMAIL=d@x")
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	origin := filepath.Join(base, "origin.git")
	git(base, "init", "-q", "--bare", "-b", "main", origin)
	pc := filepath.Join(base, "pc")
	// A file:// URL transfers only reachable objects, like a real remote.
	remote := "file://" + origin
	git(base, "clone", "-q", remote, pc)
	writeTree(t, pc, map[string]string{"a.txt": "a\n"})
	git(pc, "add", ".")
	git(pc, "commit", "-qm", "base")
	git(pc, "push", "-q", "origin", "main")
	// A feature branch is pushed, then deleted on origin (for example after a
	// squash merge); the workstation still has its remote-tracking ref.
	git(pc, "switch", "-qc", "feature")
	writeTree(t, pc, map[string]string{"b.txt": "b\n"})
	git(pc, "add", ".")
	git(pc, "commit", "-qm", "feature")
	git(pc, "push", "-q", "origin", "feature")
	git(origin, "branch", "-D", "feature")
	writeTree(t, pc, map[string]string{"c.txt": "work in progress\n"})
	head := git(pc, "rev-parse", "HEAD")

	dir := filepath.Join(base, "server-jobs", jobCheckout(remote))
	thin, _, err := workingTreeBundle(pc, head, false)
	if err != nil {
		t.Fatal(err)
	}
	thinFile := filepath.Join(base, "thin.bundle")
	os.WriteFile(thinFile, thin, 0600)
	if bundleUsable(dir, remote, thinFile) {
		t.Fatal("a bundle needing a commit origin no longer has was accepted")
	}
	full, snapshot, err := workingTreeBundle(pc, head, true)
	if err != nil {
		t.Fatal(err)
	}
	fullFile := uploadedSnapshot(snapshot)
	os.MkdirAll(filepath.Dir(fullFile), 0700)
	os.WriteFile(fullFile, full, 0600)
	if !bundleUsable(dir, remote, fullFile) {
		t.Fatal("full bundle rejected")
	}
	if err := prepareCheckout(dir, remote, head, "feature", "d", "d@x", snapshot, false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "c.txt")); got != "work in progress\n" {
		t.Fatalf("work in progress: %q", got)
	}
}
