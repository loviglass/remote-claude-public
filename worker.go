package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type worker struct {
	config   SessionConfig
	fs       *projectFS
	signer   ssh.Signer
	seen     recentIDs
	ignores  *ignoreRules
	syncMu   sync.Mutex
	sync     *syncPC
	synced   chan *syncPC
	close    chan struct{}
	wg       sync.WaitGroup
	envMu    sync.Mutex
	envFile  string
	activeMu sync.Mutex
	active   sync.WaitGroup
	closing  bool
}

func (w *worker) localEnvFile(create bool) (string, error) {
	w.envMu.Lock()
	defer w.envMu.Unlock()
	if w.envFile == "" && create {
		f, e := os.CreateTemp("", "remote-claude-env-*")
		if e != nil {
			return "", e
		}
		w.envFile = f.Name()
		f.Close()
	}
	return w.envFile, nil
}
func (w *worker) cleanup() {
	w.activeMu.Lock()
	w.closing = true
	close(w.close)
	w.activeMu.Unlock()
	done := make(chan struct{})
	go func() { w.active.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	if p, _ := w.localEnvFile(false); p != "" {
		os.Remove(p)
	}
}

func (w *worker) serve(ln net.Listener) {
	for {
		c, e := ln.Accept()
		if e != nil {
			return
		}
		w.wg.Add(1)
		go func() { defer w.wg.Done(); w.handle(c) }()
	}
}
func (w *worker) handle(c net.Conn) {
	cfg := &ssh.ServerConfig{PasswordCallback: func(m ssh.ConnMetadata, b []byte) (*ssh.Permissions, error) {
		if subtle.ConstantTimeCompare(b, []byte(w.config.Secret)) != 1 {
			return nil, fmt.Errorf("authentication failed")
		}
		return nil, nil
	}}
	cfg.AddHostKey(w.signer)
	c.SetDeadline(time.Now().Add(15 * time.Second))
	conn, chs, reqs, e := ssh.NewServerConn(c, cfg)
	if e != nil {
		c.Close()
		return
	}
	c.SetDeadline(time.Time{})
	defer conn.Close()
	ended := make(chan struct{})
	defer close(ended)
	go func() {
		select {
		case <-w.close:
			conn.Close()
		case <-ended:
		}
	}()
	go ssh.DiscardRequests(reqs)
	for ch := range chs {
		switch ch.ChannelType() {
		case "session":
			channel, requests, e := ch.Accept()
			if e == nil {
				go w.handleSession(channel, requests)
			}
		case execChannelType:
			var q ExecRequest
			if decode(string(ch.ExtraData()), &q) != nil || q.ID == "" || !w.seen.add(q.ID) {
				ch.Reject(ssh.Prohibited, "invalid or repeated request")
				continue
			}
			if !w.begin() {
				ch.Reject(ssh.Prohibited, "session is closing")
				continue
			}
			channel, requests, e := ch.Accept()
			if e != nil {
				w.active.Done()
				continue
			}
			go func() {
				defer w.active.Done()
				defer channel.Close()
				w.runChannel(channel, requests, q)
			}()
		case "rc-sync":
			if !w.config.Replica {
				ch.Reject(ssh.UnknownChannelType, "replica mode is off")
				continue
			}
			channel, requests, e := ch.Accept()
			if e != nil {
				continue
			}
			go ssh.DiscardRequests(requests)
			s := newSyncPC(w.fs.root, w.config.LocalRoot, w.config.RemoteRoot, w.ignores, func(p string, data []byte, unwrap bool) ([]byte, error) {
				return projectJSON(data, unwrap, w.fs.projectMCP)
			})
			w.syncMu.Lock()
			if w.sync != nil {
				w.sync.close()
			}
			w.sync = s
			w.syncMu.Unlock()
			if w.synced != nil {
				select {
				case w.synced <- s:
				default:
				}
			}
			go s.serve(channel)
		default:
			ch.Reject(ssh.UnknownChannelType, "unsupported channel")
		}
	}
}

func (w *worker) begin() bool {
	w.activeMu.Lock()
	defer w.activeMu.Unlock()
	if w.closing {
		return false
	}
	w.active.Add(1)
	return true
}

func (w *worker) currentSync() *syncPC {
	w.syncMu.Lock()
	defer w.syncMu.Unlock()
	return w.sync
}

func (w *worker) runChannel(ch ssh.Channel, requests <-chan *ssh.Request, q ExecRequest) {
	done := make(chan struct{})
	go func() {
		for r := range requests {
			if r.WantReply {
				r.Reply(false, nil)
			}
		}
		close(done)
	}()
	result := w.execute(q, ch, ch, ch.Stderr(), done)
	b, _ := json.Marshal(result)
	ch.SendRequest("remote-claude-result", false, b)
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{uint32(result.Code)}))
}

