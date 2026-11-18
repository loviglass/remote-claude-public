package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func sshClient(p Profile, proxyAddress string) (*ssh.Client, error) {
	host, config, e := sshConnectionConfig(p)
	if e != nil {
		return nil, e
	}
	return dialSSHWithDeadlineVia(host, config, proxyAddress)
}

// Parse the identity once so opening separate data and terminal connections
// does not ask for an encrypted key's passphrase twice.
func sshConnectionConfig(p Profile) (string, *ssh.ClientConfig, error) {
	var b []byte
	var e error
	if p.BootstrapKey != "" {
		b = []byte(p.BootstrapKey)
	} else {
		b, e = os.ReadFile(p.Identity)
		if e != nil {
			return "", nil, e
		}
	}
	signer, e := ssh.ParsePrivateKey(b)
	if _, ok := e.(*ssh.PassphraseMissingError); ok {
		fmt.Fprint(os.Stderr, "SSH key passphrase: ")
		pass, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", nil, err
		}
		signer, e = ssh.ParsePrivateKeyWithPassphrase(b, pass)
	}
	if e != nil {
		return "", nil, e
	}
	if p.Fingerprint == "" {
		return "", nil, fmt.Errorf("server fingerprint is required; use a trusted invitation")
	}
	host := profileAddress(p.Host)
	return host, &ssh.ClientConfig{User: p.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, Timeout: 15 * time.Second, HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
		if ssh.FingerprintSHA256(k) != p.Fingerprint {
			return fmt.Errorf("SSH host fingerprint mismatch")
		}
		return nil
	}, HostKeyAlgorithms: []string{ssh.KeyAlgoED25519}}, nil
}
func runSSH(c *ssh.Client, command string, input []byte) ([]byte, error) {
	s, e := c.NewSession()
	if e != nil {
		return nil, e
	}
	defer s.Close()
	s.Stdin = bytes.NewReader(input)
	var errbuf bytes.Buffer
	s.Stderr = &errbuf
	b, e := s.Output(command)
	if e != nil {
		return b, fmt.Errorf("%w: %s", e, strings.TrimSpace(errbuf.String()))
	}
	return b, nil
}
func setup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	name := fs.String("profile", "default", "profile name")
	proxy := addProxyFlags(fs)
	if e := fs.Parse(args); e != nil {
		return e
	}
	proxyAddress, e := proxy.address()
	if e != nil {
		return e
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: remote-claude setup [--profile NAME] [--proxy [--proxy-host HOST] [--proxy-port PORT]] invitation.json")
	}
	if !validName(*name) {
		return fmt.Errorf("invalid profile name")
	}
	var p Profile
	if e := readJSON(fs.Arg(0), &p); e != nil {
		return e
	}
	dir := filepath.Join(configDir(), *name)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	if _, e := os.Stat(filepath.Join(dir, "config.json")); e == nil {
		return fmt.Errorf("profile already exists: %s", dir)
	}
	identity := filepath.Join(dir, "id_ed25519")
	keyData, e := os.ReadFile(identity)
	if os.IsNotExist(e) {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		key, err := ssh.MarshalPrivateKey(private, "remote-claude "+*name)
		if err != nil {
			return err
		}
		keyData = pem.EncodeToMemory(key)
		if e = os.WriteFile(identity, keyData, 0600); e != nil {
			return e
		}
	} else if e != nil {
		return e
	}
	signer, e := ssh.ParsePrivateKey(keyData)
	if e != nil {
		return e
	}
	finalProfile := p
	finalProfile.Identity = identity
	finalProfile.BootstrapKey = ""
	// Recover after an acknowledgement was lost, reusing the authorized key.
	if previous, e := sshClient(finalProfile, proxyAddress); e == nil {
		previous.Close()
		if e = writeJSON(filepath.Join(dir, "config.json"), finalProfile); e != nil {
			return e
		}
		fmt.Println("Recovered enrollment for", p.User+"@"+p.Host)
		return nil
	}
	pub := ssh.MarshalAuthorizedKey(signer.PublicKey())
	client, e := sshClient(p, proxyAddress)
	if e != nil {
		return e
	}
	defer client.Close()
	b, e := runSSH(client, serverBinary+" enroll", pub)
	if e != nil {
		return e
	}
	var ack struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(b, &ack) != nil || !ack.OK {
		return fmt.Errorf("enrollment was not confirmed")
	}
	if e = writeJSON(filepath.Join(dir, "config.json"), finalProfile); e != nil {
		return e
	}
	fmt.Println("Configured", *name, "for", p.User+"@"+p.Host)
	fmt.Println("Open a project directory and run: remote-claude .")
	return nil
}
func connect(args []string) (result error) {
	trace := startupTrace()
	fs := flag.NewFlagSet("remote-claude", flag.ContinueOnError)
	profile := fs.String("profile", "default", "profile name")
	var resume resumeFlag
	fs.Var(&resume, "resume", "choose a previous conversation (picker); --resume=ID resumes that one")
	continueLatest := fs.Bool("continue", false, "continue the most recent conversation without asking")
	prompt := fs.String("print", "", "non-interactive prompt")
	model := fs.String("model", "", "model alias")
	allowed := fs.String("allowed-tools", "", "tools approved for a non-interactive run")
	jsonOut := fs.Bool("json", false, "JSON output for a non-interactive run")
	doctor := fs.Bool("check", false, "verify SSH, file transport and local commands without calling Claude")
	freshMetadata := fs.Bool("fresh-metadata", false, "disable the short file metadata cache (slower on high-latency links)")
	serverSearch := fs.Bool("server-search", false, "use the compatibility search over SSHFS instead of workstation ripgrep")
	shell := fs.String("shell", "auto", "auto, bash, or powershell")
	forceNew := fs.Bool("force-new", false, "with --server: start a job even if another account already runs this project")
	server := fs.Bool("server", false, "run the task entirely on the VPS from the pushed commit; the workstation may disconnect")
	proxy := addProxyFlags(fs)
	if e := fs.Parse(args); e != nil {
		return e
	}
	proxyAddress, e := proxy.address()
	if e != nil {
		return e
	}
	root := "."
	if fs.NArg() > 0 {
		root = fs.Arg(0)
	}
	root, e = filepath.Abs(root)
	if e != nil {
		return e
	}
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		return e
	}
	if !validName(*profile) {
		return fmt.Errorf("invalid profile")
	}
	p, e := loadProfile(*profile)
	if e != nil {
		return fmt.Errorf("run setup first: %w", e)
	}
	interactive := *prompt == "" && !*doctor && !*jsonOut
	afterDrop := !*server && interactive && os.Getenv(sessionWorkerEnv) == "1" && os.Getenv(ordinaryContinueEnv) == "1"
	claudeArgs, e := sessionArgs(resume, *continueLatest, afterDrop)
	if e != nil {
		return e
	}
	if *prompt != "" {
		claudeArgs = append(claudeArgs, "--print", *prompt)
	}
	if *model != "" {
		claudeArgs = append(claudeArgs, "--model", *model)
	}
	if *allowed != "" {
		claudeArgs = append(claudeArgs, "--allowedTools", *allowed)
	}
	if *jsonOut {
		claudeArgs = append(claudeArgs, "--output-format", "json")
	}
	if fs.NArg() > 1 {
		extra := fs.Args()[1:]
		if extra[0] == "--" {
			extra = extra[1:]
		}
		claudeArgs = append(claudeArgs, extra...)
	}
	if *server {
		if *prompt != "" || *doctor {
			return fmt.Errorf("--server cannot be combined with --print or --check")
		}
		return serverMode(p, root, claudeArgs, resume.set() || *continueLatest, *forceNew, proxyAddress)
	}
	if *prompt == "" && !*doctor && !*jsonOut {
		if os.Getenv(sessionWorkerEnv) != "1" {
			return superviseInteractiveSession(false)
		}
		defer func() { result = markSessionRetry(result, true) }()
		go pruneIdleWorktrees(root, worktreeIdleLimit)
	}
	host, sshConfig, e := sshConnectionConfig(p)
	if e != nil {
		return e
	}
	trace("local configuration")
	// Handshakes are independent. Start the terminal connection while the
	// data connection prepares the reverse bridge and mounts the workspace.
	terminalDial := startSSHVia(host, sshConfig, proxyAddress)
	defer terminalDial.close()
	client, e := dialSSHWithDeadlineVia(host, sshConfig, proxyAddress)
	if e != nil {
		return e
	}
	defer client.Close()
	trace("data SSH connected")
	// The update check overlaps session preparation; an update restarts the
	// client before Claude starts, and the restarted client rebinds.
	updated := make(chan error, 1)
	go func() { updated <- installAvailableUpdate(client) }()
	hash := sha256.Sum256([]byte(p.Identity + "\x00" + root))
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	signalDone := make(chan struct{})
	defer close(signalDone)
	go func() {
		select {
		case <-signals:
			client.Close()
			terminalDial.cancel()
		case <-signalDone:
		}
	}()
	id := hex.EncodeToString(hash[:12])
	locationCache := filepath.Join(configDir(), *profile, "server-state.json")
	location, e := projectLocation(client, locationCache, id, false)
	if e != nil {
		return e
	}
	trace("workspace location")
	signer, e := newHostSigner()
	if e != nil {
		return e
	}
	cfg := SessionConfig{Protocol: protocolVersion, Secret: randomID(), HostKey: string(ssh.MarshalAuthorizedKey(signer.PublicKey())), LocalRoot: root, RemoteRoot: location, OS: runtime.GOOS, Shell: os.Getenv("SHELL"), ID: id, FreshMetadata: *freshMetadata, Replica: true}
	cfg.LocalSearch = !*serverSearch && workstationSearchAvailable()
	if *shell != "auto" && *shell != "bash" && *shell != "powershell" {
		return fmt.Errorf("shell must be auto, bash, or powershell")
	}
	if *shell != "auto" {
		cfg.Shell = *shell
	}
	r, e := os.OpenRoot(root)
	if e != nil {
		return e
	}
	defer r.Close()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return e
	}
	defer ln.Close()
	w := &worker{config: cfg, fs: &projectFS{root: r}, signer: signer, close: make(chan struct{}), ignores: loadIgnoreRules(root), synced: make(chan *syncPC, 1)}
	defer w.cleanup()
	httpProxies := &httpMCPProxies{listen: client.Listen, worker: w}
	defer httpProxies.close()
	w.fs.projectMCP = httpProxies.project
	go w.serve(ln)
	reverse, e := client.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return fmt.Errorf("SSH reverse forwarding: %w", e)
	}
	defer reverse.Close()
	trace("reverse forwarding")
	cfg.Endpoint = net.JoinHostPort("127.0.0.1", fmt.Sprint(portNumber(reverse.Addr())))
	go func() {
		for {
			a, e := reverse.Accept()
			if e != nil {
				return
			}
			go func() {
				b, e := net.Dial("tcp", ln.Addr().String())
				if e != nil {
					a.Close()
					return
				}
				copyConnections(a, b)
			}()
		}
	}()
	dataLost := make(chan struct{})
	go func() {
		client.Wait()
		close(dataLost)
	}()
	go keepConnectionAlive(client, dataLost)
	body, _ := json.Marshal(cfg)
	b, e := runSSH(client, serverBinary+" server prepare", body)
	if e != nil {
		if u := <-updated; u != nil {
			return u
		}
		if strings.Contains(e.Error(), "invalid protocol or workspace") {
			// A cached server location went stale; the next attempt asks.
			os.Remove(locationCache)
			return fmt.Errorf("%w: %w", e, net.ErrClosed)
		}
		return e
	}
	var ready struct {
		Session string `json:"session"`
		Running bool   `json:"running"`
	}
	if e = json.Unmarshal(b, &ready); e != nil {
		return e
	}
	trace("session host bound")
	if e := <-updated; e != nil {
		return e
	}
	if e := waitInitialSync(w, dataLost, interactive); e != nil {
		return e
	}
	trace("project synchronised")
	terminalClient, e := terminalDial.wait()
	if e != nil {
		return e
	}
	defer terminalClient.Close()
	trace("terminal SSH connected")
	go func() {
		<-dataLost
		terminalClient.Close()
	}()
	if *doctor {
		var roundTrips []float64
		for i := 0; i < 3; i++ {
			start := time.Now()
			if _, _, e := terminalClient.SendRequest("keepalive@openssh.com", true, nil); e != nil {
				return e
			}
			roundTrips = append(roundTrips, float64(time.Since(start).Microseconds())/1000)
		}
		b, e := runSSH(terminalClient, serverBinary+" server doctor "+ready.Session, nil)
		if e == nil {
			var report map[string]any
			if json.Unmarshal(b, &report) == nil {
				report["ssh_round_trip_ms"] = roundTrips
				report["terminal_transport"] = "separate_ssh_connection"
				return json.NewEncoder(os.Stdout).Encode(report)
			}
		}
		os.Stdout.Write(b)
		return e
	}
	params := encode(attachParams{Env: terminalEnvironment(), Args: claudeArgs})
	if !interactive {
		return runRemoteTerminal(terminalClient, serverBinary+" server run "+ready.Session+" "+params, false)
	}
	if ready.Running {
		fmt.Fprintln(os.Stderr, "Reattaching:", p.User+"@"+p.Host, "| project:", root, "| commands:", runtime.GOOS)
	} else {
		fmt.Fprintln(os.Stderr, "Connected:", p.User+"@"+p.Host, "| project:", root, "| commands:", runtime.GOOS)
	}
	return runRemoteTerminal(terminalClient, serverBinary+" server attach "+ready.Session+" "+params, true)
}

