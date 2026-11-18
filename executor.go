package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// The SFTP process already owns an authenticated workstation connection. A
// private local SSH endpoint lets Git, shell and hook subprocesses open new
// channels on it instead of repeating network handshakes for every command.
// Each command still has its own stdin, stdout, stderr and cancellation.
type localExecutor struct {
	listener net.Listener
	clients  sync.Map
	stop     chan struct{}
	path     string
	// upstream returns the current workstation connection, waiting for one
	// while the workstation reconnects.
	upstream func() (*ssh.Client, error)
	config   *ssh.ServerConfig
	// before and after run around each workstation command: they synchronise
	// the project so the command and Claude see the same files.
	before func(*ExecRequest)
	after  func(*ExecRequest, *ExecResult, io.Writer)
}

func startLocalExecutor(c SessionConfig, upstream *ssh.Client, changed func()) (*localExecutor, error) {
	hook := func(*ExecRequest) { changed() }
	return newLocalExecutor(c.ExecSocket, c.Secret, func() (*ssh.Client, error) { return upstream, nil }, hook, func(q *ExecRequest, _ *ExecResult, _ io.Writer) { hook(q) })
}

// Commands that only read workstation state need no synchronisation after.
func syncAfter(q *ExecRequest) bool {
	switch q.Kind {
	case "read-file", "config-snapshot", "search", "mcp", "sync":
		return false
	case "exec":
		return !(q.Command == "git" && gitReadOnly(q.Args))
	}
	return true
}

func newLocalExecutor(path, secret string, upstream func() (*ssh.Client, error), before func(*ExecRequest), after func(*ExecRequest, *ExecResult, io.Writer)) (*localExecutor, error) {
	signer, e := newHostSigner()
	if e != nil {
		return nil, e
	}
	ln, e := net.Listen("unix", path)
	if e != nil {
		return nil, e
	}
	c := SessionConfig{ExecSocket: path, Secret: secret}
	b := &localExecutor{listener: ln, path: path, upstream: upstream, stop: make(chan struct{}), before: before, after: after}
	ok := false
	defer func() {
		if !ok {
			b.close()
		}
	}()
	if e = os.Chmod(c.ExecSocket, 0600); e != nil {
		return nil, e
	}
	if e = os.WriteFile(c.ExecSocket+".pub", ssh.MarshalAuthorizedKey(signer.PublicKey()), 0600); e != nil {
		return nil, e
	}
	b.config = &ssh.ServerConfig{PasswordCallback: func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if subtle.ConstantTimeCompare(password, []byte(c.Secret)) != 1 {
			return nil, fmt.Errorf("authentication failed")
		}
		return nil, nil
	}}
	b.config.AddHostKey(signer)
	go b.serve()
	ok = true
	return b, nil
}

func (b *localExecutor) close() {
	close(b.stop)
	b.listener.Close()
	b.clients.Range(func(k, _ any) bool { k.(net.Conn).Close(); return true })
	os.Remove(b.path)
	os.Remove(b.path + ".pub")
}

func (b *localExecutor) serve() {
	for {
		conn, e := b.listener.Accept()
		if e != nil {
			return
		}
		b.clients.Store(conn, true)
		select {
		case <-b.stop:
			conn.Close()
			b.clients.Delete(conn)
			return
		default:
		}
		go b.handle(conn)
	}
}

func (b *localExecutor) handle(conn net.Conn) {
	defer conn.Close()
	defer b.clients.Delete(conn)
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	server, channels, requests, e := ssh.NewServerConn(conn, b.config)
	if e != nil {
		return
	}
	conn.SetDeadline(time.Time{})
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { server.Wait(); cancel() }()
	go ssh.DiscardRequests(requests)
	for ch := range channels {
		switch ch.ChannelType() {
		case "session":
			channel, requests, e := ch.Accept()
			if e == nil {
				go b.session(ctx, channel, requests)
			}
		case execChannelType:
			var q ExecRequest
			if decode(string(ch.ExtraData()), &q) != nil || q.ID == "" {
				ch.Reject(ssh.Prohibited, "invalid request")
				continue
			}
			channel, requests, e := ch.Accept()
			if e == nil {
				go func() { defer channel.Close(); b.run(ctx, channel, requests, q) }()
			}
		default:
			ch.Reject(ssh.UnknownChannelType, "session required")
		}
	}
}

func (b *localExecutor) session(ctx context.Context, ch ssh.Channel, requests <-chan *ssh.Request) {
	defer ch.Close()
	for r := range requests {
		var command struct{ Command string }
		var q ExecRequest
		if r.Type != "exec" || ssh.Unmarshal(r.Payload, &command) != nil || decode(command.Command, &q) != nil || q.ID == "" {
			r.Reply(false, nil)
			continue
		}
		r.Reply(true, nil)
		b.run(ctx, ch, requests, q)
		return
	}
}

func (b *localExecutor) run(ctx context.Context, ch ssh.Channel, requests <-chan *ssh.Request, q ExecRequest) {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Closing this local channel cancels only this workstation command.
	// Other commands keep using the shared authenticated connection.
	go func() { ssh.DiscardRequests(requests); cancel() }()
	if b.before != nil {
		b.before(&q)
	}
	var result ExecResult
	upstream, e := b.upstream()
	if e == nil {
		result, e = bridgeExecClientContext(commandCtx, upstream, q, ch, ch, ch.Stderr())
	} else {
		result.Code = 255
	}
	if b.after != nil {
		b.after(&q, &result, ch.Stderr())
	}
	if e != nil {
		result.Error = e.Error()
	}
	payload, _ := json.Marshal(result)
	ch.SendRequest("remote-claude-result", false, payload)
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{uint32(result.Code)}))
}

func dialLocalExecutor(c SessionConfig) (*ssh.Client, error) {
	if c.ExecSocket == "" {
		return nil, fmt.Errorf("shared executor is unavailable")
	}
	key, e := os.ReadFile(c.ExecSocket + ".pub")
	if e != nil {
		return nil, e
	}
	public, _, _, _, e := ssh.ParseAuthorizedKey(key)
	if e != nil {
		return nil, e
	}
	conn, e := net.DialTimeout("unix", c.ExecSocket, time.Second)
	if e != nil {
		return nil, e
	}
	conn.SetDeadline(time.Now().Add(time.Second))
	client, channels, requests, e := ssh.NewClientConn(conn, "local executor", &ssh.ClientConfig{
		User: "bridge", Auth: []ssh.AuthMethod{ssh.Password(c.Secret)}, HostKeyCallback: ssh.FixedHostKey(public),
	})
	if e != nil {
		conn.Close()
		return nil, e
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(client, channels, requests), nil
}

// gitReadOnly recognises Git commands that leave the working tree alone.
// worktree is excluded: Claude creates its agent worktrees inside the project.
func gitReadOnly(args []string) bool {
	for _, a := range args {
		if a == "worktree" || a == "--output" || strings.HasPrefix(a, "--output=") {
			return false
		}
	}
	return gitMetadataWithoutInput(args)
}