// recentIDs remembers request IDs long enough to reject a replayed request
// without growing for the whole session.
type recentIDs struct {
	mu   sync.Mutex
	cur  map[string]bool
	prev map[string]bool
}

func (r *recentIDs) add(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur[id] || r.prev[id] {
		return false
	}
	if r.cur == nil || len(r.cur) >= 4096 {
		r.prev, r.cur = r.cur, map[string]bool{}
	}
	r.cur[id] = true
	return true
}
func (w *worker) handleSession(ch ssh.Channel, requests <-chan *ssh.Request) {
	defer ch.Close()
	for r := range requests {
		switch r.Type {
		case "subsystem":
			var x struct{ Name string }
			if ssh.Unmarshal(r.Payload, &x) != nil || x.Name != "sftp" {
				r.Reply(false, nil)
				continue
			}
			r.Reply(true, nil)
			srv := sftp.NewRequestServer(ch, sftp.Handlers{FileGet: w.fs, FilePut: w.fs, FileCmd: w.fs, FileList: w.fs})
			srv.Serve()
			srv.Close()
			return
		case "exec":
			w.activeMu.Lock()
			if w.closing {
				w.activeMu.Unlock()
				r.Reply(false, nil)
				return
			}
			w.active.Add(1)
			w.activeMu.Unlock()
			defer w.active.Done()
			var x struct{ Command string }
			var q ExecRequest
			if ssh.Unmarshal(r.Payload, &x) != nil || decode(x.Command, &q) != nil || q.ID == "" {
				r.Reply(false, nil)
				return
			}
			if !w.seen.add(q.ID) {
				r.Reply(false, nil)
				return
			}
			r.Reply(true, nil)
			done := make(chan struct{})
			go func() {
				for r := range requests {
					if r.WantReply {
						r.Reply(false, nil)
					}
				}
				close(done)
			}()
			result := w.execute(q, ch, ch, ch.Stderr(), done)
			b, _ := json.Marshal(result)
			ch.SendRequest("remote-claude-result", false, b)
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{uint32(result.Code)}))
			return
		default:
			r.Reply(false, nil)
		}
	}
}
func (w *worker) localPath(p string) string {
	if p == "" {
		return w.config.LocalRoot
	}
	if p == w.config.RemoteRoot {
		return w.config.LocalRoot
	}
	if strings.HasPrefix(p, w.config.RemoteRoot+"/") {
		return filepath.Join(w.config.LocalRoot, filepath.FromSlash(strings.TrimPrefix(p, w.config.RemoteRoot+"/")))
	}
	return p
}
func (w *worker) env(q ExecRequest) map[string]string {
	e := map[string]string{}
	for _, s := range os.Environ() {
		k, v, ok := strings.Cut(s, "=")
		// The client's private supervisor state must not leak into commands:
		// a remote-claude started by a command would act as this session.
		if ok && !strings.HasPrefix(k, "_REMOTE_CLAUDE_") {
			e[k] = v
		}
	}
	for _, p := range []string{".claude/settings.json", ".claude/settings.local.json"} {
		var d map[string]any
		if readJSON(filepath.Join(w.config.LocalRoot, filepath.FromSlash(p)), &d) == nil {
			if env, ok := d["env"].(map[string]any); ok {
				for k, v := range env {
					if s, ok := v.(string); ok {
						e[k] = s
					}
				}
			}
		}
	}
	for k, v := range q.Env {
		e[k] = w.localPath(v)
	}
	e["CLAUDE_PROJECT_DIR"] = w.config.LocalRoot
	e["CLAUDECODE"] = "1"
	return e
}
func expandVars(s string, env map[string]string) string {
	return os.Expand(s, func(k string) string {
		if a, b, ok := strings.Cut(k, ":-"); ok {
			if v := env[a]; v != "" {
				return v
			}
			return b
		}
		return env[k]
	})
}
func (w *worker) execute(q ExecRequest, in io.Reader, out, errout io.Writer, disconnected <-chan struct{}) ExecResult {
	s := w.currentSync()
	if s != nil && q.SyncSeq > 0 && !s.waitApplied(q.SyncSeq, q.SyncEpoch, 30*time.Second) {
		fmt.Fprintln(errout, "remote-claude: Claude's latest file edits have not reached this computer yet; the command runs anyway.")
	}
	result := w.executeCommand(q, in, out, errout, disconnected)
	if s != nil && q.SyncAfter {
		if seq, epoch, ok := s.barrier(time.Minute); ok {
			result.SyncSeq, result.SyncEpoch = seq, epoch
		}
	}
	return result
}

