package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestDialSSHThroughHTTPProxy(t *testing.T) {
	c, _ := testWorker(t)
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.HostKey))
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := proxy.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		req, err := http.ReadRequest(reader)
		if err != nil {
			done <- err
			return
		}
		if req.Method != http.MethodConnect || req.Host != c.Endpoint {
			done <- fmt.Errorf("unexpected proxy request: %s %s", req.Method, req.Host)
			return
		}
		backend, err := net.Dial("tcp", c.Endpoint)
		if err != nil {
			done <- err
			return
		}
		defer backend.Close()
		if _, err = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			done <- err
			return
		}
		copyConnections(&bufferedConn{Conn: conn, reader: reader}, backend)
		done <- nil
	}()
	client, err := dialSSHWithDeadlineVia(c.Endpoint, &ssh.ClientConfig{
		User: "bridge", Auth: []ssh.AuthMethod{ssh.Password(c.Secret)},
		HostKeyCallback: ssh.FixedHostKey(pub),
	}, proxy.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP proxy did not close after SSH client disconnected")
	}
}

func TestTunnelHTTPProxyKeepsBufferedSSHBytes(t *testing.T) {
	client, proxy := net.Pipe()
	defer client.Close()
	defer proxy.Close()
	client.SetDeadline(time.Now().Add(2 * time.Second))
	proxy.SetDeadline(time.Now().Add(2 * time.Second))
	go func() {
		reader := bufio.NewReader(proxy)
		for {
			line, err := reader.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		io.WriteString(proxy, "HTTP/1.1 200 Connection Established\r\n\r\nSSH-2.0-test\r\n")
	}()
	tunnel, err := tunnelHTTPProxy(client, "example.com:22")
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(tunnel).ReadString('\n')
	if err != nil || line != "SSH-2.0-test\r\n" {
		t.Fatalf("SSH banner after CONNECT: %q, %v", line, err)
	}
}

func TestTunnelHTTPProxyRejectsFailure(t *testing.T) {
	client, proxy := net.Pipe()
	defer client.Close()
	defer proxy.Close()
	client.SetDeadline(time.Now().Add(2 * time.Second))
	proxy.SetDeadline(time.Now().Add(2 * time.Second))
	go func() {
		reader := bufio.NewReader(proxy)
		for {
			line, err := reader.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		io.WriteString(proxy, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
	}()
	if _, err := tunnelHTTPProxy(client, "example.com:22"); err == nil || !strings.Contains(err.Error(), "407") {
		t.Fatalf("expected CONNECT refusal, got %v", err)
	}
}
