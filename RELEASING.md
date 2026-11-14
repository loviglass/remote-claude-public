# Release process

Remote Claude releases are immutable, provenance-bound artifacts. A version number identifies one source commit; a binary change always requires a new version.

## Release invariant

For version `X.Y.Z`:

- `main.go` contains `const version = "X.Y.Z"`;
- the worktree is clean, including untracked non-ignored files;
- `HEAD` is exactly tagged `vX.Y.Z`;
- `vX.Y.Z` resolves to the same commit as `HEAD`;
- `X.Y.Z` is newer than every previous numeric release tag;
- all six archives, the update manifest and `SHA256SUMS` are generated from that commit;
- the full commit is embedded in every executable and in `dist/clients/manifest.json`;
- `development` is `false` in the release manifest.

A dirty build, a tag/version mismatch, a reused older version, stale output or a modified artifact fails the build/checker.

## What CI does

Pushing a `v*` tag starts `.github/workflows/release.yml`:

1. Check out the complete Git history.
2. Install the Go toolchain from `go.mod`.
3. Run `go test -race ./...` and `go vet ./...`.
4. Run Python unit tests and a full-history gitleaks scan.
5. Run `python3 scripts/build-release.py --require-tag "$GITHUB_REF_NAME"`; the workflow tag must equal the source version tag.
6. Run `python3 scripts/check-release.py` and `(cd dist && sha256sum -c SHA256SUMS)`.
7. Publish exactly six platform archives and `SHA256SUMS` to the GitHub Release using the workflow-scoped `GITHUB_TOKEN`.

Normal pushes and pull requests run tests and secret scanning without release permissions.

## Preparing a release

1. Review the complete diff and update `CHANGELOG.md`.
2. Update `version` in `main.go` and any version-specific documentation.
3. Test behavior that changed, including real server integration where applicable. Do not claim physical Windows/macOS verification unless it actually occurred.
4. Run local checks:

   ```sh
   go test -race ./...
   go vet ./...
   python3 -m unittest discover -s scripts -p 'test_*.py'
   python3 -m compileall -q scripts
   ```

5. Commit the reviewed source and create exactly one matching release tag according to the repository's normal Git policy.
6. Push the tag and wait for the Release workflow. Do not upload locally built archives over a failed or partial CI run.
7. Inspect the workflow log and GitHub Release assets.

The orchestrator/maintainer must perform the actual commit and tag steps; preparing source changes alone does not create a release.

## Local development builds

When local artifacts are needed without a release tag:

```sh
python3 scripts/build-release.py --dev
python3 scripts/check-release.py --dev
```

The command prints a publishing warning and writes `development: true`. The manifest still records `HEAD` for diagnosis, but dirty edits are not separately captured by that commit ID. Development artifacts are only for local testing.

Do not copy development output to `/opt/remote-claude/clients`, GitHub Releases or another distribution channel. The client updater independently rejects development manifests, but that is a final safeguard rather than a publication workflow.

## Archive contract

Targets:

- Linux amd64 and ARM64 (`.tar.gz`);
- macOS amd64 and ARM64 (`.tar.gz`);
- Windows amd64 and ARM64 (`.zip`).

Archives contain the executable, platform installer, bundled ripgrep and its licenses, README, Russian quick start, MIT license and third-party notices. They do not contain the administrator guide, verification reports, invitations, credentials, keys, IDE files or Git metadata.

The checker verifies:

- exact six archives and six manifest platforms;
- source version, commit and development flag;
- every served file hash;
- archive SHA-256 values;
- archived binary bytes against served bytes;
- exact member allowlists and regular-file types;
- absence of stale output and unsafe/private paths.

## Publishing updates to a self-hosted server

A GitHub Release does not automatically update independent installations. The administrator must install the new server binary/helpers and copy the release `dist/clients/` tree to `/opt/remote-claude/clients` as described in `ADMIN.ru.md`.

Keep the previous server binary and release artifacts long enough to recover from a failed deployment. Update the pinned Claude version separately and only after compatibility testing.

## Hotfixes and rollbacks

- A hotfix is a new version and tag, not a rebuilt asset under an existing tag.
- Never replace published assets in place; installed clients update only when the numeric version increases.
- To roll back a server, reinstall the previous known-good server build and restore a compatible manifest/client tree. Existing clients with a newer version will not downgrade automatically.
- If artifacts were published from the wrong source or contain sensitive data, remove the release, treat exposed credentials as compromised, rotate them and publish a new corrected version. Deleting a release does not erase downloads, caches or forks.
