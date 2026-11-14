package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSearchRoutesPathsWithoutRewritingPatterns(t *testing.T) {
	for _, tc := range []struct{ args, want []string }{
		{[]string{"-n", "--glob", "*.go", "-e", "/remote/project/literal", "/remote/project/src"}, []string{"-n", "--glob", "*.go", "-e", "/remote/project/literal", "./src"}},
		{[]string{"--files", "--hidden", "/remote/project"}, []string{"--files", "--hidden", "."}},
		{[]string{"-in", "--", "-pattern", "dir with spaces"}, []string{"-i", "-n", "--", "-pattern", "./dir with spaces"}},
		{[]string{"--regexp=/remote/project", "--glob=**/*.go"}, []string{"--regexp", "/remote/project", "--glob", "**/*.go", "."}},
		{[]string{"--", "--files", "/remote/project"}, []string{"--", "--files", "."}},
		{[]string{"-e", "--files", "/remote/project"}, []string{"-e", "--files", "."}},
		{[]string{"/remote/project", "--files"}, []string{".", "--files"}},
	} {
		got, ok := searchArgs(tc.args, "/remote/project", "/remote/project")
		if !ok || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q => %q, %v; want %q", tc.args, got, ok, tc.want)
		}
	}
	for _, args := range [][]string{{"x", "../outside"}, {"x", "/remote/project-other"}, {"--pre", "cmd", "x"}, {"--follow", "x"}, {"--unknown", "x"}, {"-e", "x", "-"}, {"--file", "/etc/passwd"}} {
		if _, ok := searchArgs(args, "/remote/project", "/remote/project"); ok {
			t.Fatalf("must retain server behavior: %q", args)
		}
	}
}

func TestWorkstationSearchMatchesRipgrep(t *testing.T) {
	rg, err := workstationRipgrep()
	if err != nil {
		t.Skip("ripgrep unavailable")
	}
	c, _ := testWorker(t)
	os.Mkdir(filepath.Join(c.LocalRoot, "src"), 0700)
	os.WriteFile(filepath.Join(c.LocalRoot, ".gitignore"), []byte("ignored.txt\n"), 0600)
	os.WriteFile(filepath.Join(c.LocalRoot, "ignored.txt"), []byte("NEEDLE\n"), 0600)
	os.WriteFile(filepath.Join(c.LocalRoot, "src", "с пробелом.txt"), []byte("NEEDLE /remote/project literal\nsecond NEEDLE\n"), 0600)
	// A personal configuration must not change tool behavior.
	config := filepath.Join(t.TempDir(), "rgconfig")
	os.WriteFile(config, []byte("--glob=!*.txt\n"), 0600)
	t.Setenv("RIPGREP_CONFIG_PATH", config)
	for _, args := range [][]string{
		{"--files", "--hidden", "--sort", "path", "."},
		{"-l", "NEEDLE", "."},
		{"--json", "NEEDLE", "./src"},
		{"-n", "-A", "1", "/remote/project", "./src"},
		{"-0", "-l", "NEEDLE", "."},
		{"--files", "--no-ignore", "--sort", "path", "."},
		{"not-present", "."},
	} {
		args = append([]string{"--no-require-git", "--sort", "path"}, args...)
		cmd := exec.Command(rg, append([]string{"--no-config", "--path-separator", "/"}, args...)...)
		cmd.Dir = c.LocalRoot
		expected, nativeErr := cmd.Output()
		var out, errout bytes.Buffer
		result, err := bridgeExec(c, ExecRequest{Kind: "search", Args: args, Cwd: c.RemoteRoot}, nil, &out, &errout)
		if err != nil {
			t.Fatalf("%q: %v %s", args, err, errout.String())
		}
		code := 0
		if x, ok := nativeErr.(*exec.ExitError); ok {
			code = x.ExitCode()
		} else if nativeErr != nil {
			t.Fatal(nativeErr)
		}
		if result.Code != code {
			t.Fatalf("exit: got %d want %d", result.Code, code)
		}
		// JSON search statistics contain elapsed times, so compare actual matches.
		if strings.Contains(strings.Join(args, " "), "--json") {
			if !strings.Contains(out.String(), `NEEDLE /remote/project literal`) || !strings.Contains(out.String(), `./src/с пробелом.txt`) {
				t.Fatal(out.String())
			}
		} else if !bytes.Equal(out.Bytes(), expected) {
			t.Fatalf("%q: got %q want %q", args, out.Bytes(), expected)
		}
	}
	// A new search must immediately see edits made locally, without a content cache.
	os.WriteFile(filepath.Join(c.LocalRoot, "src", "new.txt"), []byte("FRESH\n"), 0600)
	var out, errout bytes.Buffer
	r, err := bridgeExec(c, ExecRequest{Kind: "search", Args: []string{"-l", "FRESH", "."}, Cwd: c.RemoteRoot}, nil, &out, &errout)
	if err != nil || r.Code != 0 || !strings.Contains(out.String(), "new.txt") {
		t.Fatalf("freshness: %+v %v %s", r, err, out.String())
	}
}

func TestSearchCancellationLeavesBridgeUsable(t *testing.T) {
	if _, err := workstationRipgrep(); err != nil {
		t.Skip("ripgrep unavailable")
	}
	c, _ := testWorker(t)
	client, err := dialBridge(c)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errout bytes.Buffer
	_, err = bridgeExecClientContext(ctx, client, ExecRequest{Kind: "search", Args: []string{"--files", "."}, Cwd: c.RemoteRoot}, nil, &out, &errout)
	if err == nil {
		t.Fatal("canceled request ran")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = bridgeExecClientContext(ctx, client, ExecRequest{Kind: "search", Args: []string{"--files", "."}, Cwd: c.RemoteRoot}, nil, &out, &errout)
	if err != nil {
		t.Fatal(err)
	}
}
