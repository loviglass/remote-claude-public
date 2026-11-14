# Architecture

This document describes the implemented behavior of Remote Claude 0.2.0. It is a technical reference, not a compatibility promise for every Claude Code extension. The adapter is pinned to Claude Code 2.1.292.

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
8. Sends a versioned `SessionConfig` to `server prepare`.
9. Mounts the project with SSHFS and starts Claude through a separate terminal connection.

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

The SFTP process owns an authenticated workstation connection. A private Unix socket with mode `0600` provides a local SSH endpoint that reuses this connection for shell, Git, hooks and MCP channels. This avoids repeated SSH handshakes and their latency.

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

## 5. Filesystem and read behavior

The workstation SFTP implementation is rooted with `os.Root` and normalizes traversal. Supported operations include:

- open/read/write and streaming;
- list, lstat, readlink and setstat;
- rename and POSIX rename;
- remove/rmdir and mkdir;
- symlink and hard link.

Regular files up to 256 KiB can be returned in one bulk-read RPC. At most eight such snapshots are held concurrently per bridge. Larger files and writes use streaming SFTP.

Ordinary file contents are not cached between opens. This is important when a local editor changes a file while Claude is working. The caches are instead focused on metadata and discovery:

| Mechanism | Behavior |
|---|---|
| SSHFS metadata cache | Approximately one second; disabled by `--fresh-metadata` |
| Negative lookup cache | Missing paths remembered for at most one second, bounded at 4096 entries; invalidated by writes/commands |
| Configuration snapshot | Batched in memory, background revalidation after one second and mandatory refresh when stale beyond five seconds |
| Small-file reads | Up to 256 KiB, at most eight concurrent snapshots; not a cross-open content cache |

The configuration snapshot has explicit limits: 32 MiB of source content, 10,000 entries and 64 MiB of encoded data. Exceeding a limit produces an error rather than an incomplete silent snapshot. `--fresh-metadata` is the compatibility fallback.

Root listings remain live. Git snapshot data includes root/`.git` metadata, `HEAD`, `config`, refs and `packed-refs`; `.git/index` and Git objects are not baked into the snapshot.

Local editor events are not forwarded as server inotify events. Visibility therefore depends on cache expiry, file reopening and network delay, usually a few seconds rather than an instant file-watcher notification.

## 6. Search

When `remote-claude-rg` is installed beside the client, or `rg` is available on the workstation PATH, supported native searches run locally with configuration files and preprocessors disabled. Explicit operands are confined to the exported root and special files are rejected.

Argument rewriting is conservative. Unsupported options, outside-project cwd, help/version requests and symlink-following cases fall back to the server ripgrep at `/opt/remote-claude/rg`.

The adapter rewrites path operands where necessary, not regular expressions, replacements or matched content. Results use project-relative paths. `--server-search` forces the compatibility path.

Release builders bundle ripgrep 15.2.0 and verify pinned upstream archive SHA-256 hashes.

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

Interactive clients run under a supervisor. A child exits with a retry marker after a transport failure; the parent starts a clean child so stale SSH goroutines cannot retain the terminal.

- Local interactive mode continues the selected conversation. Bare `--resume` is not shown a second time after a drop; an explicit session ID is preserved.
- Server mode sets reattach-only behavior and joins the existing tmux session without creating another job or image.
- Delays are 1, 2, 4, 8 and 16 seconds, then capped.
- `Ctrl+C` interrupts the retry loop.
- `--print`, doctor and JSON output are not automatically retried.

The server watchdog checks the bridge every 15 seconds. A dead bridge causes Claude to terminate and allows a stale FUSE mount to be aborted. A newer session can reclaim a project lock only after the old bridge is shown unreachable.

Clean, unlocked agent worktrees under `.claude/worktrees` that have been idle for more than 24 hours are pruned at interactive startup to reduce filesystem stalls. Branches are retained.

## 9. Server jobs

For `remote-claude --server PROJECT`, the client sends the repository origin, current commit, branch and Claude arguments. It does not upload the working tree.

The server:

1. Verifies that the commit exists on `origin`.
2. Creates or reuses `~/server-jobs/<name>-<origin-hash>`.
3. Creates `claude/<branch>-<UTC timestamp>` for a new task.
4. Refuses to overwrite dirty leftovers for a new non-resume task.
5. Starts a per-user tmux session and a Docker container.
6. Reuses the checkout and server branch for `--server --resume`.

There is a hard limit of two tmux jobs per account. A separate read-only root helper enumerates running labeled containers to warn about another account working on the same repository. This check is non-atomic, sees running containers only and is bypassed by `--force-new`.

### Containers

The image comes from `.remote-claude/Dockerfile` (with only that directory as build context) or `remote-claude-job-base:latest`. The default image is Ubuntu 24.04 with common development tools.

Runtime properties include:

- developer UID/GID;
- all capabilities dropped and `no-new-privileges`;
- host networking;
- developer home mounted;
- account and server binaries available without installing them in the image;
- no Docker socket;
- PID limit 1024 and a per-user cgroup parent.

This is not a hostile-user sandbox. Project Dockerfiles execute root build steps, the home directory is mounted and networking is shared with the host.

Claude permissions pre-approve selected status/diff/log/add/commit/push and PR create/view operations, and deny common force-push/merge spellings. These rules are not a complete Git security policy. Use branch protection and least-privilege hosting tokens.

`night` uses an existing absolute server Git checkout and does not prepare a branch. It rejects mounted workstation projects.

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
