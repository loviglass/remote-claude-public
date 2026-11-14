package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Diagnostics exercises the same live mount and executor without an API call.
func serverDoctor(id string) error {
	var c SessionConfig
	if e := readJSON(filepath.Join(stateDir(), "sessions", id+".json"), &c); e != nil {
		return e
	}
	defer serverCleanup(id)
	timings := make(map[string][]float64)
	measure := func(name string, count int, fn func() error) error {
		for i := 0; i < count; i++ {
			start := time.Now()
			e := fn()
			timings[name] = append(timings[name], float64(time.Since(start).Microseconds())/1000)
			if e != nil {
				return e
			}
		}
		return nil
	}
	name := ".remote-claude-check-" + randomID() + ".txt"
	path := filepath.Join(c.RemoteRoot, name)
	if e := measure("file_write", 1, func() error { return os.WriteFile(path, []byte("REMOTE_CLAUDE_FILE_OK\n"), 0600) }); e != nil {
		return fmt.Errorf("write mounted project: %w", e)
	}
	defer os.Remove(path)
	if e := measure("file_read", 3, func() error {
		b, e := os.ReadFile(path)
		if e == nil && string(b) != "REMOTE_CLAUDE_FILE_OK\n" {
			e = fmt.Errorf("unexpected file contents")
		}
		return e
	}); e != nil {
		return fmt.Errorf("read mounted project: %w", e)
	}
	if e := measure("file_stat", 4, func() error { _, e := os.Stat(path); return e }); e != nil {
		return e
	}
	if e := measure("directory_list", 3, func() error { _, e := os.ReadDir(c.RemoteRoot); return e }); e != nil {
		return e
	}
	q := ExecRequest{Kind: "shell", Cwd: c.RemoteRoot, Command: "printf 'REMOTE_CLAUDE_COMMAND_OK\\n'; hostname; cat " + shellQuote(name) + "; printf 'REMOTE_CLAUDE_EDIT_OK\\n' >> " + shellQuote(name)}
	if c.OS == "windows" || c.Shell == "powershell" {
		q = ExecRequest{Kind: "powershell", Cwd: c.RemoteRoot, Args: []string{"-NoProfile", "-NonInteractive", "-Command", "Write-Output 'REMOTE_CLAUDE_COMMAND_OK'; [Environment]::MachineName; Get-Content -LiteralPath " + psQuote(name) + "; Add-Content -LiteralPath " + psQuote(name) + " -Value 'REMOTE_CLAUDE_EDIT_OK'"}}
	}
	var out, errout bytes.Buffer
	start := time.Now()
	r, e := bridgeExec(c, q, nil, &out, &errout)
	timings["local_command"] = []float64{float64(time.Since(start).Microseconds()) / 1000}
	if e != nil || r.Code != 0 {
		return fmt.Errorf("workstation command: %v (exit %d): %s", e, r.Code, errout.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("REMOTE_CLAUDE_FILE_OK")) || !bytes.Contains(out.Bytes(), []byte("REMOTE_CLAUDE_COMMAND_OK")) {
		return fmt.Errorf("workstation did not see the same project file: %s", out.String())
	}
	// A local edit bypasses SSHFS. Content reads must observe it, and metadata
	// must converge after the bounded cache expires (including file growth).
	if b, e := os.ReadFile(path); e != nil || !bytes.Contains(b, []byte("REMOTE_CLAUDE_EDIT_OK")) {
		return fmt.Errorf("local edit was not visible through the mount: %v", e)
	}
	if !c.FreshMetadata {
		time.Sleep(2100 * time.Millisecond)
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	st, e := os.Stat(path)
	if e != nil || st.Size() != int64(len(b)) {
		return fmt.Errorf("file metadata did not update after local edit: %v", e)
	}
	if c.LocalSearch {
		if e := measure("workstation_search", 3, func() error {
			var stdout, stderr bytes.Buffer
			r, e := bridgeExec(c, ExecRequest{Kind: "search", Cwd: c.RemoteRoot, Args: []string{"--fixed-strings", "--files-with-matches", "REMOTE_CLAUDE_EDIT_OK", name}}, nil, &stdout, &stderr)
			if e != nil || r.Code != 0 || !bytes.Contains(stdout.Bytes(), []byte(name)) {
				return fmt.Errorf("workstation search: %v (exit %d): %s", e, r.Code, stderr.String())
			}
			return nil
		}); e != nil {
			return e
		}
	}
	_, limitsErr := os.Stat(filepath.Join(stateDir(), "resources.enabled"))
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "version": version, "local_os": c.OS, "local_project": c.LocalRoot, "server_user": os.Getenv("USER"), "command_output": out.String(), "checks": []string{"SSH host identity", "reverse tunnel", "SFTP read/write", "local shell", "shared file visibility", "local edit and metadata freshness"}, "timings_ms": timings, "metadata_cache": !c.FreshMetadata, "config_snapshot_cache": c.ConfigSnapshot && !c.FreshMetadata, "shared_executor_enabled": c.ExecSocket != "", "extra_configuration_snapshot": c.ExtraConfig && c.ConfigSnapshot && !c.FreshMetadata, "workstation_search_enabled": c.LocalSearch, "resource_limits_configured": limitsErr == nil, "claude_api_called": false})
}
