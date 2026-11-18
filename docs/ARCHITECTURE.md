# Architecture

This document describes the implemented behavior of Remote Claude 0.3.0. It is a technical reference, not a compatibility promise for every Claude Code extension. The adapter is pinned to Claude Code 2.1.292.

## 1. Processes and trust domains

Remote Claude uses one Go module and one executable in several roles:

| Invocation | Role |
|---|---|
| `remote-claude [flags] PROJECT` | Workstation client and interactive supervisor |
| `remote-claude server ...` | Server-side session and job manager |
| `remote-claude sftp` | Private SFTP transport |
| `remote-claude shell` | Bash command adapter |
| `remote-claude dispatch ...` | Hook and MCP dispatcher |
| `remote-claude search` / `rg` alias | Search adapter |
| `remote-claude enroll` | One-time public-key enrollment |

The workstation and server trust domains are deliberately different:

- Claude Code and its conversation state run on the server.
- In local mode, the project and command execution belong to the workstation user.
- The server administrator controls the pinned Claude binary, server binary and update manifest.
- A managed Linux user can read its own server-side OAuth copy and server jobs. Separate users are operational isolation, not a boundary against root or against all other trusted team members.

## 2. Connection establishment

For a local interactive session the client:

1. Loads a profile from the OS user configuration directory.
2. Reads the workstation Ed25519 identity and requires the profile's SHA-256 server host fingerprint.
3. Starts a terminal SSH handshake and a data SSH handshake in parallel.
4. Checks the server update manifest over the pinned data connection.
5. Requests a stable per-project server path based on the identity and local project path.
6. Starts a temporary loopback SSH/SFTP server rooted at the local project.
7. Opens reverse SSH forwarding from the server to that loopback listener.
8. Sends a versioned `SessionConfig` to `server prepare`, which starts or reuses the project's session host and binds it to the new bridge.
9. Waits until the replica and the workstation agree, then attaches the separate terminal connection to Claude's PTY in the host.

The server path of the replica is cached per profile, and the update check overlaps session preparation, so a reconnect costs two SSH handshakes, one preparation call and the bridge handshake.

Only Ed25519 server host keys are accepted. The bridge itself has a random per-session secret and an ephemeral host key. The workstation listener uses `127.0.0.1:0`; no public listener is created.

The server-side SSHFS options include short one-second attribute/entry/directory cache timeouts. `--fresh-metadata` disables these caches.

## 3. Enrollment

An invitation contains:

- SSH host and optional port;
- managed Linux username;
- SHA-256 fingerprint of the server's public Ed25519 host key;
- one-time bootstrap private key.

The bootstrap key is authorized only for the forced `remote-claude enroll` command. During `setup`, the client generates a different Ed25519 key, sends its public key and never stores the bootstrap key in the final profile.

The server accepts one plain Ed25519 public key without extra options or trailing data. Under a pending-enrollment lock it atomically replaces `authorized_keys`, removing the bootstrap entry. Subsequent user keys have `no-agent-forwarding,no-X11-forwarding`; enrollment does not create a restricted shell.

A lost enrollment acknowledgement can be recovered: if the newly generated key already authenticates, setup writes the profile without reusing the bootstrap credential.

## 4. Command transport

Claude receives `CLAUDE_CODE_SHELL_PREFIX`, which routes supported shell invocations through the adapter. The adapter unwraps Claude's shell wrapper, translates the mounted server project path to the local path and sends a versioned execution request.

The main execution paths are:

- **Bash/zsh:** the workstation `$SHELL` when supported, otherwise `/bin/bash`.
- **Windows PowerShell:** Claude's PowerShell tool is enabled and routed through the `pwsh` adapter. The pinned adapter does not support PowerShell `-File`.
- **Git:** a PATH shim redirects project Git commands to the workstation. Server-side Git outside the exported project remains server-side.
- **Hooks/MCP:** projected configuration invokes `remote-claude dispatch` with the original specification encoded in the request.

The session host owns an authenticated workstation connection. A private Unix socket with mode `0600` provides a local SSH endpoint that reuses it for shell, Git, hooks and MCP. Each command is one `rc-exec` channel whose open request carries the command, so a command costs one round trip plus its own run time; the synchronisation barriers (§5) ride along.

Execution behavior:

- Environment starts with the workstation environment, then project/local settings `env` and request-specific values.
- `CLAUDE_PROJECT_DIR` points to the local project root; `CLAUDECODE=1` is set.
- Executables are resolved using the effective project/MCP `PATH`, so local virtual environments can work.
- stdout and stderr stream over the channel; exit status and structured result are returned separately.
- Working-directory changes are written back to Claude's cwd file. Leaving the exported root as the persistent cwd is rejected.
- Request IDs deduplicate dispatch bookkeeping. Cancellation kills the corresponding command, not every command on the bridge.
- stdin uses an explicit pipe and bounded wait behavior so a process can finish even if Claude keeps stdin open.

If transport disappears after a command was accepted, its outcome is unknown. The client reports that it was not retried. Automatic replay would risk duplicate builds, writes, payments or Git operations.

