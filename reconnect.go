package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	bridgeLostExitCode = 222
	// Keep the 0.1.11 names so a server-mode parent can supervise a child
	// that updates itself to a newer version mid-session.
	sessionWorkerEnv      = "_REMOTE_CLAUDE_SERVER_ATTACH_WORKER"
	sessionRetryMarkerEnv = "_REMOTE_CLAUDE_SERVER_RETRY_MARKER"
	sessionRetryNumberEnv = "_REMOTE_CLAUDE_SESSION_RETRY_NUMBER"
	serverReattachOnlyEnv = "_REMOTE_CLAUDE_SERVER_REATTACH_ONLY"
	ordinaryContinueEnv   = "_REMOTE_CLAUDE_ORDINARY_CONTINUE"
)

// Each interactive session runs in a child process. The SSH package can leave a goroutine
// blocked reading terminal input when a connection dies, so reusing the same
// process could let an old attach steal keystrokes from the new one.
func superviseInteractiveSession(server bool) error {
	path, err := os.Executable()
	if err != nil {
		return err
	}
	marker, err := os.CreateTemp("", "remote-claude-reattach-*")
	if err != nil {
		return err
	}
	defer os.Remove(marker.Name())
	defer marker.Close()
	return retryInteractiveSession(marker, func(attempt int) error {
		cmd := exec.Command(path, os.Args[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Env = sessionAttemptEnv(marker.Name(), attempt, server)
		return cmd.Run()
	}, time.Sleep, server)
}

func sessionAttemptEnv(marker string, attempt int, server bool) []string {
	reattachOnly, continueOrdinary := "0", "0"
	if attempt > 0 {
		if server {
			reattachOnly = "1"
		} else {
			continueOrdinary = "1"
		}
	}
	return append(os.Environ(), sessionWorkerEnv+"=1", sessionRetryMarkerEnv+"="+marker,
		sessionRetryNumberEnv+"="+strconv.Itoa(attempt), serverReattachOnlyEnv+"="+reattachOnly,
		ordinaryContinueEnv+"="+continueOrdinary)
}

// A session that ran this long was healthy; its drop starts a fresh backoff.
const healthySession = 30 * time.Second

func retryInteractiveSession(marker *os.File, run func(attempt int) error, wait func(time.Duration), server bool) error {
	failures := 0
	for attempt := 0; ; attempt++ {
		if err := marker.Truncate(0); err != nil {
			return err
		}
		started := time.Now()
		err := run(attempt)
		if time.Since(started) >= healthySession {
			failures = 0
		}
		status, readErr := os.ReadFile(marker.Name())
		if readErr != nil {
			return readErr
		}
		if string(status) != "retry" {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return &exitError{exit.ExitCode()}
			}
			return err
		}
		// Back off while offline, but keep trying until Ctrl+C. Server
		// reattaches never start a new job or rebuild its container.
		delay := serverRetryDelay(failures)
		failures++
		if server {
			fmt.Fprintf(os.Stderr, "\nSSH connection lost; reattaching to the same server job in %s (Ctrl+C to stop).\n", delay)
		} else {
			fmt.Fprintf(os.Stderr, "\nSSH connection lost; reattaching to the running Claude session in %s (Ctrl+C to stop).\n", delay)
		}
		wait(delay)
	}
}

func retryableSSHTransport(err error) bool {
	var missing *ssh.ExitMissingError
	var netErr *net.OpError
	return errors.As(err, &missing) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.As(err, &netErr)
}

// The marker is written only by a worker that saw a transport failure. The
// supervisor reads it after the worker exits, avoiding any ambiguity with a
// legitimate remote command's exit status.
func markSessionRetry(err error, ordinary bool) error {
	if err == nil {
		return nil
	}
	var remoteExit *exitError
	bridgeLost := ordinary && errors.As(err, &remoteExit) && remoteExit.Code == bridgeLostExitCode
	activeAfterDrop := ordinary && os.Getenv(ordinaryContinueEnv) == "1" && strings.Contains(err.Error(), "project already connected; close its other session first")
	if activeAfterDrop {
		attempt, _ := strconv.Atoi(os.Getenv(sessionRetryNumberEnv))
		activeAfterDrop = attempt <= 4
	}
	if !retryableSSHTransport(err) && !bridgeLost && !activeAfterDrop {
		return err
	}
	marker := os.Getenv(sessionRetryMarkerEnv)
	if marker == "" {
		return err
	}
	if writeErr := os.WriteFile(marker, []byte("retry"), 0600); writeErr != nil {
		return fmt.Errorf("mark reconnect after %v: %w", err, writeErr)
	}
	return nil
}

// Keep the retry delay bounded even when a link is down for days.
func serverRetryDelay(attempt int) time.Duration {
	shift := min(max(attempt, 0), 4)
	return time.Second * time.Duration(1<<shift)
}
