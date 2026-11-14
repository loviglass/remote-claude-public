package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"os"
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
	upstream *ssh.Client
	config   *ssh.ServerConfig
	// changed runs before and after each workstation command, which may
	// create files the mount has recently reported missing.
	changed func()
}

func startLocalExecutor(c SessionConfig, upstream *ssh.Client, changed func()) (*localExecutor, error) {
	signer, e := newHostSigner()
	if e != nil {
		return nil, e
	}
	ln, e := net.Listen("unix", c.ExecSocket)
	if e != nil {
		return nil, e
	}
	b := &localExecutor{listener: ln, path: c.ExecSocket, upstream: upstream, stop: make(chan struct{}), changed: changed}
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
		if ch.ChannelType() != "session" {
			ch.Reject(ssh.UnknownChannelType, "session required")
			continue
		}
		channel, requests, e := ch.Accept()
		if e == nil {
			go b.session(ctx, channel, requests)
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
		commandCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		// Closing this local channel cancels only this workstation command.
		// Other commands keep using the shared authenticated connection.
		go func() { ssh.DiscardRequests(requests); cancel() }()
		if b.changed != nil {
			b.changed()
		}
		result, e := bridgeExecClientContext(commandCtx, b.upstream, q, ch, ch, ch.Stderr())
		if b.changed != nil {
			b.changed()
		}
		if e != nil {
			result.Error = e.Error()
		}
		payload, _ := json.Marshal(result)
		ch.SendRequest("remote-claude-result", false, payload)
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{uint32(result.Code)}))
		return
	}
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
