# Changelog

All notable changes are documented here. Release binaries are immutable and tied to the tagged source commit.

## 0.3.1 — no copies outside projects

- The replica is built only for a Git project of reasonable size (up to 50,000 non-ignored files and 1 GiB). The home directory, a filesystem root, directories outside Git and larger trees are not copied: their entries are links into the live view and files are read over the network, as before 0.3.0. `--live` forces this mode.
- Server-side searches without a path no longer read stdin instead of the directory; replica searches skip Git's ignore rules, which the replica already applies.

## 0.3.0 — replica and persistent sessions

### Interactive mode

- Claude's file tools work on a replica of the project on the server's disk instead of an SSHFS mount: reads, edits, directory walks and searches no longer wait for the network. Git-ignored paths, `.git` and files over 16 MiB stay on the workstation behind symlinks into a live SSHFS view.
- Two-way synchronisation with command barriers: a workstation command always sees Claude's latest edits, and Claude always sees the command's file changes. The workstation wins conflicts; Claude's losing version is kept and reported.
- A persistent session host keeps Claude running across SSH drops; the client reattaches to the same process instead of restarting it with `--continue`.
- Commands, Git calls and hooks cost one round trip instead of two (`rc-exec` channel).
- Measured with a 160 ms round trip: a scripted session (100 file reads, tree walk, edit, five commands, Git, two searches) dropped from 145 s to 5.7 s.
- Terminal resizes propagate immediately (SIGWINCH); `TERM`, `COLORTERM`, `TERM_PROGRAM` and a UTF-8 locale reach Claude.
- Faster start: the server location is cached and the update check overlaps preparation.

### Server mode

- Starts from the workstation's exact working tree: unpushed commits and uncommitted files travel as a Git bundle; the developer's repository is not modified.
- Runs under the developer's account by default, with access to the server, its services and databases; containers only for projects with `.remote-claude/Dockerfile`.
- Runs autonomously: all tools are allowed; deny rules block PR merges, force pushes and remote branch deletion.

### Fixes

- Reconnect backoff restarts after a healthy session instead of staying at 16 s for the rest of the day.
- A failing command keeps the working directory it changed to, like a local shell; `cd` outside the project no longer turns a successful command into an error.
- A command killed by a signal reports 128 + signal (for example 137) instead of 130.
- The request de-duplication set no longer grows for the whole session.
- Files created through the live view honour the umask instead of always being 0600.
- `x/../.mcp.json`-style paths can no longer bypass configuration projection.
- Commands no longer inherit the client supervisor's private environment, so a client started by a command does not act as the session's worker.
- Idle agent worktrees left locked by a killed Claude agent are pruned too; other locks are still honoured.

### Upgrading

- Sessions started by an earlier release keep working while the server binary is replaced: new command adapters fall back to the earlier executor protocol, and a project open in such a session is not taken over until that session ends.
- Clients update themselves from the server manifest at their next connection.

## 0.2.0 — public self-hosted release

### Publication and configuration

- Removed embedded production addresses, SSH fingerprints and the one-off automatic server relocation.
- Replaced owner-specific provisioning defaults with required `--host`/`REMOTE_CLAUDE_HOST` configuration and generic Linux usernames.
- Added optional managed UID ranges for administrators who implement egress routing by UID.
- Generalized Happ-specific wording to standard HTTP CONNECT proxy configuration without changing the CLI flags.
- Renamed generated invitations to `remote-claude-NAME.json` and updated the Windows installer discovery flow.
- Added `install-server.py` for installation without creating users or copying root credentials.
- Removed obsolete owner-specific provisioning and verification scripts/reports.

### Release integrity

- Added source-commit provenance to `remote-claude version` and the update manifest.
- Added strict clean-worktree, exact-tag and version-increase gates.
- Added deterministic archive metadata, clean staging and independent archive/hash/member verification.
- Added explicit development builds; development manifests cannot trigger client updates.
- Added CI for Go race tests, vet, Python tests and full-history credential scanning.
- Added tag-driven GitHub Release publication for six platform archives and `SHA256SUMS`.

### Documentation and licensing

- Added MIT licensing, a security policy and a release guide.
- Rewrote the public README and Russian user/administrator guides around generic self-hosting.
- Documented architecture, caches, hooks/MCP adaptation, reconnect behavior, resource controls, operational benefits and limitations without promising immunity from provider restrictions.

Earlier versions were internal releases and are intentionally not part of the new public repository history.
