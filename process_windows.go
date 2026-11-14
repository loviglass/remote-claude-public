//go:build windows

package main

import (
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"strconv"
)

func configureProcess(c *exec.Cmd) {}
func enableTerminalOutput() func() {
	h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return func() {}
	}
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return func() {}
	}
	windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	return func() { windows.SetConsoleMode(h, mode) }
}
func stopProcess(c *exec.Cmd) { killProcess(c) }
func killProcess(c *exec.Cmd) {
	if c.Process != nil {
		exec.Command("taskkill", "/PID", strconv.Itoa(c.Process.Pid), "/T", "/F").Run()
	}
}
func replaceFile(a, b string) error {
	x, e := windows.UTF16PtrFromString(a)
	if e != nil {
		return e
	}
	y, e := windows.UTF16PtrFromString(b)
	if e != nil {
		return e
	}
	return windows.MoveFileEx(x, y, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
func restartSelf() error {
	path, e := os.Executable()
	if e != nil {
		return e
	}
	c := exec.Command(path, os.Args[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	e = c.Run()
	if x, ok := e.(*exec.ExitError); ok {
		return &exitError{x.ExitCode()}
	}
	return e
}
