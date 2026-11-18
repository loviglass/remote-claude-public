package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const version = "0.3.0"
const protocolVersion = 2

// Release builds replace this with the full source commit using -ldflags -X.
var buildCommit = "unknown"
var serverBinary = "/opt/remote-claude/remote-claude"

const claudeBinary = "/opt/remote-claude/claude"

func main() {
	var err error
	args := os.Args[1:]
	if filepath.Base(os.Args[0]) == "shell" {
		finish(bridgeShell(args))
		return
	}
	if filepath.Base(os.Args[0]) == "rg" {
		finish(bridgeSearch(args))
		return
	}
	if filepath.Base(os.Args[0]) == "git" {
		finish(bridgeGit(args))
		return
	}
	if filepath.Base(os.Args[0]) == "pwsh" {
		err = bridgePowerShell(args)
		finish(err)
		return
	}
	if len(args) == 0 {
		args = []string{"."}
	}
	switch args[0] {
	case "version", "--version":
		commit := buildCommit
		if len(commit) > 12 {
			commit = commit[:12]
		}
		fmt.Println("remote-claude", version, "commit", commit)
		return
	case "setup":
		err = setup(args[1:])
	case "server":
		err = serverRun(args[1:])
	case "sftp":
		err = transportSFTP()
	case "search":
		err = bridgeSearch(args[1:])
	case "shell":
		err = bridgeShell(args[1:])
	case "dispatch":
		err = bridgeDispatch(args[1:])
	case "enroll":
		err = enrollServer()
	case "night":
		err = night(args[1:])
	case "ssh":
		err = remoteSSH(args[1:])
	case "--help", "help":
		fmt.Println("remote-claude setup [--profile NAME] [--proxy [--proxy-host HOST] [--proxy-port PORT]] invitation.json\nremote-claude [--profile NAME] [--proxy [--proxy-host HOST] [--proxy-port PORT]] [--resume[=ID] | --continue] [--check] [--print PROMPT] PROJECT [-- Claude arguments]\nremote-claude --server [--profile NAME] [--proxy [--proxy-host HOST] [--proxy-port PORT]] [--resume[=ID] | --continue] [--force-new] PROJECT [-- Claude arguments]\nremote-claude ssh [--profile NAME] [--proxy [--proxy-host HOST] [--proxy-port PORT]]\nremote-claude night [--profile NAME] [--proxy [--proxy-host HOST] [--proxy-port PORT]] VPS_REPOSITORY_PATH [Claude arguments]\nremote-claude version")
		return
	default:
		err = connect(args)
	}
	finish(err)
}
func finish(err error) {
	if err == errRestart {
		err = restartSelf()
	}
	if err != nil {
		var e *exitError
		if errors.As(err, &e) {
			os.Exit(e.Code)
		}
		fmt.Fprintln(os.Stderr, "remote-claude:", err)
		os.Exit(1)
	}
}

type exitError struct{ Code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit %d", e.Code) }
func readJSON(path string, out any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, out)
}
func writeJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".rc-write-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(append(b, '\n'))
	}
	if ce := f.Close(); e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return replaceFile(f.Name(), path)
}
func configDir() string {
	d, e := os.UserConfigDir()
	if e != nil {
		d = "."
	}
	return filepath.Join(d, "remote-claude")
}