**Commands are not sandboxed.** They inherit the workstation user's normal ability to access the rest of the system. Rooting applies to the file transport and validated search operands, not arbitrary shell commands.

## 5. Replica synchronisation

Claude works on a replica of the project in `~/.local/share/remote-claude/projects/<id>/worktree` on the server's disk. The workstation remains the owner of the project.

What is replicated:

| Workstation path | Replica |
|---|---|
| Files and directories Git does not ignore (including untracked ones) | Real files, kept in sync both ways |
| Git-ignored paths, `.git`, files over 16 MiB | Symlinks to the same path in the live SSHFS view (`projects/<id>/live`) |
| Sockets, FIFOs, devices | Not shown |

Ignore rules are the project's `.gitignore` files, `info/exclude`, the global excludes file and unanchored rules of parent directories. Outside Git a small default list (`node_modules`, virtual environments, Python caches) applies. A rule mismatch only moves a path between the replica and the live view; both show the workstation's content.

Protocol:

- Each side keeps the last version both agree on (the base): kind, permission bits, content hash and the stat of each copy. The server persists it, so a later session transfers only differences.
- A side detects its changes by scanning (stat comparison, then hashing changed files) and sends them with the base hash they replace. File events (inotify, ReadDirectoryChangesW) trigger scans promptly; periodic scans are the safety net and the macOS mechanism.
- Each direction is one ordered gob stream with sequence numbers over an SSH channel of the bridge. Data messages are bounded in flight; control messages are not, so the two receive loops cannot deadlock.
- Files are replaced atomically (temporary file and rename in the same directory, through `os.Root`), so neither Claude nor a local editor reads a half-written file and no path can escape the project.
- Claude configuration with executable entries is projected on its way to the replica and unprojected on its way back (§7); the base tracks both forms.

Command barriers:

1. Before a workstation command, the server scans the replica and sends Claude's changes; the request carries the last sequence number, and the workstation waits until it has applied it.
2. After a command that can change files, the workstation scans and returns its last sequence number with the result; the server applies the changes before returning the result to Claude.
3. Read-only Git commands, searches and MCP starts skip the second barrier.

Conflicts: the workstation always wins. If Claude's unsynchronised edit loses, it is saved under `projects/<id>/conflicts/` and the next command's output tells Claude. Simultaneous identical edits and lost acknowledgements resolve without a conflict. The replication window between a workstation check and an atomic replacement is closed by re-checking just before the rename.

Reconnects: Claude keeps working on the replica while the workstation is away. The next connection transfers the base, the workstation reports its changes first, and then Claude's offline edits go to the workstation. A replica without persisted state is treated as disposable: leftovers are removed instead of resurrected.

The live SSHFS view keeps the metadata cache, `.git` snapshot and single-request small-file reads of earlier versions. It is aborted when the bridge disappears, so reads of ignored paths fail fast instead of hanging.

## 6. Search

Searches without `--no-ignore*`, `-u` or `--follow` run with the server's ripgrep on the replica: the replica holds exactly the files such a search can see. Others run with workstation ripgrep (`remote-claude-rg` beside the client, or `rg` on the PATH), with configuration files and preprocessors disabled, explicit operands confined to the project and special files rejected. Without workstation ripgrep they run on the server with `--follow` through the live view.

## 7. Project configuration, hooks and MCP

The mounted repository provides native discovery of:

- `CLAUDE.md` and nested instructions;
- `.claude/rules`;
- project and local settings;
- skills, commands, agents and output styles.

Executable entries need a transport adapter. On read, configuration projection replaces them with `remote-claude dispatch` specifications; on write, the original definitions are restored. This applies to JSON configuration and supported YAML frontmatter in skills/agents.

Consequently, a `Read` of executable configuration inside Claude can show an adapter even though a local editor shows the original file.

### Hooks

- Command hooks run on the workstation and preserve JSON I/O and exit behavior.
- Exec-form hooks are dispatched before ordinary shell-prefix forwarding.
- HTTP hooks are sent from the workstation with expanded headers/environment and a 60-second timeout; non-2xx responses are errors.
- `CLAUDE_ENV_FILE` points to a workstation temporary file. Subsequent Bash commands source it.

### MCP

- stdio servers start on the workstation with local cwd, environment, argument expansion and PATH resolution.
- HTTP/SSE servers are reached through loopback proxy listeners carried over reverse SSH. Connections therefore originate on the workstation and can use its localhost or VPN.
- HTTP headers are expanded locally and SSE endpoints are preserved.

Personal workstation `~/.claude` is not imported. Third-party MCP OAuth flows, LSP-heavy plugins, plugin-native binaries/JavaScript modes and Claude's native OS sandbox are not adapted.

## 8. Reconnect and lifecycle

A session host (`remote-claude server host <id>`) owns each project on the server. It starts on the first `prepare`, runs in its own session and keeps:

- the replica and its synchronisation state;
- the bridge to the current workstation connection, with keepalives every 10 seconds;
- the command executor socket, which waits up to a minute for a workstation that is reconnecting;
- Claude's PTY. Output goes to the attached terminal; without one it is dropped and the next attach makes Claude redraw.

