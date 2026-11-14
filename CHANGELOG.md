# Changelog

All notable changes are documented here. Release binaries are immutable and tied to the tagged source commit.

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
