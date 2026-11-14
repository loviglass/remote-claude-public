package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func TestBashWrapper(t *testing.T) {
	command := "printf '%s\\n' 'hello; не команда'; pwd"
	wrapper := "source /home/dev/.claude/shell-snapshots/snapshot-bash-123.sh 2>/dev/null || true && eval " + shellQuote(command) + " < /dev/null && pwd -P >| /tmp/claude-ab12-cwd"
	got, cwd, e := unwrapBash(wrapper)
	if e != nil || got != command || cwd != "/tmp/claude-ab12-cwd" {
		t.Fatalf("%q %q %v", got, cwd, e)
	}
	if _, _, e = unwrapBash("source /x/shell-snapshots/bad.sh; echo unsafe"); e == nil {
		t.Fatal("unrecognized wrapper was accepted")
	}
}
func TestProjectSettingsRoundTrip(t *testing.T) {
	original := []byte(`{"permissions":{"deny":["Bash(rm *)"]},"env":{"HELLO":"world"},"hooks":{"PostToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"python3","args":["${CLAUDE_PROJECT_DIR}/hook.py"],"timeout":12}]}]},"mcpServers":{"sample":{"command":"node","args":["mcp.js"],"env":{"KEY":"${LOCAL_SECRET}"}}}}`)
	projected, e := projectJSON(original, false)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(projected, []byte(serverBinary)) || !bytes.Contains(projected, []byte("Bash(rm *)")) {
		t.Fatal(string(projected))
	}
	restored, e := projectJSON(projected, true)
	if e != nil {
		t.Fatal(e)
	}
	var a, b any
	json.Unmarshal(original, &a)
	json.Unmarshal(restored, &b)
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if !bytes.Equal(x, y) {
		t.Fatalf("round trip changed configuration: %s", restored)
	}
}
func testWorker(t *testing.T) (SessionConfig, *worker) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "проект с пробелами 'x'")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	r, e := os.OpenRoot(root)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { r.Close() })
	signer, e := newHostSigner()
	if e != nil {
		t.Fatal(e)
	}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { ln.Close() })
	c := SessionConfig{Protocol: 1, Endpoint: ln.Addr().String(), Secret: randomID(), HostKey: string(ssh.MarshalAuthorizedKey(signer.PublicKey())), LocalRoot: root, RemoteRoot: "/remote/project", OS: runtime.GOOS}
	w := &worker{config: c, fs: &projectFS{root: r}, signer: signer, close: make(chan struct{})}
	go w.serve(ln)
	t.Cleanup(func() { close(w.close) })
	return c, w
}
func TestFileTransportAndConfigWrite(t *testing.T) {
	c, _ := testWorker(t)
	client, e := dialBridge(c)
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	s, e := sftp.NewClient(client)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Mkdir("/.claude"); e != nil {
		t.Fatal(e)
	}
	h, e := s.Create("/.claude/settings.json")
	if e != nil {
		t.Fatal(e)
	}
	original := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo original"}]}]},"model":"sonnet"}`
	io.WriteString(h, original)
	if e = h.Close(); e != nil {
		t.Fatal(e)
	}
	h, e = s.Open("/.claude/settings.json")
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(h)
	h.Close()
	if e != nil || !bytes.Contains(b, []byte("dispatch")) {
		t.Fatalf("%s: %v", b, e)
	}
	h, e = s.Create("/.claude/settings.json")
	if e != nil {
		t.Fatal(e)
	}
	h.Write(bytes.ReplaceAll(b, []byte("sonnet"), []byte("haiku")))
	if e = h.Close(); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(filepath.Join(c.LocalRoot, ".claude/settings.json"))
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(raw, []byte("dispatch")) || !bytes.Contains(raw, []byte("echo original")) || !bytes.Contains(raw, []byte("haiku")) {
		t.Fatal(string(raw))
	}
	h, e = s.Create("/файл с пробелом.txt")
	if e != nil {
		t.Fatal(e)
	}
	h.Write([]byte("hello"))
	h.Close()
	if e = s.Rename("/файл с пробелом.txt", "/renamed.txt"); e != nil {
		t.Fatal(e)
	}
	raw, e = os.ReadFile(filepath.Join(c.LocalRoot, "renamed.txt"))
	if e != nil || string(raw) != "hello" {
		t.Fatalf("%s %v", raw, e)
	}
}
func TestRealShellExecutionAndHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	c, _ := testWorker(t)
	var out, errout bytes.Buffer
	command := "cd /remote/project && printf '%s' 'локально' > result.txt && mkdir child && cd child"
	result, e := bridgeExec(c, ExecRequest{Kind: "shell", Command: command, Cwd: c.RemoteRoot}, nil, &out, &errout)
	if e != nil || result.Code != 0 || result.Cwd != filepath.Join(c.LocalRoot, "child") {
		t.Fatalf("%+v %v %s", result, e, errout.String())
	}
	b, e := os.ReadFile(filepath.Join(c.LocalRoot, "result.txt"))
	if e != nil || string(b) != "локально" {
		t.Fatalf("%s %v", b, e)
	}
	script := `#!/bin/sh
cat > hook-input.json
printf '%s' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"fixture denial"}}'
`
	if e = os.WriteFile(filepath.Join(c.LocalRoot, "hook.sh"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	out.Reset()
	errout.Reset()
	result, e = bridgeExec(c, ExecRequest{Kind: "hook", Cwd: c.RemoteRoot, Spec: map[string]any{"type": "command", "command": "/bin/sh", "args": []any{"${CLAUDE_PROJECT_DIR}/hook.sh"}}}, strings.NewReader(`{"cwd":"/remote/project","tool_name":"Bash"}`), &out, &errout)
	if e != nil || result.Code != 0 || !strings.Contains(out.String(), "fixture denial") {
		t.Fatalf("%+v %v %s %s", result, e, out.String(), errout.String())
	}
	b, e = os.ReadFile(filepath.Join(c.LocalRoot, "hook-input.json"))
	if e != nil || !bytes.Contains(b, []byte("tool_name")) {
		t.Fatal(string(b), e)
	}
}
func TestRejectedBridgeSecret(t *testing.T) {
	c, _ := testWorker(t)
	c.Secret = "wrong"
	if s, e := dialBridge(c); e == nil {
		s.Close()
		t.Fatal("wrong secret accepted")
	}
}
func TestPartialConfigWriteDoesNotCommit(t *testing.T) {
	_, w := testWorker(t)
	if e := w.fs.root.Mkdir(".claude", 0700); e != nil {
		t.Fatal(e)
	}
	if e := w.fs.root.WriteFile(".claude/settings.json", []byte(`{"model":"sonnet"}`), 0600); e != nil {
		t.Fatal(e)
	}
	h := &configHandle{fs: w.fs, path: ".claude/settings.json", data: []byte(`{"model":"haiku"}`), mode: 0600}
	h.TransferError(io.ErrUnexpectedEOF)
	if h.Close() == nil {
		t.Fatal("partial upload committed")
	}
	b, _ := w.fs.root.ReadFile(h.path)
	if string(b) != `{"model":"sonnet"}` {
		t.Fatal(string(b))
	}
}
func TestDisconnectKillsCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process fixture")
	}
	_, w := testWorker(t)
	disconnected := make(chan struct{})
	finished := make(chan ExecResult, 1)
	go func() {
		finished <- w.execute(ExecRequest{Kind: "shell", Command: "sleep 20", Cwd: w.config.RemoteRoot}, nil, io.Discard, io.Discard, disconnected)
	}()
	time.Sleep(100 * time.Millisecond)
	close(disconnected)
	select {
	case r := <-finished:
		if r.Code == 0 {
			t.Fatal("command survived cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process group was not canceled")
	}
}

func TestBashFinishesWithOpenInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	c, _ := testWorker(t)
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	done := make(chan error, 1)
	go func() {
		_, e := bridgeExec(c, ExecRequest{Kind: "shell", Command: "printf finished", Cwd: c.RemoteRoot}, r, io.Discard, io.Discard)
		done <- e
	}()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("completed command waits for unused stdin")
	}
}

// Claude leaves stdin open on Git children such as `git worktree list`; the
// workstation must answer once the process exits instead of waiting for EOF.
func TestExecFinishesWithOpenInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	c, _ := testWorker(t)
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, e := bridgeExec(c, ExecRequest{Kind: "exec", Command: "true", Cwd: c.RemoteRoot}, r, io.Discard, io.Discard)
		done <- e
	}()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("completed command waited %v for unused stdin", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("completed command waits for unused stdin")
	}
	var out bytes.Buffer
	if _, e := bridgeExec(c, ExecRequest{Kind: "exec", Command: "cat", Cwd: c.RemoteRoot}, strings.NewReader("forwarded"), &out, io.Discard); e != nil || out.String() != "forwarded" {
		t.Fatalf("%q %v", out.String(), e)
	}
}

func TestSkillFrontmatterHooks(t *testing.T) {
	s := []byte("---\nname: test\nhooks:\n  PreToolUse:\n    - matcher: Bash\n      hooks:\n        - type: command\n          command: echo skill-hook\n---\n# Keep this body byte for byte\n")
	b, e := projectJSON(s, false)
	if e != nil || !bytes.Contains(b, []byte(serverBinary)) {
		t.Fatalf("%s %v", b, e)
	}
	b, e = projectJSON(b, true)
	if e != nil || !bytes.Contains(b, []byte("echo skill-hook")) || !bytes.HasSuffix(b, []byte("---\n# Keep this body byte for byte\n")) {
		t.Fatalf("%s %v", b, e)
	}
}

func TestLocalSessionEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("CLAUDE_ENV_FILE is a Bash feature")
	}
	c, w := testWorker(t)
	t.Cleanup(func() {
		if p, _ := w.localEnvFile(false); p != "" {
			os.Remove(p)
		}
	})
	_, e := bridgeExec(c, ExecRequest{Kind: "hook", Cwd: c.RemoteRoot, Env: map[string]string{"CLAUDE_ENV_FILE": "/vps/env.sh"}, Spec: map[string]any{"type": "command", "command": `printf '%s\n' 'export BRIDGE_TEST_ENV=from-hook' >> "$CLAUDE_ENV_FILE"`}}, strings.NewReader("{}"), io.Discard, io.Discard)
	if e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	_, e = bridgeExec(c, ExecRequest{Kind: "shell", Cwd: c.RemoteRoot, Command: `printf '%s' "$BRIDGE_TEST_ENV"`}, nil, &out, io.Discard)
	if e != nil || out.String() != "from-hook" {
		t.Fatalf("%s %v", out.String(), e)
	}
}

func TestHTTPMCPProxyAndRestore(t *testing.T) {
	_, w := testWorker(t)
	t.Setenv("BRIDGE_HEADER_TEST", "local-secret")
	upstream := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Local-Test") != "local-secret" {
			out.WriteHeader(403)
			return
		}
		if r.URL.Path == "/sse" {
			out.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(out, "event: endpoint\ndata: /messages?session=test\n\n")
			return
		}
		io.WriteString(out, "LOCAL_HTTP_OK "+r.URL.RequestURI())
	}))
	defer upstream.Close()
	p := &httpMCPProxies{listen: net.Listen, worker: w}
	defer p.close()
	spec := map[string]any{"type": "http", "url": upstream.URL + "/mcp?test=yes", "headers": map[string]any{"X-Local-Test": "${BRIDGE_HEADER_TEST}"}}
	view, ok, e := p.project(spec, false)
	if e != nil || !ok {
		t.Fatal(e)
	}
	if _, ok := view["headers"]; ok {
		t.Fatal("workstation header was exported")
	}
	r, e := http.Get(view["url"].(string))
	if e != nil {
		t.Fatal(e)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if string(b) != "LOCAL_HTTP_OK /mcp?test=yes" {
		t.Fatal(string(b))
	}
	original, ok, e := p.project(view, true)
	if e != nil || !ok || encode(original) != encode(spec) {
		t.Fatal("proxy failed to restore source")
	}
	spec["url"] = upstream.URL + "/sse"
	view, _, e = p.project(spec, false)
	if e != nil {
		t.Fatal(e)
	}
	r, e = http.Get(view["url"].(string))
	if e != nil {
		t.Fatal(e)
	}
	b, _ = io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(b), strings.TrimSuffix(view["url"].(string), "/sse")+"/messages?session=test") {
		t.Fatal(string(b))
	}
}

func TestRealPowerShellTransport(t *testing.T) {
	if _, e := exec.LookPath("pwsh"); e != nil {
		t.Skip("optional real PowerShell runtime not installed")
	}
	c, _ := testWorker(t)
	if e := os.Mkdir(filepath.Join(c.LocalRoot, "child"), 0700); e != nil {
		t.Fatal(e)
	}
	script := `Set-Content -LiteralPath /remote/project/powershell.txt -Value 'локальный PowerShell'; Get-Content -LiteralPath /remote/project/powershell.txt; Set-Location /remote/project/child; (Get-Location).Path | Out-File -FilePath $env:REMOTE_CLAUDE_CWD_FILE -Encoding utf8 -NoNewline`
	var out, errout bytes.Buffer
	r, e := bridgeExec(c, ExecRequest{Kind: "powershell", Cwd: c.RemoteRoot, Args: []string{"-NoProfile", "-NonInteractive", "-Command", script}, Spec: map[string]any{"track_cwd": true}}, nil, &out, &errout)
	if e != nil || r.Code != 0 || !strings.Contains(out.String(), "локальный PowerShell") || r.Cwd != filepath.Join(c.LocalRoot, "child") {
		t.Fatalf("%+v %v stdout=%s stderr=%s", r, e, out.String(), errout.String())
	}
}
