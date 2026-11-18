//go:build !windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func configureProcess(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func enableTerminalOutput() func() { return func() {} }
func stopProcess(c *exec.Cmd) {
	if c.Process != nil {
		syscall.Kill(-c.Process.Pid, syscall.SIGTERM)
	}
}
func killProcess(c *exec.Cmd) {
	if c.Process != nil {
		syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}
func replaceFile(a, b string) error { return os.Rename(a, b) }
func restartSelf() error {
	path, e := os.Executable()
	if e != nil {
		return e
	}
	return syscall.Exec(path, os.Args, os.Environ())
}

func exitSignal(e *exec.ExitError) int {
	if st, ok := e.Sys().(syscall.WaitStatus); ok && st.Signaled() {
		return int(st.Signal())
	}
	return 2
}

func resizeSignals() <-chan os.Signal {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGWINCH)
	return c
}