// projectLocation finds the server path of this project's replica. The server
// state directory is cached per profile, saving a round trip on every start.
func projectLocation(client *ssh.Client, cache, id string, fresh bool) (string, error) {
	suffix := "/projects/" + id + "/worktree"
	var cached struct {
		StateDir string `json:"state_dir"`
	}
	if !fresh && readJSON(cache, &cached) == nil && strings.HasPrefix(cached.StateDir, "/") {
		return cached.StateDir + suffix, nil
	}
	b, e := runSSH(client, serverBinary+" server location "+id, nil)
	if e != nil {
		return "", e
	}
	var location struct {
		Root string `json:"root"`
	}
	if e = json.Unmarshal(b, &location); e != nil {
		return "", e
	}
	if strings.HasSuffix(location.Root, suffix) {
		cached.StateDir = strings.TrimSuffix(location.Root, suffix)
		writeJSON(cache, cached)
	}
	return location.Root, nil
}

// waitInitialSync blocks until the replica matches the project, showing
// progress when the first transfer of a project takes a while.
func waitInitialSync(w *worker, lost <-chan struct{}, show bool) error {
	var s *syncPC
	select {
	case s = <-w.synced:
	case <-lost:
		return fmt.Errorf("connection lost during synchronisation: %w", net.ErrClosed)
	case <-time.After(time.Minute):
		return fmt.Errorf("the server did not start project synchronisation")
	}
	start := time.Now()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	shown := false
	for {
		select {
		case <-s.ready:
			if shown {
				fmt.Fprintf(os.Stderr, "\rProject synchronised: %d files, %.1f MB in %.1fs.        \n", s.sentFiles.Load(), float64(s.sentBytes.Load())/1e6, time.Since(start).Seconds())
			}
			return nil
		case <-s.done:
			return fmt.Errorf("project synchronisation stopped: %w", net.ErrClosed)
		case <-lost:
			return fmt.Errorf("connection lost during synchronisation: %w", net.ErrClosed)
		case <-tick.C:
			if show && time.Since(start) > time.Second {
				shown = true
				fmt.Fprintf(os.Stderr, "\rSynchronising project: %d files, %.1f MB...", s.sentFiles.Load(), float64(s.sentBytes.Load())/1e6)
			}
		}
	}
}

