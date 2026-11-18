package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/crypto/ssh"
)

func stateDir() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".local", "share", "remote-claude")
}
func projectDir(id string) string { return filepath.Join(stateDir(), "projects", id, "worktree") }
func serverRun(args []string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("server runs on Linux")
	}
	if len(args) == 0 {
		return fmt.Errorf("server subcommand required")
	}
	switch args[0] {
	case "location":
		if len(args) != 2 || !validName(args[1]) {
			return fmt.Errorf("invalid project id")
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"root": projectDir(args[1])})
	case "prepare":
		return serverPrepare()
	case "night":
		return serverNight(args[1:])
	case "job":
		return serverJob(args[1:])
	case "job-list":
		return serverJobList(args[1:])
	case "job-upload":
		return serverJobUpload(args[1:])
	case "job-image":
		return serverJobImage(args[1:])
	case "job-container":
		return serverJobContainer(args[1:])
	case "job-run":
		return serverJobRun(args[1:])
	case "host":
		if len(args) != 2 {
			return fmt.Errorf("invalid project id")
		}
		return serverHost(args[1])
	case "doctor":
		if len(args) != 2 || !validName(args[1]) {
			return fmt.Errorf("invalid session id")
		}
		return serverDoctor(args[1])
	case "attach", "run":
		if len(args) < 2 || len(args) > 3 || !validName(args[1]) {
			return fmt.Errorf("invalid session id")
		}
		encoded := ""
		if len(args) == 3 {
			encoded = args[2]
		}
		if args[0] == "run" {
			return serverRunHost(args[1], encoded)
		}
		return serverAttachHost(args[1], encoded)
	case "cleanup":
		// Hosts outlive connections; kept so older clients exit cleanly.
		return nil
	}
	return fmt.Errorf("unknown server command")
}
func serverPrepare() error {
	var c SessionConfig
	if e := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&c); e != nil {
		return e
	}
	if c.Protocol != protocolVersion || !validName(c.ID) || c.RemoteRoot != projectDir(c.ID) {
		return fmt.Errorf("invalid protocol or workspace; update the remote-claude client")
	}
	host, _, ok := strings.Cut(c.Endpoint, ":")
	if !ok || host != "127.0.0.1" {
		return fmt.Errorf("bridge must listen on server loopback")
	}
	paths := projectHostPaths(c.ID)
	if e := os.MkdirAll(paths.dir, 0700); e != nil {
		return e
	}
	c.Replica, c.LiveRoot = true, paths.live
	if e := writeJSON(paths.bridge, c); e != nil {
		return e
	}
	if e := ensureHost(c.ID); e != nil {
		return e
	}
	rep, e := hostCall(c.ID, hostRequest{Op: "bind"})
	if e != nil {
		return e
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"session": c.ID, "running": rep.Running})
}

func enrollServer() error {
	b, e := io.ReadAll(io.LimitReader(os.Stdin, 16384))
	if e != nil {
		return e
	}
	key, _, options, rest, e := ssh.ParseAuthorizedKey(b)
	if e != nil || len(options) > 0 || len(strings.TrimSpace(string(rest))) > 0 || key.Type() != ssh.KeyAlgoED25519 {
		return fmt.Errorf("one plain ed25519 public key required")
	}
	h, e := os.UserHomeDir()
	if e != nil {
		return e
	}
	marker := filepath.Join(h, ".ssh", "enrollment.pending")
	lock, e := os.OpenFile(marker+".lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return fmt.Errorf("enrollment is already in progress or finished")
	}
	lock.Close()
	defer os.Remove(marker + ".lock")
	if _, e = os.Stat(marker); e != nil {
		return fmt.Errorf("invitation has already been used")
	}
	tmp := filepath.Join(h, ".ssh", "authorized_keys.new")
	data := append([]byte("no-agent-forwarding,no-X11-forwarding "), ssh.MarshalAuthorizedKey(key)...)
	if e = os.WriteFile(tmp, data, 0600); e != nil {
		return e
	}
	if e = os.Rename(tmp, filepath.Join(h, ".ssh", "authorized_keys")); e != nil {
		return e
	}
	if e = os.Remove(marker); e != nil {
		return e
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]bool{"ok": true})
}
