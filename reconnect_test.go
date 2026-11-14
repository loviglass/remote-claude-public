package main

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSessionRetryOnlyForTransportFailures(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"missing SSH exit status", &ssh.ExitMissingError{}, true},
		{"closed channel", io.EOF, true},
		{"closed network", net.ErrClosed, true},
		{"dial failure", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("offline")}, true},
		{"remote command failed", &exitError{1}, false},
		{"user stopped terminal", &exitError{130}, false},
		{"local error", errors.New("invalid terminal"), false},
		{"clean exit", nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryableSSHTransport(tt.err); got != tt.want {
				t.Fatalf("retryableSSHTransport(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestSessionRetryMarkerOnlyForTransportFailures(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	t.Setenv(sessionRetryMarkerEnv, marker)
	broken := &ssh.ExitMissingError{}
	if err := markSessionRetry(broken, false); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "retry" {
		t.Fatalf("retry marker = %q, %v", b, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	normal := &exitError{1}
	if got := markSessionRetry(normal, false); got != normal {
		t.Fatalf("normal exit was converted to retry: %v", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("normal exit wrote a retry marker: %v", err)
	}
}

func TestSessionReconnectUsesFreshAttempt(t *testing.T) {
	marker, err := os.CreateTemp(t.TempDir(), "marker-*")
	if err != nil {
		t.Fatal(err)
	}
	defer marker.Close()
	var attempts []int
	var waits []time.Duration
	err = retryInteractiveSession(marker, func(attempt int) error {
		attempts = append(attempts, attempt)
		if len(attempts) == 1 {
			return os.WriteFile(marker.Name(), []byte("retry"), 0600)
		}
		if b, err := os.ReadFile(marker.Name()); err != nil || len(b) != 0 {
			t.Fatalf("stale retry marker: %q, %v", b, err)
		}
		return nil
	}, func(delay time.Duration) { waits = append(waits, delay) }, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0] != 0 || attempts[1] != 1 {
		t.Fatalf("retry attempts = %v", attempts)
	}
	if len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("retry waits = %v", waits)
	}
}

func TestRetryModePreservesServerJobsAndContinuesOrdinaryConversation(t *testing.T) {
	value := func(env []string, key string) string {
		var got string
		for _, entry := range env {
			if v, ok := strings.CutPrefix(entry, key+"="); ok {
				got = v
			}
		}
		return got
	}
	for _, tt := range []struct {
		server, retry      bool
		reattach, ordinary string
	}{
		{true, false, "0", "0"},
		{true, true, "1", "0"},
		{false, false, "0", "0"},
		{false, true, "0", "1"},
	} {
		attempt := 0
		if tt.retry {
			attempt = 1
		}
		env := sessionAttemptEnv("marker", attempt, tt.server)
		if got := value(env, serverReattachOnlyEnv); got != tt.reattach {
			t.Errorf("server=%v retry=%v: reattach-only=%q", tt.server, tt.retry, got)
		}
		if got := value(env, ordinaryContinueEnv); got != tt.ordinary {
			t.Errorf("server=%v retry=%v: ordinary-continue=%q", tt.server, tt.retry, got)
		}
	}
}

func TestOrdinaryRetryWaitsBrieflyForPreviousWorkspace(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	t.Setenv(sessionRetryMarkerEnv, marker)
	t.Setenv(ordinaryContinueEnv, "1")
	active := errors.New("exit status 1: remote-claude: project already connected; close its other session first")
	t.Setenv(sessionRetryNumberEnv, "1")
	if err := markSessionRetry(active, true); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "retry" {
		t.Fatalf("workspace retry marker = %q, %v", b, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sessionRetryNumberEnv, "5")
	if got := markSessionRetry(active, true); got != active {
		t.Fatalf("workspace conflict retried indefinitely: %v", got)
	}
}

func TestOrdinaryRetriesWhenLocalBridgeDisappears(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	t.Setenv(sessionRetryMarkerEnv, marker)
	lost := &exitError{bridgeLostExitCode}
	if err := markSessionRetry(lost, true); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "retry" {
		t.Fatalf("bridge loss retry marker = %q, %v", b, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if got := markSessionRetry(lost, false); got != lost {
		t.Fatalf("server job exit was retried as ordinary bridge loss: %v", got)
	}
}
