Based on github.com/pkg/sftp v1.13.11. Original license: LICENSE.

Local changes are limited to request-server.go and packet-manager.go:
WithConcurrentReads enables parallel read-only requests and responses by ID.
Mutations (including writable-handle closes) remain global barriers. Read-only
closes wait only for preceding operations on their own handle, and directory
cursors advance in order per handle. Pending dependencies do not occupy the
worker pool; the number of dependency waiters is bounded by the worker count.
The open-handle registry is unlocked before calling a potentially remote Close.
The default upstream scheduling is unchanged when the option is omitted.

Only the VPS cache proxy enables this option. The workstation file server keeps
the upstream default. This avoids a slow source read blocking cached Git/config
metadata needed by Claude's synchronous UI code, including when a different
file is being closed or a directory listing is in progress.

Protocol reference:
https://datatracker.ietf.org/doc/html/draft-ietf-secsh-filexfer-02#section-6.1

Regression coverage is in the application's sftp_concurrency_test.go and its
existing filesystem/configuration/disconnect integration tests.
