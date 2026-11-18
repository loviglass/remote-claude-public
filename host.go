//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

type sessionHost struct {
	id    string
	paths hostPaths
	sync  *syncServer
	exec  *localExecutor

	mu         sync.Mutex
	cond       *sync.Cond
	bridge     *ssh.Client
	bridgeCfg  SessionConfig
	live       *exec.Cmd
	claude     *exec.Cmd
	ptmx       *os.File
	claudeDone chan struct{}
	exitCode   int
	attached   net.Conn
	lastActive time.Time
	stopping   bool

	// Output goes to whichever terminal is attached; without one it is
	// dropped, and the next attach makes Claude redraw.
	outMu sync.Mutex
	out   *lockedWriter
}

func serverHost(id string) error {
	if !validName(id) {
		return fmt.Errorf("invalid project id")
	}
	paths := projectHostPaths(id)
	for _, d := range []string{paths.dir, filepath.Dir(paths.sock)} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return err
		}
	}
	lock, err := os.OpenFile(paths.lock, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another host owns this project")
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("host %s starting (version %s)", id, version)
	// Earlier versions mounted the project itself at the replica path.
	abortProjectMount(paths.replica)
	exec.Command("fusermount3", "-uz", paths.replica).Run()
	var cfg SessionConfig
	if err := readJSON(paths.bridge, &cfg); err != nil {
		return err
	}
	srv, err := openSyncServer(paths.replica, paths.live, cfg.LocalRoot, paths.state, paths.stash)
	if err != nil {
		return err
	}
	h := &sessionHost{id: id, paths: paths, sync: srv, lastActive: time.Now()}
	h.cond = sync.NewCond(&h.mu)
	if err := h.writeConfig(cfg); err != nil {
		return err
	}
	os.Remove(paths.exec)
	h.exec, err = startHostExecutor(h)
	if err != nil {
		return err
	}
	os.Remove(paths.sock)
	ln, err := net.Listen("unix", paths.sock)
	if err != nil {
		return err
	}
	os.Chmod(paths.sock, 0600)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		<-signals
		h.shutdown()
		ln.Close()
	}()
	go h.watchLifetime(ln)
	for {
		c, err := ln.Accept()
		if err != nil {
			break
		}
		go h.handle(c)
	}
	h.shutdown()
	log.Printf("host %s stopped", id)
	return nil
}

// writeConfig stores what command adapters need. It is stable across
// reconnects, so a running Claude keeps working with a new workstation link.
func (h *sessionHost) writeConfig(bridge SessionConfig) error {
	c := SessionConfig{
		Protocol: protocolVersion, LocalRoot: bridge.LocalRoot, RemoteRoot: h.paths.replica, OS: bridge.OS,
		Shell: bridge.Shell, ID: h.id, LocalSearch: bridge.LocalSearch, ExecSocket: h.paths.exec,
		Secret: hostExecSecret(h.paths), Replica: true, LiveRoot: h.paths.live,
	}
	return writeJSON(h.paths.config, c)
}

func hostExecSecret(p hostPaths) string {
	var c SessionConfig
	if readJSON(p.config, &c) == nil && c.Secret != "" {
		return c.Secret
	}
	return randomID()
}

func (h *sessionHost) handle(c net.Conn) {
	r := bufio.NewReader(c)
	line, err := r.ReadBytes('\n')
	if err != nil {
		c.Close()
		return
	}
	var req hostRequest
	if json.Unmarshal(line, &req) != nil {
		c.Close()
		return
	}
	reply := func(rep hostReply) { b, _ := json.Marshal(rep); c.Write(append(b, '\n')) }
	switch req.Op {
	case "bind":
		defer c.Close()
		if err := h.bind(); err != nil {
			reply(hostReply{Error: err.Error()})
			return
		}
		reply(hostReply{OK: true, Running: h.claudeRunning()})
	case "attach":
		h.attach(c, r, req)
	case "status":
		defer c.Close()
		reply(hostReply{OK: true, Running: h.claudeRunning()})
	default:
		c.Close()
	}
}

func (h *sessionHost) claudeRunning() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.claude != nil && h.claudeDone != nil && !closed(h.claudeDone)
}

