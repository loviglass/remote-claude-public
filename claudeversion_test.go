package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeVersionCacheRechecksReplacedBinary(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("native VPS binary verification uses Unix executable permissions")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "claude")
	checks := filepath.Join(dir, "checks")
	cache := filepath.Join(dir, "version.json")
	script := "#!/bin/sh\nprintf 'checked\\n' >> " + shellQuote(checks) + "\nprintf '2.1.292 (Claude Code)\\n'\n"
	if e := os.WriteFile(binary, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if e := verifyClaudeVersion(binary, cache); e != nil {
			t.Fatal(e)
		}
	}
	if b, e := os.ReadFile(checks); e != nil || string(b) != "checked\n" {
		t.Fatalf("unchanged binary should be checked once: %q, %v", b, e)
	}
	replacement := filepath.Join(dir, "replacement")
	if e := os.WriteFile(replacement, []byte("#!/bin/sh\nprintf '9.9.999 (Unsupported)\\n'\n"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Rename(replacement, binary); e != nil {
		t.Fatal(e)
	}
	if e := verifyClaudeVersion(binary, cache); e == nil {
		t.Fatal("replacement with an unsupported version reused the old verification")
	}
	if e := os.Remove(binary); e != nil {
		t.Fatal(e)
	}
	if e := verifyClaudeVersion(binary, cache); e == nil {
		t.Fatal("missing binary reused the old verification")
	}
}
