package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Forward both Claude's real PowerShell parser and its execution process. The
// permission parser therefore runs against the same PowerShell as the command.
var psCwdPattern = regexp.MustCompile(`(?m); \(Get-Location\)\.Path \| Out-File -FilePath '(/tmp/claude-[^']+/claude-pwd-ps-[^']+)' -Encoding utf8 -NoNewline`)

func bridgePowerShell(args []string) error {
	c, e := sessionConfig()
	if e != nil {
		return e
	}
	cwd, _ := os.Getwd()
	q := ExecRequest{Kind: "powershell", Cwd: cwd, Args: args, Env: bridgeEnvironment()}
	cwdFile := ""
	for i, a := range args {
		if strings.EqualFold(a, "-Command") && i+1 < len(args) {
			m := psCwdPattern.FindStringSubmatch(args[i+1])
			if len(m) > 1 {
				cwdFile = m[1]
				q.Args = append([]string(nil), args...)
				q.Args[i+1] = strings.ReplaceAll(args[i+1], psQuote(cwdFile), "$env:REMOTE_CLAUDE_CWD_FILE")
				q.Spec = map[string]any{"track_cwd": true}
			}
		}
		if strings.EqualFold(a, "-File") {
			return fmt.Errorf("unsupported PowerShell -File wrapper in pinned Claude version")
		}
	}
	r, e := bridgeExec(c, q, os.Stdin, os.Stdout, os.Stderr)
	if e != nil {
		return e
	}
	if cwdFile != "" {
		if e = writeRemoteCwd(c, cwdFile, r.Cwd); e != nil {
			return e
		}
	}
	if r.Code != 0 {
		return &exitError{r.Code}
	}
	return nil
}

func writeRemoteCwd(c SessionConfig, file, cwd string) error {
	p := strings.ReplaceAll(cwd, "\\", "/")
	root := strings.ReplaceAll(c.LocalRoot, "\\", "/")
	if p == root || strings.HasPrefix(p, root+"/") {
		p = c.RemoteRoot + strings.TrimPrefix(p, root)
	} else {
		// The command itself succeeded; only the persistent directory stays.
		return fmt.Errorf("the working directory stays in the project; %s is outside it", p)
	}
	return os.WriteFile(file, []byte(p+"\n"), 0600)
}