func closed(c chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// bind connects to the workstation that just prepared this project.
func (h *sessionHost) bind() error {
	var cfg SessionConfig
	if err := readJSON(h.paths.bridge, &cfg); err != nil {
		return err
	}
	client, err := dialBridge(cfg)
	if err != nil {
		return fmt.Errorf("connect to workstation bridge: %w", err)
	}
	ch, reqs, err := client.OpenChannel("rc-sync", nil)
	if err != nil {
		client.Close()
		return fmt.Errorf("workstation does not support synchronisation: %w", err)
	}
	go ssh.DiscardRequests(reqs)
	h.mu.Lock()
	old := h.bridge
	h.bridge, h.bridgeCfg = client, cfg
	h.lastActive = time.Now()
	h.cond.Broadcast()
	h.mu.Unlock()
	if old != nil {
		old.Close()
	}
	h.writeConfig(cfg)
	_, finished := h.sync.serve(ch)
	go func() {
		err := <-finished
		log.Printf("synchronisation ended: %v", err)
		client.Close()
	}()
	go h.keepalive(client)
	go func() {
		client.Wait()
		h.mu.Lock()
		if h.bridge == client {
			h.bridge = nil
			h.lastActive = time.Now()
		}
		h.mu.Unlock()
		// Fail reads of ignored paths quickly instead of hanging Claude.
		abortProjectMount(h.paths.live)
	}()
	go h.mountLive(cfg)
	return nil
}

// The reverse tunnel can die silently; a missed keepalive ends the bridge so
// commands wait for the next connection instead of hanging.
func (h *sessionHost) keepalive(client *ssh.Client) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for range t.C {
		done := make(chan error, 1)
		go func() { _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				continue
			}
		case <-time.After(20 * time.Second):
		}
		client.Close()
		return
	}
}

// mountLive shows paths that stay on the workstation (ignored files, .git and
// large files) under the live directory the replica's symlinks point into.
func (h *sessionHost) mountLive(cfg SessionConfig) {
	abortProjectMount(h.paths.live)
	exec.Command("fusermount3", "-uz", h.paths.live).Run()
	if err := os.MkdirAll(h.paths.live, 0700); err != nil {
		log.Printf("live view: %v", err)
		return
	}
	// The caching transport answers .git metadata from a snapshot and reads
	// small files in one request; it runs no executor of its own.
	lc := cfg
	lc.RemoteRoot, lc.ExecSocket = h.paths.live, ""
	lc.ConfigSnapshot, lc.GitSnapshot, lc.BulkRead = true, true, true
	if err := writeJSON(h.paths.liveConfig, lc); err != nil {
		log.Printf("live view: %v", err)
		return
	}
	cache := "dir_cache=yes,dcache_timeout=1,dcache_stat_timeout=0,dcache_link_timeout=0,attr_timeout=1,entry_timeout=1"
	cmd := exec.Command("sshfs", "bridge:/", h.paths.live, "-o", "ssh_command="+serverBinary+" sftp,"+cache+",direct_io,negative_timeout=0,uid="+fmt.Sprint(os.Getuid())+",gid="+fmt.Sprint(os.Getgid()))
	cmd.Env = append(os.Environ(), "REMOTE_CLAUDE_SESSION="+h.paths.liveConfig)
	if b, err := cmd.CombinedOutput(); err != nil {
		log.Printf("live view: %v: %s", err, b)
	}
}

// currentBridge waits up to timeout for a workstation connection.
func (h *sessionHost) currentBridge(timeout time.Duration) (*ssh.Client, error) {
	deadline := time.Now().Add(timeout)
	t := time.AfterFunc(timeout, func() { h.mu.Lock(); h.cond.Broadcast(); h.mu.Unlock() })
	defer t.Stop()
	h.mu.Lock()
	defer h.mu.Unlock()
	for h.bridge == nil && !h.stopping && time.Now().Before(deadline) {
		h.cond.Wait()
	}
	if h.bridge == nil {
		return nil, fmt.Errorf("the workstation is offline; reconnect with remote-claude to continue running commands")
	}
	return h.bridge, nil
}

// Terminal frames between attach and host: type byte, uint32 length, payload.
const (
	frameData   = 'd'
	frameResize = 'r'
	frameExit   = 'x'
	frameNote   = 'm'
)

func writeFrame(w io.Writer, kind byte, payload []byte) error {
	head := make([]byte, 5, 5+len(payload))
	head[0] = kind
	binary.BigEndian.PutUint32(head[1:], uint32(len(payload)))
	_, err := w.Write(append(head, payload...))
	return err
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n > 1<<20 {
		return 0, nil, fmt.Errorf("terminal frame too large")
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return head[0], b, err
}

func resizePayload(rows, cols int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b, uint16(rows))
	binary.BigEndian.PutUint16(b[2:], uint16(cols))
	return b
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) frame(kind byte, payload []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return writeFrame(l.w, kind, payload)
}

