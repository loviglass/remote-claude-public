package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Diagnostics exercise the replica, the synchronisation barriers and the
// workstation executor exactly as Claude's tools do, without an API call.
func serverDoctor(id string) error {
	var c SessionConfig
	if e := readJSON(projectHostPaths(id).config, &c); e != nil {
		return e
	}
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
		return fmt.Errorf("write project replica: %w", e)
	}
	defer func() {
		// Removal propagates to the workstation with the next barrier.
		os.Remove(path)
		bridgeExec(c, ExecRequest{Kind: "shell", Cwd: c.RemoteRoot, Command: "true"}, nil, &bytes.Buffer{}, &bytes.Buffer{})
	}()
	if e := measure("file_read", 3, func() error {
		b, e := os.ReadFile(path)
		if e == nil && string(b) != "REMOTE_CLAUDE_FILE_OK\n" {
			e = fmt.Errorf("unexpected file contents")
		}
		return e
	}); e != nil {
		return fmt.Errorf("read project replica: %w", e)
	}
	if e := measure("directory_list", 3, func() error { _, e := os.ReadDir(c.RemoteRoot); return e }); e != nil {
		return e
	}
	// The command must see the replica edit (barrier before) and its own
	// edit must be in the replica when it returns (barrier after).
	q := ExecRequest{Kind: "shell", Cwd: c.RemoteRoot, Command: "printf 'REMOTE_CLAUDE_COMMAND_OK\\n'; hostname; cat " + shellQuote(name) + "; printf 'REMOTE_CLAUDE_EDIT_OK\\n' >> " + shellQuote(name)}
	if c.OS == "windows" || c.Shell == "powershell" {
		q = ExecRequest{Kind: "powershell", Cwd: c.RemoteRoot, Args: []string{"-NoProfile", "-NonInteractive", "-Command", "Write-Output 'REMOTE_CLAUDE_COMMAND_OK'; [Environment]::MachineName; Get-Content -LiteralPath " + psQuote(name) + "; Add-Content -LiteralPath " + psQuote(name) + " -Value 'REMOTE_CLAUDE_EDIT_OK'"}}
	}
	var out, errout bytes.Buffer
	if e := measure("local_command", 1, func() error {
		out.Reset()
		errout.Reset()
		r, e := bridgeExec(c, q, nil, &out, &errout)
		if e == nil && r.Code != 0 {
			e = fmt.Errorf("exit %d", r.Code)
		}
		return e
	}); e != nil {
		return fmt.Errorf("workstation command: %v: %s", e, errout.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("REMOTE_CLAUDE_FILE_OK")) || !bytes.Contains(out.Bytes(), []byte("REMOTE_CLAUDE_COMMAND_OK")) {
		return fmt.Errorf("the workstation did not receive the replica edit before the command: %s", out.String())
	}
	if b, e := os.ReadFile(path); e != nil || !bytes.Contains(b, []byte("REMOTE_CLAUDE_EDIT_OK")) {
		return fmt.Errorf("the command's edit was not in the replica when it finished: %v", e)
	}
	if e := measure("empty_command", 3, func() error {
		r, e := bridgeExec(c, ExecRequest{Kind: "shell", Cwd: c.RemoteRoot, Command: "true"}, nil, &bytes.Buffer{}, &bytes.Buffer{})
		if e == nil && r.Code != 0 {
			e = fmt.Errorf("exit %d", r.Code)
		}
		return e
	}); e != nil {
		return fmt.Errorf("workstation command: %w", e)
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
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "version": version, "local_os": c.OS, "local_project": c.LocalRoot, "server_user": os.Getenv("USER"), "command_output": out.String(), "checks": []string{"SSH host identity", "reverse tunnel", "replica read/write", "local shell", "replica to workstation before command", "workstation to replica after command"}, "timings_ms": timings, "replica": c.Replica, "workstation_search_enabled": c.LocalSearch, "resource_limits_configured": limitsErr == nil, "claude_api_called": false})
}
