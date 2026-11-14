package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A lazy unmount detaches a dead SSHFS mount but leaves requests already sent
// to it waiting for an answer. Threads blocked there survive SIGKILL, so the
// old Claude process stays registered as the owner of its conversation and a
// resumed session hangs. Aborting the FUSE connection fails those requests.
func abortProjectMount(dir string) {
	info, e := os.ReadFile("/proc/self/mountinfo")
	if e != nil {
		return
	}
	if id, ok := fuseConnectionID(info, dir); ok {
		os.WriteFile(filepath.Join("/sys/fs/fuse/connections", id, "abort"), []byte("1"), 0)
	}
}

// Read the device from mountinfo instead of stat, which can block on the dead
// mount. The fusectl directory is named after the kernel dev_t.
func fuseConnectionID(mountinfo []byte, dir string) (string, bool) {
	id, found := "", false
	s := bufio.NewScanner(bytes.NewReader(mountinfo))
	for s.Scan() {
		fields := strings.Fields(s.Text())
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep < 5 || sep+1 >= len(fields) || unescapeMountPath(fields[4]) != dir || !strings.HasPrefix(fields[sep+1], "fuse") {
			continue
		}
		major, minor, ok := strings.Cut(fields[2], ":")
		ma, e1 := strconv.ParseUint(major, 10, 32)
		mi, e2 := strconv.ParseUint(minor, 10, 32)
		if !ok || e1 != nil || e2 != nil {
			continue
		}
		// Later lines are mounts stacked on top; the topmost is the live one.
		id, found = strconv.FormatUint(ma<<20|mi, 10), true
	}
	return id, found
}

// mountinfo escapes space, tab, newline and backslash as three octal digits.
func unescapeMountPath(p string) string {
	if !strings.Contains(p, `\`) {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' && i+3 < len(p) {
			if n, e := strconv.ParseUint(p[i+1:i+4], 8, 8); e == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(p[i])
	}
	return b.String()
}