func (h *sessionHost) attach(c net.Conn, r *bufio.Reader, req hostRequest) {
	defer c.Close()
	out := &lockedWriter{w: c}
	defer func() {
		h.mu.Lock()
		if h.attached == c {
			h.attached = nil
			h.lastActive = time.Now()
		}
		h.mu.Unlock()
	}()
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		out.frame(frameExit, exitPayload(1))
		return
	}
	previous := h.attached
	h.attached = c
	h.lastActive = time.Now()
	// A Claude that already exited (for example after /exit) is replaced by
	// a new one, as a fresh start would do.
	running := h.claude != nil && !closed(h.claudeDone)
	var err error
	if !running {
		err = h.startClaudeLocked(req)
	}
	ptmx, done := h.ptmx, h.claudeDone
	h.mu.Unlock()
	if previous != nil {
		previous.Close()
	}
	if err != nil {
		out.frame(frameNote, []byte("remote-claude: start Claude: "+err.Error()+"\r\n"))
		out.frame(frameExit, exitPayload(1))
		return
	}
	if running {
		out.frame(frameNote, []byte("remote-claude: reattached to the running Claude session\r\n"))
		// A size change makes Claude redraw for the new terminal.
		if req.Rows > 0 && req.Cols > 1 {
			pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(req.Rows), Cols: uint16(req.Cols - 1)})
			time.Sleep(50 * time.Millisecond)
		}
	}
	// A newer attach may have taken over while this one was redrawing.
	h.mu.Lock()
	if h.attached != c {
		h.mu.Unlock()
		return
	}
	if req.Rows > 0 && req.Cols > 0 {
		pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(req.Rows), Cols: uint16(req.Cols)})
	}
	h.setOutput(out)
	h.mu.Unlock()
	defer h.clearOutput(out)
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		for {
			kind, payload, err := readFrame(r)
			if err != nil {
				return
			}
			switch kind {
			case frameData:
				ptmx.Write(payload)
			case frameResize:
				if len(payload) == 4 {
					pty.Setsize(ptmx, &pty.Winsize{Rows: binary.BigEndian.Uint16(payload), Cols: binary.BigEndian.Uint16(payload[2:])})
				}
			}
		}
	}()
	select {
	case <-done:
		h.finalSync()
		h.mu.Lock()
		code := h.exitCode
		h.mu.Unlock()
		out.frame(frameExit, exitPayload(code))
	case <-inputDone:
	}
}

func exitPayload(code int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(code))
	return b
}

func (h *sessionHost) setOutput(w *lockedWriter) {
	h.outMu.Lock()
	h.out = w
	h.outMu.Unlock()
}

func (h *sessionHost) clearOutput(w *lockedWriter) {
	h.outMu.Lock()
	if h.out == w {
		h.out = nil
	}
	h.outMu.Unlock()
}

func (h *sessionHost) pumpOutput(ptmx *os.File) {
	buf := make([]byte, 64<<10)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			h.outMu.Lock()
			w := h.out
			h.outMu.Unlock()
			if w != nil {
				w.frame(frameData, buf[:n])
			}
		}
		if err != nil {
			return
		}
	}
}

func (h *sessionHost) startClaudeLocked(req hostRequest) error {
	cmd, err := claudeCommand(h.paths.config, req.Args, req.Env)
	if err != nil {
		return err
	}
	stopScope, err := constrainClaude(cmd, h.id)
	if err != nil {
		return err
	}
	size := &pty.Winsize{Rows: 30, Cols: 100}
	if req.Rows > 0 && req.Cols > 0 {
		size = &pty.Winsize{Rows: uint16(req.Rows), Cols: uint16(req.Cols)}
	}
	ptmx, err := pty.StartWithSize(cmd, size)
	if err != nil {
		stopScope()
		return err
	}
	h.claude, h.ptmx, h.claudeDone = cmd, ptmx, make(chan struct{})
	done := h.claudeDone
	go h.pumpOutput(ptmx)
	go func() {
		err := cmd.Wait()
		code := 0
		if x, ok := err.(*exec.ExitError); ok {
			code = x.ExitCode()
		} else if err != nil {
			code = 1
		}
		stopScope()
		h.mu.Lock()
		h.exitCode = code
		h.lastActive = time.Now()
		h.mu.Unlock()
		close(done)
		log.Printf("Claude exited with %d", code)
	}()
	return nil
}

