package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// A session host owns one project on the server: the replica, the connection
// to the workstation, the command executor and Claude's terminal. It survives
// SSH disconnects, so a dropped link costs a reattach instead of a restart and
// Claude keeps its turn, its context and its unsaved work.
type hostPaths struct {
	dir, replica, live, sock, lock, config, bridge, liveConfig, state, stash, exec string
}

func projectHostPaths(id string) hostPaths {
	d := filepath.Join(stateDir(), "projects", id)
	sockets := d
	// Unix socket paths are limited to 108 bytes; a long home directory
	// moves the sockets to a private directory under /tmp.
	if len(filepath.Join(d, "host.sock")) > 100 {
		sockets = filepath.Join(os.TempDir(), fmt.Sprintf("remote-claude-%d", os.Getuid()), id)
	}
	return hostPaths{
		dir: d, replica: filepath.Join(d, "worktree"), live: filepath.Join(d, "live"),
		sock: filepath.Join(sockets, "host.sock"), lock: filepath.Join(d, "host.lock"),
		config: filepath.Join(d, "host.json"), bridge: filepath.Join(d, "bridge.json"),
		liveConfig: filepath.Join(d, "live.json"), state: filepath.Join(d, "sync-state.gob"),
		stash: filepath.Join(d, "conflicts"), exec: filepath.Join(sockets, "exec.sock"),
	}
}

// Claude stops after this long without any attached terminal.
const detachedLimit = 12 * time.Hour

// A host without Claude (print and check runs) leaves soon after its client.
const idleHostLimit = 2 * time.Minute

type hostRequest struct {
	Op   string            `json:"op"`
	Rows int               `json:"rows,omitempty"`
	Cols int               `json:"cols,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	Args []string          `json:"args,omitempty"`
}

type hostReply struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Running bool   `json:"running,omitempty"`
}

type attachParams struct {
	Env  map[string]string `json:"env,omitempty"`
	Args []string          `json:"args,omitempty"`
}
