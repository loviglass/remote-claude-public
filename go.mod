module remote-claude

go 1.26.0

toolchain go1.26.8

require (
	github.com/pkg/sftp v1.13.11
	golang.org/x/crypto v0.57.0
	golang.org/x/sys v0.48.0
	golang.org/x/term v0.46.0
	gopkg.in/yaml.v3 v3.0.1
	mvdan.cc/sh/v3 v3.14.1
)

require (
	github.com/creack/pty v1.1.24 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/kr/fs v0.1.0 // indirect
)

replace github.com/pkg/sftp => ./third_party/sftp