// claudeCommand prepares Claude for a host-managed project.
func claudeCommand(configPath string, extraArgs []string, termEnv map[string]string) (*exec.Cmd, error) {
	var c SessionConfig
	if err := readJSON(configPath, &c); err != nil {
		return nil, err
	}
	if err := verifyClaudeVersion(claudeBinary, filepath.Join(stateDir(), "claude-version.json")); err != nil {
		return nil, err
	}
	prompt := fmt.Sprintf("This session uses remote-claude. Claude runs on a Linux server; the project lives on the developer's %s computer at %s, and shell commands, Git, hooks and stdio MCP servers run there. File tools work on a continuously synchronised copy at %s: your edits reach the developer's computer before the next command runs, and command results are synchronised back before you see them. Paths ignored by Git (dependencies, build output, local secrets) and .git appear as symlinks to the developer's files and are read over the network. Use project-relative paths in commands whenever possible. Do not install project dependencies on the server. Server home configuration is not the developer's personal configuration.", c.OS, c.LocalRoot, c.RemoteRoot)
	args := append([]string{"--setting-sources", "project,local", "--append-system-prompt", prompt}, extraArgs...)
	cmd := exec.Command(claudeBinary, args...)
	cmd.Dir = c.RemoteRoot
	env := append(os.Environ(), "REMOTE_CLAUDE_SESSION="+configPath, "DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1",
		"CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS=3", "CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH=1")
	// Claude's Bash edit diff watches every project directory; Git-ignored
	// paths are symlinks into the network view, so keep it off.
	env = append(env, "CLAUDE_CODE_BASH_EDIT_DIFF=0")
	if _, err := os.Stat(serverRipgrep); err == nil {
		env = append(env, "USE_BUILTIN_RIPGREP=0")
	}
	for _, k := range []string{"TERM", "COLORTERM", "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "LANG", "LC_ALL"} {
		if v := termEnv[k]; v != "" && !strings.ContainsAny(v, "\x00\n") {
			env = append(env, k+"="+v)
		}
	}
	gitDir, err := gitShimDir()
	if err != nil {
		return nil, err
	}
	env = append(env, "CLAUDE_CODE_SHELL_PREFIX="+filepath.Join(gitDir, "shell"))
	commandPath := gitDir + ":" + os.Getenv("PATH")
	if c.OS == "windows" || c.Shell == "powershell" {
		env = append(env, "CLAUDE_CODE_USE_POWERSHELL_TOOL=1")
		commandPath = "/opt/remote-claude/windows:" + commandPath
	}
	cmd.Env = append(env, "PATH="+commandPath)
	return cmd, nil
}

// watchLifetime ends the host when nothing needs it any more.
func (h *sessionHost) watchLifetime(ln net.Listener) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		h.mu.Lock()
		idle := time.Since(h.lastActive)
		running := h.claude != nil && !closed(h.claudeDone)
		exited := h.claude != nil && closed(h.claudeDone)
		attached := h.attached != nil
		bridged := h.bridge != nil
		claude := h.claude
		h.mu.Unlock()
		switch {
		case exited && !attached:
			// Claude ended (for example /exit) and its terminal has seen it.
			h.finalSync()
			ln.Close()
			return
		case running && !attached && idle > detachedLimit:
			log.Printf("no terminal for %s; stopping Claude", detachedLimit)
			claude.Process.Signal(syscall.SIGHUP)
		case claude == nil && !bridged && idle > idleHostLimit:
			ln.Close()
			return
		}
	}
}

func (h *sessionHost) shutdown() {
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		return
	}
	h.stopping = true
	h.cond.Broadcast()
	bridge, claude := h.bridge, h.claude
	h.mu.Unlock()
	if claude != nil && claude.Process != nil {
		claude.Process.Signal(syscall.SIGHUP)
	}
	h.sync.close()
	if h.exec != nil {
		h.exec.close()
	}
	if bridge != nil {
		bridge.Close()
	}
	abortProjectMount(h.paths.live)
	exec.Command("fusermount3", "-uz", h.paths.live).Run()
	os.Remove(h.paths.sock)
}

// ensureHost returns a connection-ready host, starting one if needed.
func ensureHost(id string) error {
	paths := projectHostPaths(id)
	if c, err := net.DialTimeout("unix", paths.sock, time.Second); err == nil {
		c.Close()
		return nil
	}
	logFile, err := os.OpenFile(filepath.Join(paths.dir, "host.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(serverBinary, "server", "host", id)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	for i := 0; i < 100; i++ {
		if c, err := net.DialTimeout("unix", paths.sock, time.Second); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("session host did not start; see %s", filepath.Join(paths.dir, "host.log"))
}

func hostCall(id string, req hostRequest) (hostReply, error) {
	c, err := net.DialTimeout("unix", projectHostPaths(id).sock, 5*time.Second)
	if err != nil {
		return hostReply{}, err
	}
	defer c.Close()
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return hostReply{}, err
	}
	var rep hostReply
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return rep, err
	}
	if err := json.Unmarshal(line, &rep); err != nil {
		return rep, err
	}
	if rep.Error != "" {
		return rep, fmt.Errorf("%s", rep.Error)
	}
	return rep, nil
}