// The workstation side of the terminal is reproduced for Claude, so colours,
// key handling and notifications match the local terminal.
func terminalEnvironment() map[string]string {
	env := map[string]string{}
	for _, k := range []string{"TERM", "COLORTERM", "TERM_PROGRAM", "TERM_PROGRAM_VERSION"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	if env["TERM"] == "" || env["TERM"] == "dumb" {
		env["TERM"] = "xterm-256color"
	}
	if runtime.GOOS == "windows" && env["COLORTERM"] == "" && os.Getenv("WT_SESSION") != "" {
		env["COLORTERM"] = "truecolor"
	}
	for _, k := range []string{"LC_ALL", "LANG"} {
		if v := os.Getenv(k); strings.Contains(strings.ToUpper(v), "UTF-8") || strings.Contains(strings.ToUpper(v), "UTF8") {
			env["LANG"] = v
			break
		}
	}
	if env["LANG"] == "" {
		env["LANG"] = "C.UTF-8"
	}
	return env
}

// keepConnectionAlive closes a connection whose peer stopped answering, so a
// silently dead link turns into a prompt reconnect.
func keepConnectionAlive(client *ssh.Client, stop <-chan struct{}) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		done := make(chan error, 1)
		go func() { _, _, e := client.SendRequest("keepalive@openssh.com", true, nil); done <- e }()
		select {
		case e := <-done:
			if e == nil {
				continue
			}
		case <-time.After(30 * time.Second):
		case <-stop:
			return
		}
		client.Close()
		return
	}
}

