package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeLookup(uid string) (*user.User, error) {
	switch uid {
	case "1001":
		return &user.User{Uid: uid, Username: "developer1", HomeDir: "/home/developer1"}, nil
	case "1002":
		return &user.User{Uid: uid, Username: "developer2", HomeDir: "/home/developer2"}, nil
	}
	return nil, fmt.Errorf("unknown")
}

func TestJobListReportsEveryAccountsJobs(t *testing.T) {
	raw := []byte(`[
	 {"State":{"StartedAt":"2026-10-13T08:00:00Z"},"Config":{"Labels":{"remote-claude.uid":"1002"},"WorkingDir":"/home/developer2/server-jobs/proj-0a1b2c3d"}},
	 {"State":{"StartedAt":"2026-10-13T09:00:00Z"},"Config":{"Labels":{"remote-claude.uid":"1001"},"WorkingDir":"/home/developer1/server-jobs/other-ffffffff"}},
	 {"State":{"StartedAt":"2026-10-13T09:00:00Z"},"Config":{"Labels":{"remote-claude.uid":"1001"},"WorkingDir":"/etc"}},
	 {"State":{"StartedAt":"2026-10-13T09:00:00Z"},"Config":{"Labels":{"remote-claude.uid":"1001"},"WorkingDir":"/home/developer2/server-jobs/proj-0a1b2c3d"}},
	 {"State":{"StartedAt":"2026-10-13T09:00:00Z"},"Config":{"Labels":{"remote-claude.uid":"9"},"WorkingDir":"/home/x/server-jobs/p"}}]`)
	list, e := jobsFromInspect(raw, fakeLookup, func(string) string { return "claude/main-1" })
	if e != nil || len(list) != 2 {
		t.Fatalf("got %+v, %v; want two valid jobs (foreign dir, unknown uid and other account's dir dropped)", list, e)
	}
	if list[0].Account != "developer2" || list[0].Checkout != "proj-0a1b2c3d" || list[0].Branch != "claude/main-1" {
		t.Fatalf("%+v", list[0])
	}
}

func TestSameProjectUnderAnotherAccountIsReported(t *testing.T) {
	list := []jobInfo{
		{Account: "developer1", UID: "1001", Checkout: "proj-0a1b2c3d"},
		{Account: "developer2", UID: "1002", Checkout: "proj-0a1b2c3d", Branch: "claude/main-x", Started: time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)},
		{Account: "developer2", UID: "1002", Checkout: "other-ffffffff"},
	}
	got := otherAccountJobs(list, "proj-0a1b2c3d", "1001")
	if len(got) != 1 || got[0].Account != "developer2" {
		t.Fatalf("got %+v", got)
	}
	msg := conflictMessage(got)
	for _, want := range []string{"developer2", "claude/main-x", "2026-10-13 08:00", "--force-new", "--profile"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q: %s", want, msg)
		}
	}
	if len(otherAccountJobs(list, "proj-0a1b2c3d", "1002")) != 1 || len(otherAccountJobs(nil, "p", "1")) != 0 {
		t.Fatal("own jobs must not count")
	}
}

func TestForceNewSkipsTheCheck(t *testing.T) {
	if e := guardAgainstParallelJob("proj-0a1b2c3d", true); e != nil {
		t.Fatal(e)
	}
}

func TestBranchIsReadOnlyFromAPlainRefLine(t *testing.T) {
	d := t.TempDir()
	if readBranch(d) != "" {
		t.Fatal("no .git must give no branch")
	}
}

func TestBranchReadsRefAndRefusesSymlinkedHead(t *testing.T) {
	d := t.TempDir()
	if e := os.MkdirAll(filepath.Join(d, ".git"), 0700); e != nil {
		t.Fatal(e)
	}
	head := filepath.Join(d, ".git", "HEAD")
	os.WriteFile(head, []byte("ref: refs/heads/claude/main-1\n"), 0600)
	if got := readBranch(d); got != "claude/main-1" {
		t.Fatalf("got %q", got)
	}
	secret := filepath.Join(d, "secret")
	os.WriteFile(secret, []byte("ref: refs/heads/leak\n"), 0600)
	os.Remove(head)
	os.Symlink(secret, head)
	if got := readBranch(d); got != "" {
		t.Fatalf("followed a symlink: %q", got)
	}
}
