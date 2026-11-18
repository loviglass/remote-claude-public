//go:build !windows

package main

import (
	"io/fs"
	"syscall"
)

func fileIno(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
