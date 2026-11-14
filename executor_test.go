package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSharedExecutorStreamsAndKeepsCommandsSeparate(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("VPS executor uses Unix sockets")
	}
	c, _ := testWorker(t)
	c.ExecSocket = filepath.Join(t.TempDir(), "exec.ssh")
	upstream, e := dialBridge(c)
	if e != nil {
		t.Fatal(e)
	}
	defer upstream.Close()
	var changes atomic.Int32
	broker, e := startLocalExecutor(c, upstream, func() { changes.Add(1) })
	if e != nil {
		t.Fatal(e)
	}
	defer broker.close()
	// Commands must actually use the shared connection; a new direct dial
	// cannot succeed at this address.
	c.Endpoint = "127.0.0.1:1"
	for _, p := range []string{c.ExecSocket, c.ExecSocket + ".pub"} {
		st, e := os.Stat(p)
		if e != nil || st.Mode().Perm() != 0600 {
			t.Fatalf("private endpoint %s: %v, %v", p, st, e)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			var out, errout bytes.Buffer
			result, e := bridgeExec(c, ExecRequest{Kind: "exec", Command: "sh", Args: []string{"-c", "cat; printf stderr >&2; exit 7"}, Cwd: c.RemoteRoot}, strings.NewReader("stdin body"), &out, &errout)
			if e != nil || result.Code != 7 || out.String() != "stdin body" || errout.String() != "stderr" {
				t.Errorf("separate streams/results: %+v, %v, %q, %q", result, e, out.String(), errout.String())
			}
		})
	}
	wg.Wait()
	// Each command may create files, before and after it runs.
	if n := changes.Load(); n != 16 {
		t.Fatalf("mount was told of %d changes for 8 commands", n)
	}
	q := ExecRequest{Kind: "exec", Command: "sh", Args: []string{"-c", "printf once >> exactly-once"}, Cwd: c.RemoteRoot, ID: randomID()}
	if _, e := bridgeExec(c, q, nil, io.Discard, io.Discard); e != nil {
		t.Fatal(e)
	}
	if _, e := bridgeExec(c, q, nil, io.Discard, io.Discard); e == nil {
		t.Fatal("duplicate request was accepted through the shared executor")
	}
	if data, e := os.ReadFile(filepath.Join(c.LocalRoot, "exactly-once")); e != nil || string(data) != "once" {
		t.Fatalf("duplicate command executed: %q, %v", data, e)
	}
	// Cancelling a local command must close its workstation channel without
	// closing the shared connection or repeating an accepted request.
	local, e := dialLocalExecutor(c)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, e := bridgeExecClientContext(ctx, local, ExecRequest{Kind: "exec", Command: "sh", Args: []string{"-c", "echo $$ > command-pid; touch started; sleep 30; touch finished"}, Cwd: c.RemoteRoot}, nil, io.Discard, io.Discard)
		finished <- e
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, e := os.Stat(filepath.Join(c.LocalRoot, "started")); e == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			local.Close()
			t.Fatal("workstation command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pidBytes, e := os.ReadFile(filepath.Join(c.LocalRoot, "command-pid"))
	if e != nil {
		t.Fatal(e)
	}
	pid, e := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if e != nil {
		t.Fatal(e)
	}
	process, e := os.FindProcess(pid)
	if e != nil {
		t.Fatal(e)
	}
	defer process.Release()
	cancel()
	local.Close()
	select {
	case e := <-finished:
		if e == nil {
			t.Fatal("cancelled command returned success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation left the command waiting")
	}
	deadline = time.Now().Add(2 * time.Second)
	for process.Signal(syscall.Signal(0)) == nil {
		if time.Now().After(deadline) {
			process.Kill()
			t.Fatal("cancelled workstation process was still running")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var out bytes.Buffer
	if r, e := bridgeExec(c, ExecRequest{Kind: "shell", Command: "printf still-connected", Cwd: c.RemoteRoot}, nil, &out, io.Discard); e != nil || r.Code != 0 || out.String() != "still-connected" {
		t.Fatalf("unrelated command after cancellation: %+v, %v, %q", r, e, out.String())
	}
	if _, e := os.Stat(filepath.Join(c.LocalRoot, "finished")); !os.IsNotExist(e) {
		t.Fatalf("cancelled command ran to completion: %v", e)
	}
	// Wrong secrets are rejected locally, even though the endpoint is private.
	c.Secret = "wrong"
	if conn, e := dialLocalExecutor(c); e == nil {
		conn.Close()
		t.Fatal("wrong executor secret accepted")
	}
}

func TestPendingSSHClosesAnIncompleteHandshake(t *testing.T) {
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, e := ln.Accept()
		if e == nil {
			accepted <- conn
		}
	}()
	pending := startSSH(ln.Addr().String(), &ssh.ClientConfig{User: "test", HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	conn := <-accepted
	defer conn.Close()
	closed := make(chan struct{})
	go func() { pending.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("closing a pending SSH connection waited for its handshake timeout")
	}
	if client, e := pending.wait(); client != nil || e == nil {
		t.Fatalf("cancelled handshake returned client %v, error %v", client, e)
	}
}
