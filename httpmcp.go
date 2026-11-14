package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// HTTP and SSE MCP connections enter through loopback SSH forwards. Requests
// then originate on the workstation, including access to localhost/VPN and
// expansion of local header environment variables.
type httpMCPProxies struct {
	listen    func(string, string) (net.Listener, error)
	worker    *worker
	mu        sync.Mutex
	bySpec    map[string]map[string]any
	byURL     map[string]map[string]any
	listeners []net.Listener
}

func (p *httpMCPProxies) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ln := range p.listeners {
		ln.Close()
	}
}
func (p *httpMCPProxies) project(spec map[string]any, unwrap bool) (map[string]any, bool, error) {
	u, ok := spec["url"].(string)
	if !ok {
		return nil, false, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if unwrap {
		original, ok := p.byURL[u]
		return original, ok, nil
	}
	key := encode(spec)
	if cached, ok := p.bySpec[key]; ok {
		return cached, true, nil
	}
	env := p.worker.env(ExecRequest{})
	target, e := url.Parse(expandVars(u, env))
	if e != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return nil, false, fmt.Errorf("invalid HTTP MCP URL")
	}
	ln, e := p.listen("tcp", "127.0.0.1:0")
	if e != nil {
		return nil, false, e
	}
	p.listeners = append(p.listeners, ln)
	prefix := "/" + randomID()
	base := fmt.Sprintf("http://127.0.0.1:%d", portNumber(ln.Addr()))
	proxy := &httputil.ReverseProxy{FlushInterval: -1, Rewrite: func(r *httputil.ProxyRequest) {
		r.Out.URL.Scheme = target.Scheme
		r.Out.URL.Host = target.Host
		r.Out.URL.Path = strings.TrimPrefix(r.In.URL.Path, prefix)
		r.Out.URL.RawPath = ""
		r.Out.Host = target.Host
		if headers, ok := spec["headers"].(map[string]any); ok {
			for k, v := range headers {
				if s, ok := v.(string); ok {
					r.Out.Header.Set(k, expandVars(s, env))
				}
			}
		}
	}, ModifyResponse: func(r *http.Response) error {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "text/event-stream") {
			r.Body = &sseEndpointReader{body: r.Body, reader: bufio.NewReader(r.Body), base: base + prefix, target: target}
			r.ContentLength = -1
			r.Header.Del("Content-Length")
		}
		return nil
	}}
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix+"/") && r.URL.Path != prefix {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})}
	go server.Serve(ln)
	projected := map[string]any{}
	for k, v := range spec {
		if k != "headers" {
			projected[k] = v
		}
	}
	projected["url"] = base + prefix + target.EscapedPath()
	if target.RawQuery != "" {
		projected["url"] = projected["url"].(string) + "?" + target.RawQuery
	}
	if p.bySpec == nil {
		p.bySpec = map[string]map[string]any{}
		p.byURL = map[string]map[string]any{}
	}
	p.bySpec[key] = projected
	p.byURL[projected["url"].(string)] = spec
	return projected, true, nil
}

// Legacy SSE sends a relative POST endpoint as a server event. Preserve that
// endpoint through the same authenticated tunnel without changing tool events.
type sseEndpointReader struct {
	body     io.ReadCloser
	reader   *bufio.Reader
	pending  []byte
	endpoint bool
	base     string
	target   *url.URL
}

func (s *sseEndpointReader) Close() error { return s.body.Close() }
func (s *sseEndpointReader) Read(b []byte) (int, error) {
	if len(s.pending) == 0 {
		line, e := s.reader.ReadString('\n')
		if line == "" {
			return 0, e
		}
		if strings.HasPrefix(line, "event:") {
			s.endpoint = strings.TrimSpace(strings.TrimPrefix(line, "event:")) == "endpoint"
		}
		if s.endpoint && strings.HasPrefix(line, "data:") {
			if endpoint, e := url.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:"))); e == nil {
				endpoint = s.target.ResolveReference(endpoint)
				if endpoint.Host == s.target.Host {
					line = "data: " + s.base + endpoint.RequestURI() + "\n"
				}
			}
		}
		if strings.TrimSpace(line) == "" {
			s.endpoint = false
		}
		s.pending = []byte(line)
	}
	n := copy(b, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}
