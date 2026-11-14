package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
)

const defaultHTTPProxyPort = 10809

type proxyFlags struct {
	enabled bool
	host    string
	port    int
	flags   *flag.FlagSet
}

func addProxyFlags(fs *flag.FlagSet) *proxyFlags {
	p := &proxyFlags{flags: fs}
	fs.BoolVar(&p.enabled, "proxy", false, "connect to the server through an HTTP CONNECT proxy")
	fs.StringVar(&p.host, "proxy-host", "127.0.0.1", "HTTP CONNECT proxy host (requires --proxy)")
	fs.IntVar(&p.port, "proxy-port", defaultHTTPProxyPort, "HTTP CONNECT proxy port (requires --proxy)")
	return p
}

func (p *proxyFlags) address() (string, error) {
	if !p.enabled {
		var override bool
		p.flags.Visit(func(f *flag.Flag) {
			if f.Name == "proxy-host" || f.Name == "proxy-port" {
				override = true
			}
		})
		if override {
			return "", fmt.Errorf("--proxy-host and --proxy-port require --proxy")
		}
		return "", nil
	}
	if p.host == "" || strings.ContainsAny(p.host, " \t\r\n/") {
		return "", fmt.Errorf("invalid --proxy-host")
	}
	if p.port < 1 || p.port > 65535 {
		return "", fmt.Errorf("--proxy-port must be between 1 and 65535")
	}
	return net.JoinHostPort(p.host, strconv.Itoa(p.port)), nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// tunnelHTTPProxy turns a local HTTP proxy connection into a raw SSH stream.
// The reader is retained because the proxy response may arrive together with
// the SSH server's identification line.
func tunnelHTTPProxy(conn net.Conn, target string) (net.Conn, error) {
	if target == "" || strings.ContainsAny(target, " \t\r\n") {
		return nil, fmt.Errorf("invalid SSH target address")
	}
	if _, e := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); e != nil {
		return nil, e
	}
	reader := bufio.NewReader(conn)
	response, e := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if e != nil {
		return nil, e
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CONNECT %s: %s", target, response.Status)
	}
	return &bufferedConn{Conn: conn, reader: reader}, nil
}
