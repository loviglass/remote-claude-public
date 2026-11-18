package main

import (
	"bytes"
	"encoding/json"
	"net"
	"testing"

	"golang.org/x/crypto/ssh"
)

// An executor of an earlier release knows only session channels with exec
// requests; a new adapter must still reach it while the server binary is
// upgraded under a running session.
func TestExecFallsBackToLegacyExecutor(t *testing.T) {
	signer, err := newHostSigner()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(signer)
	// SSH needs a buffered transport: both sides write their version first.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		a, err := ln.Accept()
		if err != nil {
			return
		}
		_, chans, reqs, err := ssh.NewServerConn(a, cfg)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		for nc := range chans {
			if nc.ChannelType() != "session" {
				nc.Reject(ssh.UnknownChannelType, "session required")
				continue
			}
			ch, requests, _ := nc.Accept()
			go func() {
				defer ch.Close()
				for r := range requests {
					var x struct{ Command string }
					var q ExecRequest
					if r.Type != "exec" || ssh.Unmarshal(r.Payload, &x) != nil || decode(x.Command, &q) != nil {
						r.Reply(false, nil)
						continue
					}
					r.Reply(true, nil)
					ch.Write([]byte("legacy:" + q.Command))
					res, _ := json.Marshal(ExecResult{Code: 7})
					ch.SendRequest("remote-claude-result", false, res)
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{7}))
					return
				}
			}()
		}
	}()
	b, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn, chans, reqs, err := ssh.NewClientConn(b, "legacy", &ssh.ClientConfig{User: "bridge", Auth: []ssh.AuthMethod{ssh.Password("x")}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		t.Fatal(err)
	}
	client := ssh.NewClient(conn, chans, reqs)
	defer client.Close()
	var out bytes.Buffer
	r, err := bridgeExecClient(client, ExecRequest{Kind: "shell", Command: "echo hi"}, nil, &out, &bytes.Buffer{})
	if err != nil || r.Code != 7 || out.String() != "legacy:echo hi" {
		t.Fatalf("result %+v, output %q, err %v", r, out.String(), err)
	}
}
