package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const supportedClaudeVersion = "2.1.292"

type claudeVersionCache struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	MtimeNS int64  `json:"mtime_ns"`
	Version string `json:"version"`
}

func claudeBinaryIdentity(binary string) (claudeVersionCache, error) {
	real, e := filepath.EvalSymlinks(binary)
	if e != nil {
		return claudeVersionCache{}, e
	}
	st, e := os.Stat(real)
	if e != nil {
		return claudeVersionCache{}, e
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return claudeVersionCache{}, fmt.Errorf("Claude binary is not executable: %s", real)
	}
	return claudeVersionCache{Path: real, Size: st.Size(), MtimeNS: st.ModTime().UnixNano(), Version: supportedClaudeVersion}, nil
}

// The pinned, root-owned executable does not change between sessions. Avoid a
// second full native CLI startup solely to print its version. Recheck after a
// path, size or modification time change; cache failures only lose this speedup.
func verifyClaudeVersion(binary, cachePath string) error {
	identity, e := claudeBinaryIdentity(binary)
	if e != nil {
		return e
	}
	var cached claudeVersionCache
	if readJSON(cachePath, &cached) == nil && cached == identity {
		return nil
	}
	output, e := exec.Command(binary, "--version").Output()
	if e != nil {
		return e
	}
	if !strings.HasPrefix(string(output), supportedClaudeVersion+" ") {
		return fmt.Errorf("this adapter was verified against Claude %s; installed version: %s", supportedClaudeVersion, output)
	}
	after, e := claudeBinaryIdentity(binary)
	if e != nil {
		return e
	}
	if after != identity {
		return fmt.Errorf("Claude binary changed during version verification; reconnect")
	}
	writeJSON(cachePath, identity)
	return nil
}