func (w *worker) executeCommand(q ExecRequest, in io.Reader, out, errout io.Writer, disconnected <-chan struct{}) ExecResult {
	if q.Kind == "sync" {
		// Only the barriers: used when Claude finishes.
		return ExecResult{Code: 0}
	}
	if q.Kind == "read-file" {
		return w.readFileSnapshot(q, out)
	}
	if q.Kind == "config-snapshot" {
		if e := w.fs.sendConfigSnapshot(q, out); e != nil {
			return ExecResult{Code: 1, Error: e.Error()}
		}
		return ExecResult{Code: 0}
	}
	env := w.env(q)
	cwd := w.localPath(q.Cwd)
	result := ExecResult{Code: 1, Cwd: cwd}
	var cmd *exec.Cmd
	var cwdFile string
	var input = in
	if q.Kind == "shell" || q.Kind == "powershell" {
		input = nil
	}
	var buffer bytes.Buffer
	hook := q.Kind == "hook"
	if hook {
		if env["CLAUDE_ENV_FILE"] != "" {
			p, e := w.localEnvFile(true)
			if e != nil {
				result.Error = e.Error()
				return result
			}
			env["CLAUDE_ENV_FILE"] = p
		}
		b, e := io.ReadAll(io.LimitReader(in, 16<<20))
		if e != nil {
			result.Error = e.Error()
			return result
		}
		b = []byte(strings.ReplaceAll(string(b), w.config.RemoteRoot, filepath.ToSlash(w.config.LocalRoot)))
		input = bytes.NewReader(b)
		if q.Spec["type"] == "http" {
			u, _ := q.Spec["url"].(string)
			u = expandVars(u, env)
			req, e := http.NewRequest("POST", u, bytes.NewReader(b))
			if e != nil {
				result.Error = e.Error()
				return result
			}
			req.Header.Set("Content-Type", "application/json")
			allowed := map[string]string{}
			if a, ok := q.Spec["allowedEnvVars"].([]any); ok {
				for _, k := range a {
					if s, ok := k.(string); ok {
						allowed[s] = env[s]
					}
				}
			}
			if h, ok := q.Spec["headers"].(map[string]any); ok {
				for k, v := range h {
					if s, ok := v.(string); ok {
						req.Header.Set(k, expandVars(s, allowed))
					}
				}
			}
			resp, e := (&http.Client{Timeout: 60 * time.Second}).Do(req)
			if e != nil {
				result.Error = e.Error()
				return result
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				result.Error = fmt.Sprintf("HTTP hook returned %d", resp.StatusCode)
				return result
			}
			io.Copy(out, resp.Body)
			result.Code = 0
			return result
		}
	}
	if q.Kind == "mcp" || hook {
		command, _ := q.Spec["command"].(string)
		if p, ok := q.Spec["cwd"].(string); ok && p != "" {
			cwd = w.localPath(expandVars(p, env))
			if !filepath.IsAbs(cwd) {
				cwd = filepath.Join(w.config.LocalRoot, cwd)
			}
		}
		if m, ok := q.Spec["env"].(map[string]any); ok {
			for k, v := range m {
				if s, ok := v.(string); ok {
					env[k] = expandVars(s, env)
				}
			}
		}
		args, e := stringArgs(q.Spec["args"])
		if e != nil {
			result.Error = e.Error()
			return result
		}
		for i := range args {
			args[i] = expandVars(args[i], env)
		}
		if _, execForm := q.Spec["args"]; execForm || q.Kind == "mcp" {
			cmd = commandWithEnv(expandVars(command, env), args, env, cwd)
		} else {
			sh, _ := q.Spec["shell"].(string)
			cmd = localShell(command, sh)
		}
	} else if q.Kind == "powershell" {
		args := append([]string(nil), q.Args...)
		if q.Spec["track_cwd"] == true {
			f, e := os.CreateTemp("", "remote-claude-pwd-*")
			if e != nil {
				result.Error = e.Error()
				return result
			}
			cwdFile = f.Name()
			f.Close()
			defer os.Remove(cwdFile)
			env["REMOTE_CLAUDE_CWD_FILE"] = cwdFile
		}
		for i, a := range args {
			if strings.EqualFold(a, "-Command") && i+1 < len(args) {
				args[i+1] = powerShellUTF8 + translatePowerShellScript(args[i+1], w.config.RemoteRoot, filepath.ToSlash(w.config.LocalRoot))
			}
			if strings.EqualFold(a, "-EncodedCommand") && i+1 < len(args) {
				if raw, e := base64.StdEncoding.DecodeString(args[i+1]); e == nil && len(raw)%2 == 0 {
					u := make([]uint16, len(raw)/2)
					for j := range u {
						u[j] = binary.LittleEndian.Uint16(raw[j*2:])
					}
					u = utf16.Encode([]rune(powerShellUTF8 + string(utf16.Decode(u))))
					b := make([]byte, len(u)*2)
					for j, n := range u {
						binary.LittleEndian.PutUint16(b[j*2:], n)
					}
					args[i+1] = base64.StdEncoding.EncodeToString(b)
				}
			}
		}
		cmd = exec.Command(powerShellPath(), args...)
	} else if q.Kind == "search" {
		// Explicit operands must obey the same exported-root boundary as SFTP.
		// Recursive ripgrep does not follow symlinks; --follow stays on the VPS.
		if paths, ok := q.Spec["paths"].([]any); ok {
			for _, operand := range paths {
				p, ok := operand.(string)
				if !ok {
					result.Error = "invalid search path"
					return result
				}
				rel, err := filepath.Rel(w.config.LocalRoot, filepath.Join(cwd, filepath.FromSlash(p)))
				if err != nil {
					result.Error = err.Error()
					return result
				}
				st, err := w.fs.root.Stat(rel)
				if err != nil && !os.IsNotExist(err) {
					result.Error = err.Error()
					return result
				}
				if err == nil && !st.Mode().IsRegular() && !st.IsDir() {
					result.Error = "unsupported special file in search"
					return result
				}
			}
		}
		rg, err := workstationRipgrep()
		if err != nil {
			result.Error = err.Error()
			return result
		}
		// The caller's options are explicit. Personal ripgrep defaults must
		// not alter Claude's search, nor spawn preprocessors on the workstation.
		delete(env, "RIPGREP_CONFIG_PATH")
		cmd = exec.Command(rg, append([]string{"--no-config", "--path-separator", "/"}, q.Args...)...)
	} else if q.Kind == "exec" {
		cmd = commandWithEnv(q.Command, q.Args, env, cwd)
	} else {
		f, e := os.CreateTemp("", "remote-claude-cwd-*")
		if e != nil {
			result.Error = e.Error()
			return result
		}
		cwdFile = f.Name()
		f.Close()
		defer os.Remove(cwdFile)
		command := translateScript(q.Command, w.config.RemoteRoot, w.config.LocalRoot)
		if p, _ := w.localEnvFile(false); p != "" {
			command = ". " + shellQuote(filepath.ToSlash(p)) + "\n" + command
		}
		script := command + "\n_rc_status=$?\npwd -P > " + shellQuote(cwdFile) + "\nexit $_rc_status"
		cmd = localShell(script, "")
	}
	cmd.Dir = cwd
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// exec.Cmd waits for its own stdin copier even after WaitDelay, and that
	// copier blocks on the channel while Claude keeps the caller's stdin open.
	// Feeding a pipe ourselves lets Wait return as soon as the process exits.
	var stdinWriter *os.File
	if input != nil {
		pr, pw, e := os.Pipe()
		if e != nil {
			result.Error = e.Error()
			return result
		}
		cmd.Stdin, stdinWriter = pr, pw
		defer pr.Close()
		defer pw.Close()
	}
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdout = out
	cmd.Stderr = errout
	if hook {
		cmd.Stdout = &buffer
	}
	configureProcess(cmd)
	if e := cmd.Start(); e != nil {
		result.Error = e.Error()
		return result
	}
	if stdinWriter != nil {
		cmd.Stdin.(*os.File).Close()
		go func() { io.Copy(stdinWriter, input); stdinWriter.Close() }()
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	var e error
	select {
	case e = <-finished:
	case <-disconnected:
		stopProcess(cmd)
		select {
		case e = <-finished:
		case <-time.After(2 * time.Second):
			killProcess(cmd)
			e = <-finished
		}
	case <-w.close:
		killProcess(cmd)
		e = <-finished
	}
	if e == nil {
		result.Code = 0
	} else if x, ok := e.(*exec.ExitError); ok {
		result.Code = x.ExitCode()
		if result.Code < 0 {
			// Killed by a signal: report it the way a shell does.
			result.Code = 128 + exitSignal(x)
		}
	} else {
		result.Error = e.Error()
	}
	if cwdFile != "" {
		if b, e := os.ReadFile(cwdFile); e == nil && len(b) > 0 {
			result.Cwd = strings.TrimSpace(strings.TrimPrefix(string(b), "\uFEFF"))
		}
	}
	if hook {
		b := strings.ReplaceAll(buffer.String(), filepath.ToSlash(w.config.LocalRoot), w.config.RemoteRoot)
		if runtime.GOOS == "windows" {
			escaped, _ := json.Marshal(w.config.LocalRoot)
			b = strings.ReplaceAll(b, string(escaped[1:len(escaped)-1]), w.config.RemoteRoot)
		}
		io.WriteString(out, b)
	}
	return result
}
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

const powerShellUTF8 = "[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new(); [Console]::InputEncoding = [Console]::OutputEncoding; $OutputEncoding = [Console]::OutputEncoding; "

func translatePowerShellScript(s, remote, local string) string {
	if remote == "" || remote == local {
		return s
	}
	var b strings.Builder
	var quote byte
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], remote) {
			switch quote {
			case '\'':
				b.WriteString(strings.ReplaceAll(local, "'", "''"))
			case '"':
				b.WriteString(strings.NewReplacer("`", "``", "$", "`$", "\"", "`\"").Replace(local))
			default:
				end := i + len(remote)
				for end < len(s) && !strings.ContainsRune(" \t\r\n'\";|&(){}<>", rune(s[end])) {
					end++
				}
				b.WriteString(psQuote(local + s[i+len(remote):end]))
				i = end
				continue
			}
			i += len(remote)
			continue
		}
		c := s[i]
		i++
		b.WriteByte(c)
		if c == '`' && quote != '\'' && i < len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		if c == '\'' || c == '"' {
			if quote == 0 {
				quote = c
			} else if quote == c {
				quote = 0
			}
		}
	}
	return b.String()
}
func localShell(script, shell string) *exec.Cmd {
	if shell == "powershell" {
		return exec.Command(powerShellPath(), "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", powerShellUTF8+script)
	}
	if runtime.GOOS == "windows" {
		for _, dir := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs")} {
			p := filepath.Join(dir, "Git", "bin", "bash.exe")
			if _, e := os.Stat(p); e == nil {
				return exec.Command(p, "-c", script)
			}
		}
		// Never silently substitute PowerShell for Bash scripts or launch WSL.
		return exec.Command("remote-claude-requires-Git-Bash-for-Bash-hooks.exe", "-c", script)
	}
	p := os.Getenv("SHELL")
	if p == "" || (!strings.HasSuffix(p, "/bash") && !strings.HasSuffix(p, "/zsh")) {
		p = "/bin/bash"
	}
	return exec.Command(p, "-c", script)
}
func powerShellPath() string {
	if p, e := exec.LookPath("pwsh"); e == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	}
	return "pwsh"
}

