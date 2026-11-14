package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io"
	"os"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

func unwrapBash(s string) (string, string, error) {
	if !strings.Contains(s, "shell-snapshots/") {
		return s, "", nil
	}
	tree, e := syntax.NewParser().Parse(strings.NewReader(s), "")
	if e != nil {
		return "", "", e
	}
	var command, cwd string
	evalCount := 0
	syntax.Walk(tree, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok && len(c.Args) > 0 {
			first, _ := expand.Literal(nil, c.Args[0])
			if first == "eval" {
				evalCount++
				if len(c.Args) == 2 {
					command, _ = expand.Literal(nil, c.Args[1])
				}
			}
		}
		if r, ok := n.(*syntax.Redirect); ok {
			p, _ := expand.Literal(nil, r.Word)
			if strings.HasPrefix(p, "/tmp/claude-") && strings.HasSuffix(p, "-cwd") {
				cwd = p
			}
		}
		return true
	})
	if evalCount != 1 || cwd == "" {
		return "", "", fmt.Errorf("unrecognized Claude shell wrapper; refusing to execute it on either machine")
	}
	return command, cwd, nil
}
func bridgeShell(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("shell prefix expects one command argument")
	}
	c, e := sessionConfig()
	if e != nil {
		return e
	}
	command, cwdFile, e := unwrapBash(args[0])
	if e != nil {
		return e
	}
	// Claude applies SHELL_PREFIX to stdio MCP processes too. A projected
	// exec-form adapter must run here before forwarding its original spec.
	if cwdFile == "" {
		if a, ok := transportCommand(command); ok {
			return bridgeDispatch(a)
		}
	}
	cwd, _ := os.Getwd()
	kind := "shell"
	if cwdFile == "" {
		kind = "shell-raw"
	}
	env := bridgeEnvironment()
	result, e := bridgeExec(c, ExecRequest{Kind: kind, Command: command, Cwd: cwd, Env: env}, os.Stdin, os.Stdout, os.Stderr)
	if e != nil {
		return e
	}
	if cwdFile != "" && result.Code == 0 {
		if e = writeRemoteCwd(c, cwdFile, result.Cwd); e != nil {
			return e
		}
	}
	if result.Code != 0 {
		return &exitError{result.Code}
	}
	return nil
}
func transportCommand(s string) ([]string, bool) {
	tree, e := syntax.NewParser().Parse(strings.NewReader(s), "")
	if e != nil || len(tree.Stmts) != 1 {
		return nil, false
	}
	stmt := tree.Stmts[0]
	if stmt.Negated || stmt.Background || len(stmt.Redirs) != 0 {
		return nil, false
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) != 4 || len(call.Assigns) != 0 {
		return nil, false
	}
	a := make([]string, 4)
	for i, word := range call.Args {
		a[i], e = expand.Literal(nil, word)
		if e != nil {
			return nil, false
		}
	}
	if a[0] == serverBinary && a[1] == "dispatch" && (a[2] == "mcp" || a[2] == "hook") {
		return a[2:], true
	}
	return nil, false
}
func bridgeEnvironment() map[string]string {
	e := map[string]string{}
	for _, k := range []string{"CLAUDE_PROJECT_DIR", "CLAUDE_CODE_SESSION_ID", "CLAUDE_ENV_FILE", "CLAUDE_PLUGIN_ROOT", "CLAUDE_PLUGIN_DATA"} {
		if v := os.Getenv(k); v != "" {
			e[k] = v
		}
	}
	return e
}
func bridgeDispatch(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("dispatch expects kind and specification")
	}
	c, e := sessionConfig()
	if e != nil {
		return e
	}
	var spec map[string]any
	if e = decode(args[1], &spec); e != nil {
		return e
	}
	cwd, _ := os.Getwd()
	result, e := bridgeExec(c, ExecRequest{Kind: args[0], Cwd: cwd, Spec: spec, Env: bridgeEnvironment()}, os.Stdin, os.Stdout, os.Stderr)
	if e != nil {
		return e
	}
	if result.Code != 0 {
		return &exitError{result.Code}
	}
	return nil
}
func transportSFTP() error {
	c, e := sessionConfig()
	if e != nil {
		return e
	}
	client, e := dialBridge(c)
	if e != nil {
		return e
	}
	defer client.Close()
	if c.ConfigSnapshot && !c.FreshMetadata {
		return serveCachedSFTP(client, c)
	}
	ch, requests, e := client.OpenChannel("session", nil)
	if e != nil {
		return e
	}
	defer ch.Close()
	go func() {
		for r := range requests {
			if r.WantReply {
				r.Reply(false, nil)
			}
		}
	}()
	ok, e := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{"sftp"}))
	if e != nil || !ok {
		return fmt.Errorf("SFTP subsystem: %v", e)
	}
	go func() { io.Copy(ch, os.Stdin); ch.CloseWrite() }()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); io.Copy(os.Stderr, ch.Stderr()) }()
	_, e = io.Copy(os.Stdout, ch)
	wg.Wait()
	return e
}
func validateJSON(r io.Reader) error { var v any; return json.NewDecoder(r).Decode(&v) }
func prettyJSON(v any) []byte        { b, _ := json.MarshalIndent(v, "", "  "); return bytes.TrimSpace(b) }