// serverAttachHost relays this SSH terminal to the host's Claude terminal.
func serverAttachHost(id, encoded string) error {
	var params attachParams
	if encoded != "" {
		if err := decode(encoded, &params); err != nil {
			return err
		}
	}
	c, err := net.DialTimeout("unix", projectHostPaths(id).sock, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	fd := int(os.Stdin.Fd())
	req := hostRequest{Op: "attach", Env: params.Env, Args: params.Args}
	if term.IsTerminal(fd) {
		if w, rows, err := term.GetSize(fd); err == nil {
			req.Rows, req.Cols = rows, w
		}
		state, err := term.MakeRaw(fd)
		if err == nil {
			defer term.Restore(fd, state)
		}
	}
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return err
	}
	in := &lockedWriter{w: c}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 && in.frame(frameData, buf[:n]) != nil {
				return
			}
			if err != nil {
				return
			}
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			if w, rows, err := term.GetSize(fd); err == nil {
				in.frame(frameResize, resizePayload(rows, w))
			}
		}
	}()
	r := bufio.NewReaderSize(c, 64<<10)
	for {
		kind, payload, err := readFrame(r)
		if err != nil {
			return &exitError{bridgeLostExitCode}
		}
		switch kind {
		case frameData:
			os.Stdout.Write(payload)
		case frameNote:
			os.Stderr.Write(payload)
		case frameExit:
			code := 0
			if len(payload) == 4 {
				code = int(binary.BigEndian.Uint32(payload))
			}
			if code != 0 {
				return &exitError{code}
			}
			return nil
		}
	}
}

// serverRunHost runs a non-interactive Claude (print and JSON modes) against
// the host's replica and executor.
func serverRunHost(id, encoded string) error {
	var params attachParams
	if err := decode(encoded, &params); err != nil {
		return err
	}
	cmd, err := claudeCommand(projectHostPaths(id).config, params.Args, params.Env)
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	stopScope, err := constrainClaude(cmd, id+"-print")
	if err != nil {
		return err
	}
	defer stopScope()
	err = cmd.Run()
	// Claude's last edits reach the workstation before the client leaves.
	var c SessionConfig
	if readJSON(projectHostPaths(id).config, &c) == nil {
		bridgeExec(c, ExecRequest{Kind: "sync", Cwd: c.RemoteRoot}, nil, io.Discard, io.Discard)
	}
	if x, ok := err.(*exec.ExitError); ok {
		return &exitError{x.ExitCode()}
	}
	return err
}

func startHostExecutor(h *sessionHost) (*localExecutor, error) {
	return newLocalExecutor(h.paths.exec, hostExecSecret(h.paths), func() (*ssh.Client, error) {
		return h.currentBridge(time.Minute)
	}, func(q *ExecRequest) {
		if q.Kind == "read-file" || q.Kind == "config-snapshot" {
			return
		}
		if seq, epoch, ok := h.sync.flush(15 * time.Second); ok {
			q.SyncSeq, q.SyncEpoch = seq, epoch
		}
		q.SyncAfter = syncAfter(q)
	}, func(q *ExecRequest, r *ExecResult, stderr io.Writer) {
		if r.SyncSeq > 0 && !h.sync.waitApplied(r.SyncSeq, r.SyncEpoch, 2*time.Minute) {
			fmt.Fprintln(stderr, "remote-claude: the command finished, but its file changes have not reached the server yet; re-read files before editing them.")
		}
		for _, n := range h.sync.takeNotices() {
			fmt.Fprintln(stderr, n)
		}
	})
}

// finalSync delivers Claude's last edits to the workstation before Claude's
// exit is reported, so nothing waits in the replica for a later session.
func (h *sessionHost) finalSync() {
	h.mu.Lock()
	bridge := h.bridge
	h.mu.Unlock()
	if bridge == nil {
		return
	}
	seq, epoch, ok := h.sync.flush(10 * time.Second)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bridgeExecClientContext(ctx, bridge, ExecRequest{Kind: "sync", SyncSeq: seq, SyncEpoch: epoch}, nil, io.Discard, io.Discard)
}
