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
	if e := prepareCheckout(dir, origin, strings.Repeat("0", 40), "main", "Developer", "developer@example.com", true); e != nil {
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
	if e := prepareCheckout(dir, "https://example.com/team/project.git", strings.Repeat("0", 40), "main", "", "", true); e == nil || !strings.Contains(e.Error(), "no earlier server job") {
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