// Opt-in diagnostics contain only phase names and elapsed time, never session
// configuration, credentials, project contents or executed commands.
func startupTrace() func(string) {
	start := time.Now()
	if os.Getenv("REMOTE_CLAUDE_STARTUP_TRACE") != "1" {
		return func(string) {}
	}
	return func(phase string) {
		fmt.Fprintf(os.Stderr, "Startup: %.3fs %s\n", time.Since(start).Seconds(), phase)
	}
}

func runRemoteTerminal(client *ssh.Client, command string, interactive bool) error {
	s, e := client.NewSession()
	if e != nil {
		return e
	}
	defer s.Close()
	s.Stdin = os.Stdin
	s.Stdout = os.Stdout
	s.Stderr = os.Stderr
	terminal := interactive && term.IsTerminal(int(os.Stdin.Fd()))
	clean := false
	var aborted atomic.Bool
	if terminal {
		restoreOutput := enableTerminalOutput()
		defer restoreOutput()
		width, height, e := term.GetSize(int(os.Stdin.Fd()))
		if e != nil {
			width, height = 100, 30
		}
		if e = s.RequestPty(terminalEnvironment()["TERM"], height, width, ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 38400, ssh.TTY_OP_OSPEED: 38400}); e != nil {
			return e
		}
		state, e := term.MakeRaw(int(os.Stdin.Fd()))
		if e != nil {
			return e
		}
		defer term.Restore(int(os.Stdin.Fd()), state)
		defer func() { os.Stdout.WriteString(terminalReset(clean)) }()
		stall := &stallInterrupt{abort: func() { aborted.Store(true); client.Close() }}
		s.Stdin = stallInput{os.Stdin, stall}
		s.Stdout = stallOutput{os.Stdout, stall}
	}
	stop := make(chan struct{})
	defer close(stop)
	if terminal {
		// Resizes propagate at once on Unix; Windows consoles are polled.
		winch := resizeSignals()
		poll := time.NewTicker(250 * time.Millisecond)
		if winch != nil {
			poll.Stop()
		}
		go func() {
			defer poll.Stop()
			lastW, lastH := 0, 0
			for {
				select {
				case <-stop:
					return
				case <-winch:
				case <-poll.C:
				}
				a, b, e := term.GetSize(int(os.Stdin.Fd()))
				if e == nil && (a != lastW || b != lastH) {
					s.WindowChange(b, a)
					lastW, lastH = a, b
				}
			}
		}()
	}
	go keepConnectionAlive(client, stop)
	e = s.Run(command)
	clean = e == nil
	if aborted.Load() {
		return &exitError{130}
	}
	if x, ok := e.(*ssh.ExitError); ok {
		return &exitError{x.ExitStatus()}
	}
	return e
}
func validName(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
