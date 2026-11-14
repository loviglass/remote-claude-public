package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// Limits are opt-in for administrators; they are enabled by configure-resources.py.
// New scopes share one budget per user, including descendants. Server-mode
// jobs are not scoped here: they run in the pool of serverjob.go.
func constrainClaude(cmd *exec.Cmd, id string) (func(), error) {
	if _, err := os.Stat(filepath.Join(stateDir(), "resources.enabled")); os.IsNotExist(err) {
		return func() {}, nil
	} else if err != nil {
		return nil, err
	}
	runtimeDir := "/run/user/" + strconv.Itoa(os.Getuid())
	if _, err := os.Stat(filepath.Join(runtimeDir, "bus")); err != nil {
		return nil, fmt.Errorf("resource limits enabled but user systemd is unavailable: %w", err)
	}
	unit := "remote-claude-" + id + ".scope"
	slice := "remote-claude.slice"
	env := append(cmd.Env, "XDG_RUNTIME_DIR="+runtimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path="+runtimeDir+"/bus")
	args := []string{"--user", "--scope", "--quiet", "--unit=" + unit, "--slice=" + slice, "--"}
	args = append(args, cmd.Path)
	args = append(args, cmd.Args[1:]...)
	cmd.Path = "/usr/bin/systemd-run"
	cmd.Args = append([]string{cmd.Path}, args...)
	cmd.Env = env
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stop := exec.CommandContext(ctx, "/usr/bin/systemctl", "--user", "stop", unit)
		stop.Env = env
		stop.Run() // An already collected scope needs no cleanup.
	}, nil
}