// exec.Command normally resolves PATH before Cmd.Env is assigned. Resolve using
// the project/MCP environment so local virtual environments work as configured.
func commandWithEnv(command string, args []string, env map[string]string, cwd string) *exec.Cmd {
	if !strings.ContainsAny(command, "/\\") {
		pathValue := env["PATH"]
		if runtime.GOOS == "windows" {
			for k, v := range env {
				if strings.EqualFold(k, "PATH") {
					pathValue = v
				}
			}
		}
		exts := []string{""}
		if runtime.GOOS == "windows" && filepath.Ext(command) == "" {
			exts = strings.Split(".COM;.EXE;.BAT;.CMD", ";")
		}
		for _, d := range filepath.SplitList(pathValue) {
			if d == "" {
				continue
			}
			if !filepath.IsAbs(d) {
				d = filepath.Join(cwd, d)
			}
			for _, ext := range exts {
				p := filepath.Join(d, command+ext)
				if st, e := os.Stat(p); e == nil && !st.IsDir() && (runtime.GOOS == "windows" || st.Mode()&0111 != 0) {
					return exec.Command(p, args...)
				}
			}
		}
		cmd := exec.Command(command, args...)
		cmd.Err = fmt.Errorf("executable %q was not found in the workstation PATH", command)
		return cmd
	}
	return exec.Command(command, args...)
}

// Substitute only the workspace prefix, quoting spaces and apostrophes for the
// enclosing shell context. Relative commands pass through unchanged.
func translateScript(s, remote, local string) string {
	if remote == local || remote == "" {
		return s
	}
	if runtime.GOOS == "windows" {
		return strings.ReplaceAll(s, remote, filepath.ToSlash(local))
	}
	var b strings.Builder
	var quote byte
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], remote) {
			switch quote {
			case '\'':
				b.WriteString(strings.ReplaceAll(local, "'", "'\"'\"'"))
			case '"':
				r := strings.NewReplacer("\\", "\\\\", "$", "\\$", "`", "\\`", "\"", "\\\"")
				b.WriteString(r.Replace(local))
			default:
				b.WriteString(shellQuote(local))
			}
			i += len(remote)
			continue
		}
		c := s[i]
		b.WriteByte(c)
		i++
		if c == '\\' && quote != '\'' && i < len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		if c == '\'' || c == '"' {
			if quote == 0 {
				quote = c
			} else if quote == c {
				quote = 0
			}
		}
	}
	return b.String()
}
func portNumber(addr net.Addr) int {
	_, p, _ := net.SplitHostPort(addr.String())
	n, _ := strconv.Atoi(p)
	return n
}
