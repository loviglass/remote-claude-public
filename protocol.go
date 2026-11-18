package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type Profile struct {
	Host         string `json:"host"`
	User         string `json:"user"`
	Fingerprint  string `json:"fingerprint"`
	Identity     string `json:"identity,omitempty"`
	BootstrapKey string `json:"bootstrap_key,omitempty"`
}
type SessionConfig struct {
	Protocol       int      `json:"protocol"`
	Endpoint       string   `json:"endpoint"`
	Secret         string   `json:"secret"`
	HostKey        string   `json:"host_key"`
	LocalRoot      string   `json:"local_root"`
	RemoteRoot     string   `json:"remote_root"`
	OS             string   `json:"os"`
	Shell          string   `json:"shell"`
	ID             string   `json:"id"`
	Args           []string `json:"args,omitempty"`
	FreshMetadata  bool     `json:"fresh_metadata,omitempty"`
	ConfigSnapshot bool     `json:"config_snapshot,omitempty"`
	GitSnapshot    bool     `json:"git_snapshot,omitempty"`
	ExtraConfig    bool     `json:"extra_config,omitempty"`
	ExecSocket     string   `json:"exec_socket,omitempty"`
	LocalSearch    bool     `json:"local_search,omitempty"`
	BulkRead       bool     `json:"bulk_read,omitempty"`
	// Replica: file tools use a synchronised copy at RemoteRoot; Git-ignored
	// paths are symlinks into the live view at LiveRoot.
	Replica  bool   `json:"replica,omitempty"`
	LiveRoot string `json:"live_root,omitempty"`
}
type ExecRequest struct {
	Kind    string            `json:"kind"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Spec    map[string]any    `json:"spec,omitempty"`
	ID      string            `json:"id"`
	// SyncSeq: replica changes up to this sequence must be on the
	// workstation before the command starts. SyncAfter asks the workstation
	// to report the command's file changes in ExecResult.SyncSeq.
	SyncSeq   uint64 `json:"sync_seq,omitempty"`
	SyncEpoch string `json:"sync_epoch,omitempty"`
	SyncAfter bool   `json:"sync_after,omitempty"`
}
type ExecResult struct {
	Cwd       string `json:"cwd,omitempty"`
	Code      int    `json:"code"`
	Error     string `json:"error,omitempty"`
	SyncSeq   uint64 `json:"sync_seq,omitempty"`
	SyncEpoch string `json:"sync_epoch,omitempty"`
}

func randomID() string {
	b := make([]byte, 24)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func encode(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
func decode(s string, v any) error {
	b, e := base64.RawURLEncoding.DecodeString(s)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func newHostSigner() (ssh.Signer, error) {
	_, k, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		return nil, e
	}
	return ssh.NewSignerFromKey(k)
}
func dialBridge(c SessionConfig) (*ssh.Client, error) {
	pub, _, _, _, e := ssh.ParseAuthorizedKey([]byte(c.HostKey))
	if e != nil {
		return nil, e
	}
	return dialSSHWithDeadline(c.Endpoint, &ssh.ClientConfig{User: "bridge", Auth: []ssh.AuthMethod{ssh.Password(c.Secret)}, HostKeyCallback: ssh.FixedHostKey(pub), Timeout: 15 * time.Second})
}
func dialSSHWithDeadline(address string, config *ssh.ClientConfig) (*ssh.Client, error) {
	return dialSSHContext(context.Background(), address, config)
}

func dialSSHWithDeadlineVia(address string, config *ssh.ClientConfig, proxyAddress string) (*ssh.Client, error) {
	return dialSSHContextVia(context.Background(), address, config, proxyAddress)
}

func dialSSHContext(ctx context.Context, address string, config *ssh.ClientConfig) (*ssh.Client, error) {
	return dialSSHContextVia(ctx, address, config, "")
}

func dialSSHContextVia(ctx context.Context, address string, config *ssh.ClientConfig, proxyAddress string) (*ssh.Client, error) {
	dialAddress := address
	if proxyAddress != "" {
		dialAddress = proxyAddress
	}
	rawConn, e := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", dialAddress)
	if e != nil {
		if proxyAddress != "" {
			return nil, fmt.Errorf("HTTP proxy %s: %w", proxyAddress, e)
		}
		return nil, e
	}
	stop := context.AfterFunc(ctx, func() { rawConn.Close() })
	defer stop()
	rawConn.SetDeadline(time.Now().Add(15 * time.Second))
	var conn net.Conn = rawConn
	if proxyAddress != "" {
		conn, e = tunnelHTTPProxy(rawConn, address)
		if e != nil {
			rawConn.Close()
			return nil, fmt.Errorf("HTTP proxy %s: %w", proxyAddress, e)
		}
	}
	c, channels, requests, e := ssh.NewClientConn(conn, address, config)
	if e != nil {
		conn.Close()
		return nil, e
	}
	if e := ctx.Err(); e != nil {
		conn.Close()
		return nil, e
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, channels, requests), nil
}

type pendingSSH struct {
	done   chan struct{}
	cancel context.CancelFunc
	client *ssh.Client
	err    error
}

func startSSH(address string, config *ssh.ClientConfig) *pendingSSH {
	return startSSHVia(address, config, "")
}

func startSSHVia(address string, config *ssh.ClientConfig, proxyAddress string) *pendingSSH {
	ctx, cancel := context.WithCancel(context.Background())
	p := &pendingSSH{done: make(chan struct{}), cancel: cancel}
	go func() {
		p.client, p.err = dialSSHContextVia(ctx, address, config, proxyAddress)
		close(p.done)
	}()
	return p
}

func (p *pendingSSH) wait() (*ssh.Client, error) {
	<-p.done
	return p.client, p.err
}

func (p *pendingSSH) close() {
	p.cancel()
	<-p.done
	if p.client != nil {
		p.client.Close()
	}
}
func sessionConfig() (SessionConfig, error) {
	var c SessionConfig
	p := os.Getenv("REMOTE_CLAUDE_SESSION")
	if p == "" {
		return c, fmt.Errorf("missing REMOTE_CLAUDE_SESSION")
	}
	e := readJSON(p, &c)
	return c, e
}
func bridgeExec(c SessionConfig, r ExecRequest, in io.Reader, out, errout io.Writer) (ExecResult, error) {
	client, e := dialLocalExecutor(c)
	if e != nil {
		// No exec request has been sent, so a direct connection is safe. Never
		// repeat a command after an accepted request or uncertain outcome.
		client, e = dialBridge(c)
	}
	if e != nil {
		return ExecResult{}, e
	}
	defer client.Close()
	return bridgeExecClient(client, r, in, out, errout)
}

func bridgeExecClient(client *ssh.Client, r ExecRequest, in io.Reader, out, errout io.Writer) (ExecResult, error) {
	return bridgeExecClientContext(context.Background(), client, r, in, out, errout)
}

func bridgeExecClientContext(ctx context.Context, client *ssh.Client, r ExecRequest, in io.Reader, out, errout io.Writer) (ExecResult, error) {
	if e := ctx.Err(); e != nil {
		return ExecResult{Code: 255}, e
	}
	if r.ID == "" {
		r.ID = randomID()
	}
	// The request travels with the channel open: one round trip, then output.
	ch, requests, e := client.OpenChannel(execChannelType, []byte(encode(r)))
	var open *ssh.OpenChannelError
	if errors.As(e, &open) && open.Reason == ssh.UnknownChannelType {
		// A session of an earlier release keeps its executor while the
		// server binary is upgraded underneath it.
		ch, requests, e = legacyExec(client, r)
	}
	if e != nil {
		return ExecResult{}, fmt.Errorf("executor rejected request: %w", e)
	}
	defer ch.Close()
	stop := context.AfterFunc(ctx, func() { ch.Close() })
	defer stop()
	if in != nil {
		go func() { io.Copy(ch, in); ch.CloseWrite() }()
	} else {
		ch.CloseWrite()
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(out, ch) }()
	go func() { defer wg.Done(); io.Copy(errout, ch.Stderr()) }()
	result := ExecResult{Code: 255}
	haveResult := false
	for q := range requests {
		if q.Type == "remote-claude-result" {
			json.Unmarshal(q.Payload, &result)
			haveResult = true
		}
		if q.Type == "exit-status" && !haveResult {
			var s struct{ Status uint32 }
			if ssh.Unmarshal(q.Payload, &s) == nil {
				result.Code = int(s.Status)
			}
		}
		if q.WantReply {
			q.Reply(false, nil)
		}
	}
	wg.Wait()
	if !haveResult {
		return result, fmt.Errorf("connection lost; command outcome is unknown; command was not retried")
	}
	if result.Error != "" {
		return result, fmt.Errorf("%s", result.Error)
	}
	return result, nil
}

const execChannelType = "rc-exec"

func legacyExec(client *ssh.Client, r ExecRequest) (ssh.Channel, <-chan *ssh.Request, error) {
	ch, requests, e := client.OpenChannel("session", nil)
	if e != nil {
		return nil, nil, e
	}
	payload := ssh.Marshal(struct{ Command string }{encode(r)})
	if ok, e := ch.SendRequest("exec", true, payload); e != nil || !ok {
		ch.Close()
		return nil, nil, fmt.Errorf("executor rejected request: %v", e)
	}
	return ch, requests, nil
}

func copyConnections(a, b net.Conn) {
	var w sync.WaitGroup
	w.Add(2)
	go func() { defer w.Done(); io.Copy(a, b); a.Close() }()
	go func() { defer w.Done(); io.Copy(b, a); b.Close() }()
	w.Wait()
}
