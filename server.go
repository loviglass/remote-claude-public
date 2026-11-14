package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

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
	case "job-image":
		return serverJobImage(args[1:])
	case "job-container":
		return serverJobContainer(args[1:])
	case "job-run":
		return serverJobRun(args[1:])
	case "doctor":
		if len(args) != 2 || !validName(args[1]) {
			return fmt.Errorf("invalid session id")
		}
		return serverDoctor(args[1])
	case "attach":
		if len(args) != 2 || !validName(args[1]) {
			return fmt.Errorf("invalid session id")
		}
		return serverAttach(args[1])
	case "cleanup":
		if len(args) != 2 || !validName(args[1]) {
			return fmt.Errorf("invalid session id")
		}
		return serverCleanup(args[1])
	}
	return fmt.Errorf("unknown server command")
}
func serverPrepare() error {
	var c SessionConfig
	if e := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&c); e != nil {
		return e
	}
	if c.Protocol != protocolVersion || !validName(c.ID) || c.RemoteRoot != projectDir(c.ID) {
		return fmt.Errorf("invalid protocol or workspace")
	}
	host, _, e := strings.Cut(c.Endpoint, ":")
	if !e || host != "127.0.0.1" {
		return fmt.Errorf("bridge must listen on server loopback")
	}
	session := randomID()
	dir := filepath.Join(stateDir(), "sessions")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	if e := os.MkdirAll(filepath.Dir(c.RemoteRoot), 0700); e != nil {
		return e
	}
	lock := filepath.Join(filepath.Dir(c.RemoteRoot), "active")
	// A dropped workstation cannot resume an in-flight command. Reclaim only a
	// workspace whose previous authenticated bridge is no longer reachable.
	if old, e := os.ReadFile(lock); e == nil && validName(string(old)) {
		var previous SessionConfig
		if readJSON(filepath.Join(dir, string(old)+".json"), &previous) == nil {
			conn, e := dialBridge(previous)
			if e != nil {
				serverCleanup(string(old))
			} else {
				conn.Close()
			}
		}
	}
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("project already connected; close its other session first (%s)", lock)
	}
	f.WriteString(session)
	f.Close()
	p := filepath.Join(dir, session+".json")
	// Only the managed caching transport owns a persistent workstation SSH
	// connection. Clients without it and strict metadata mode keep direct RPCs.
	c.ExecSocket = ""
	if c.ConfigSnapshot && !c.FreshMetadata {
		c.ExecSocket = filepath.Join(dir, session+".ssh")
	}
	if e := writeJSON(p, c); e != nil {
		os.Remove(lock)
		return e
	}
	ok := false
	defer func() {
		if !ok {
			serverCleanup(session)
		}
	}()
	versionReady := make(chan struct{})
	var versionError error
	go func() {
		versionError = verifyClaudeVersion(claudeBinary, filepath.Join(stateDir(), "claude-version.json"))
		close(versionReady)
	}()
	defer func() { <-versionReady }()
	// Reclaim a disconnected mount before touching it. Even stat/MkdirAll on
	// a dead FUSE mount can fail with EIO, preventing a normal reconnect.
	if e := os.MkdirAll(c.RemoteRoot, 0700); e != nil {
		return e
	}
	// Cache only metadata briefly. Cache kernel path lookups too: without it,
	// stat(a/b/c) makes a network round trip for each ancestor. SSHFS rounds its
	// cache clock to seconds, so use a zero stat/link TTL plus a one-second
	// kernel TTL; directory listings have a one-second SSHFS TTL. Content reads
	// remain uncached. The combined metadata age is approximately two seconds.
	cache := "dir_cache=yes,dcache_timeout=1,dcache_stat_timeout=0,dcache_link_timeout=0,attr_timeout=1,entry_timeout=1"
	if c.FreshMetadata {
		cache = "dir_cache=no,attr_timeout=0,entry_timeout=0"
	}
	cmd := exec.Command("sshfs", "bridge:/", c.RemoteRoot, "-o", "ssh_command="+serverBinary+" sftp,"+cache+",direct_io,negative_timeout=0,uid="+fmt.Sprint(os.Getuid())+",gid="+fmt.Sprint(os.Getgid()))
	cmd.Env = append(os.Environ(), "REMOTE_CLAUDE_SESSION="+p)
	if b, e := cmd.CombinedOutput(); e != nil {
		return fmt.Errorf("mount project: %v: %s", e, b)
	}
	if _, e := os.Stat(c.RemoteRoot); e != nil {
		return e
	}
	<-versionReady
	if versionError != nil {
		return versionError
	}
	ok = true
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"session": session})
}
func serverAttach(id string) error {
	path := filepath.Join(stateDir(), "sessions", id+".json")
	var c SessionConfig
	if e := readJSON(path, &c); e != nil {
		return e
	}
	defer serverCleanup(id)
	if e := verifyClaudeVersion(claudeBinary, filepath.Join(stateDir(), "claude-version.json")); e != nil {
		return e
	}
	args := []string{"--setting-sources", "project,local", "--append-system-prompt", fmt.Sprintf("This session uses remote-claude. Claude runs on Linux, but project commands execute on the developer's %s computer. Local project path: %s. File tools use the mounted project at %s. Use project-relative paths in commands whenever possible. Do not install project dependencies on the VPS. The bridge executes project hooks and stdio MCP servers on the developer computer. Server home configuration is not the developer's personal configuration.", c.OS, c.LocalRoot, c.RemoteRoot)}
	args = append(args, c.Args...)
	cmd := exec.Command(claudeBinary, args...)
	cmd.Dir = c.RemoteRoot
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// The blanket nonessential-traffic switch also blocks the interactive
	// /usage request in the pinned Claude version. Disable only background
	// updates and reporting so /usage can fetch account limits.
	cmd.Env = append(os.Environ(), "REMOTE_CLAUDE_SESSION="+path, "DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1")
	cmd.Env = append(cmd.Env, "CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS=3", "CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH=1")
	// The Bash edit diff puts an inotify watch on every project directory from
	// Claude's main thread. On the mount each watch is a synchronous round trip
	// to the workstation, so large trees (node_modules, worktrees) freeze the
	// session for minutes, and commands run on the workstation where VPS
	// inotify never sees their writes anyway.
	cmd.Env = append(cmd.Env, "CLAUDE_CODE_BASH_EDIT_DIFF=0")
	if c.LocalSearch {
		if _, err := os.Stat(serverRipgrep); err == nil {
			cmd.Env = append(cmd.Env, "USE_BUILTIN_RIPGREP=0")
		}
	}
	gitDir, e := gitShimDir()
	if e != nil {
		return e
	}
	cmd.Env = append(cmd.Env, "CLAUDE_CODE_SHELL_PREFIX="+filepath.Join(gitDir, "shell"))
	commandPath := gitDir + ":" + os.Getenv("PATH")
	if !c.ConfigSnapshot || c.FreshMetadata {
		// Ripgrep's discovery deadline can silently omit configuration files on
		// an uncached network mount. The slower Node walker is complete.
		cmd.Env = append(cmd.Env, "CLAUDE_CODE_USE_NATIVE_FILE_SEARCH=1")
	}
	if c.OS == "windows" || c.Shell == "powershell" {
		cmd.Env = append(cmd.Env, "CLAUDE_CODE_USE_POWERSHELL_TOOL=1")
		commandPath = "/opt/remote-claude/windows:" + commandPath
	}
	cmd.Env = append(cmd.Env, "PATH="+commandPath)
	stopScope, e := constrainClaude(cmd, id)
	if e != nil {
		return e
	}
	defer stopScope()
	if e = cmd.Start(); e != nil {
		return e
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	var bridgeLost atomic.Bool
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				conn, e := dialBridge(c)
				if e != nil {
					bridgeLost.Store(true)
					cmd.Process.Kill()
					// A newer session may already have mounted the same path.
					if owner, _ := os.ReadFile(filepath.Join(filepath.Dir(c.RemoteRoot), "active")); string(owner) == id {
						abortProjectMount(c.RemoteRoot)
					}
					return
				}
				conn.Close()
			}
		}
	}()
	e = <-finished
	if bridgeLost.Load() {
		return &exitError{bridgeLostExitCode}
	}
	if e != nil {
		if x, ok := e.(*exec.ExitError); ok {
			return &exitError{x.ExitCode()}
		}
		return e
	}
	return nil
}
func serverCleanup(id string) error {
	p := filepath.Join(stateDir(), "sessions", id+".json")
	var c SessionConfig
	if e := readJSON(p, &c); e != nil {
		if os.IsNotExist(e) {
			return nil
		}
		return e
	}
	if !validName(c.ID) || c.RemoteRoot != projectDir(c.ID) {
		return fmt.Errorf("invalid workspace in cleanup")
	}
	lock := filepath.Join(filepath.Dir(c.RemoteRoot), "active")
	owner, _ := os.ReadFile(lock)
	if string(owner) != id {
		return fmt.Errorf("workspace belongs to another session")
	}
	abortProjectMount(c.RemoteRoot)
	exec.Command("fusermount3", "-uz", c.RemoteRoot).Run()
	if c.ExecSocket == filepath.Join(stateDir(), "sessions", id+".ssh") {
		os.Remove(c.ExecSocket)
		os.Remove(c.ExecSocket + ".pub")
	}
	os.Remove(lock)
	return os.Remove(p)
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