Interactive clients run under a supervisor. A child exits with a retry marker after a transport failure; the parent starts a clean child, which reconnects, rebinds the host and reattaches. Delays are 1, 2, 4, 8 and 16 seconds and restart after a session that ran for 30 seconds. `Ctrl+C` interrupts the retry loop. `--print`, doctor and JSON output are not retried. The supervisor's private environment is removed from commands, so a client started by a command is independent.

The host stops Claude after 12 hours without an attached terminal, and exits when Claude has exited and its terminal has seen it, or after two idle minutes when it never ran Claude (print and check runs).

Clean, unlocked agent worktrees under `.claude/worktrees` that have been idle for more than 24 hours are pruned at interactive startup. Branches are retained.

## 9. Server jobs

For `remote-claude --server PROJECT`, the client records the working tree as a commit on top of `HEAD`, using a copy of the index so the developer's index, worktree and branches stay untouched. A Git bundle with that commit and any commits `origin` lacks is uploaded with `server job-upload`.

The server:

1. Creates or reuses `~/server-jobs/<name>-<origin-hash>` and fetches `origin`.
2. Unpacks the bundle and verifies that the workstation's `HEAD` is available.
3. Creates `claude/<branch>-<UTC timestamp>` at that commit and checks out the snapshot's files with the index at `HEAD`: the work in progress shows as uncommitted changes.
4. Refuses to overwrite dirty leftovers for a new non-resume task.
5. Starts a per-user tmux session under the developer's account. With `.remote-claude/Dockerfile` the job runs in a container instead.
6. Reuses the checkout and server branch for `--server --resume`.

Jobs run unattended, so they allow every tool (`acceptEdits` plus `Bash`, `Edit`, `Write`, `NotebookEdit`, `WebFetch`, `WebSearch`). Deny rules, which take precedence in every permission mode, block `gh pr merge`, merge API calls, force pushes, mirror pushes and remote branch deletion. These rules are not a complete Git security policy. Use branch protection and least-privilege hosting tokens.

There is a hard limit of two tmux jobs per account. A separate read-only root helper enumerates running labeled containers to warn about another account working on the same repository. This check is non-atomic and is bypassed by `--force-new`.

### Containers

With `.remote-claude/Dockerfile` (built with only that directory as context) the job runs in a container with:

- developer UID/GID;
- all capabilities dropped and `no-new-privileges`;
- host networking;
- developer home mounted;
- account and server binaries available without installing them in the image;
- no Docker socket;
- PID limit 1024 and a per-user cgroup parent.

This is not a hostile-user sandbox. Project Dockerfiles execute root build steps, the home directory is mounted and networking is shared with the host.

`night` uses an existing absolute server Git checkout and does not prepare a branch. It rejects workstation projects.

## 10. Resource control

Resource controls are opt-in and administered by `configure-resources.py`.

New local managed sessions are placed in a user systemd scope. The default aggregate per-user policy is 1536 MiB `MemoryHigh`, 2048 MiB `MemoryMax`, 150% `CPUQuota` and 256 tasks.

Server jobs share `remoteclaude.slice`: low CPU/I/O weights, `MemoryMax=50%` and `TasksMax=4096`. A governor recalculates soft memory limits every five seconds and can stop the heaviest user's jobs under severe pressure. Existing sessions are not moved into new cgroups retroactively.

Disk usage of server checkouts is not limited by this mechanism.

## 11. Authorization synchronization

Managed accounts can share one Claude OAuth login. A single systemd timer avoids concurrent refresh-token use:

1. Discover only accounts with `.remote-claude-managed`.
2. Open home/credential files without following symlinks and validate ownership/type.
3. Select the newest usable login by expiry.
4. Refresh approximately two hours before expiry, before Claude's own refresh threshold.
5. Run the HTTP refresh as the source account UID/GID so its egress rules apply.
6. Replace only `claudeAiOauth` and atomically distribute it to managed accounts.
7. Log token fingerprints rather than token values.

Rejected refresh tokens are not retried indefinitely; network failures use a five-minute retry delay. State is stored in `/var/lib/remote-claude-auth/`.

Linux account count does not multiply subscription quota. Managed users must be trusted with the shared server-side authorization.

## 12. Updates and release provenance

The client reads `/opt/remote-claude/clients/manifest.json` through the pinned server connection. A candidate must have a strictly newer numeric version and a valid executable SHA-256 for the current platform. Development manifests are rejected.

Files are downloaded through the same SSH connection. The executable is installed last through a temporary file and atomic replacement; Windows first renames the running executable. A failed update leaves the old client in place and does not prevent a compatible connection.

Release builds:

- require a clean worktree and exact `v<version>` tag at `HEAD`;
- require a version newer than previous release tags;
- inject the full source commit into the executable and manifest;
- build six OS/architecture targets from scratch;
- allowlist public archive members;
- compare archive and served binary hashes;
- reject development artifacts for publication and self-update.

The update manifest is not independently signed. Its trust anchor is the pinned SSH server. GitHub Release archives additionally have published SHA-256 checksums, which users should verify after download.
