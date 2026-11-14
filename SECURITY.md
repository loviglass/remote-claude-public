# Security policy

## Reporting a vulnerability

Do not open a public issue for an exploitable vulnerability, a live credential or private infrastructure details. Use the repository owner's private vulnerability-reporting channel (GitHub **Private vulnerability reporting**, when enabled) or another contact listed by the owner.

Include:

- affected version and source commit from `remote-claude version`;
- platform and deployment mode;
- reproduction steps and impact;
- a minimal proof of concept with credentials and private project contents removed.

Do not include real invitations, private keys, OAuth files, access tokens or production addresses. If a credential was exposed, rotate it first and say that rotation occurred.

There is no formal response-time or bounty commitment unless the repository owner states one separately.

## Deployment trust model

Remote Claude is designed for a trusted server administrator and trusted managed users.

- **Server root:** controls Claude, the bridge, update manifest and all managed account data. It is fully trusted.
- **Managed server users:** can use their own shell, read their own OAuth copy and access their own jobs. Additional server privileges are granted only through explicit sudoers rules.
- **Workstation user:** local commands inherit that user's full OS permissions. The bridge is not a command sandbox.
- **Project authors:** project configuration can define hooks, MCP commands and server-mode Dockerfiles. Review untrusted repositories before approving execution.
- **Git hosting:** Claude permission rules are not branch protection. Enforce review and merge restrictions on the hosting service and use least-privilege tokens.

## Transport and enrollment protections

- Server host identity is pinned by an Ed25519 SHA-256 fingerprint.
- Invitations contain a one-time bootstrap private key and must be handled as secrets.
- Enrollment replaces the bootstrap key with a workstation-generated public key and removes the bootstrap credential from the stored profile.
- The temporary workstation listener is loopback-only and reached through reverse SSH.
- Bridge sessions use a random secret and ephemeral host key.
- The project SFTP layer is rooted at the selected project. Shell commands are deliberately not confined to it.

## OAuth handling

OAuth credentials remain on the server in managed home directories. The authorization synchronizer validates file ownership/type, avoids symlink following for credential files, updates only `claudeAiOauth` and logs token fingerprints rather than token values.

Managed users must nevertheless be trusted with their own server-side copy. Separate Linux accounts do not provide independent subscription limits and do not protect data from root.

## Server-mode containers

Containers provide toolchain and resource isolation, not a hardened hostile-user boundary. They use the developer's UID/GID, mount the developer home, share host networking and omit the Docker socket. Project Dockerfile build steps run with root build privileges inside the build environment.

Enable server mode only for trusted participants. Do not run untrusted Dockerfile or hook definitions without an independent review and appropriate external isolation.

## Update integrity

Client updates are transported over pinned SSH and checked against SHA-256 values supplied by the same server's manifest. This protects against transport corruption but is not an independent signature against a malicious server administrator.

Release CI binds artifacts to an exact clean version tag, embeds the source commit, validates archive contents and rejects development manifests. Users should verify downloaded archives against the release `SHA256SUMS`.

## Secret hygiene

Never commit or attach:

- `remote-claude-*.json` invitations or other enrollment files;
- `id_ed25519`, `id_rsa`, PEM or other private keys;
- `.credentials.json`, OAuth access/refresh tokens or Git hosting tokens;
- `.env` files with secrets;
- production runbooks containing non-public addresses, account lists or client project names.

The repository `.gitignore` excludes common forms, and CI scans full Git history with gitleaks. These measures supplement review; they do not prove that every kind of private information is absent.

If a secret enters Git or a release asset:

1. Revoke/rotate it immediately; removal alone is insufficient.
2. Remove the affected release/distribution copies where possible.
3. Assume existing clones, forks, caches and downloads retain it.
4. Prefer a fresh sanitized repository when old history is not safe to expose.
5. Publish a new version rather than replacing an existing binary silently.
