//go:build !windows

package main

import (
	"os"
	"os/exec"
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
