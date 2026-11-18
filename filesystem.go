package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

type projectFS struct {
	root       *os.Root
	mu         sync.Mutex
	projectMCP mcpProjection
}

func cleanPath(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", os.ErrInvalid
	}
	p = path.Clean("/" + p)
	return strings.TrimPrefix(p, "/"), nil
}
func fsPath(p string) string {
	if p == "" {
		return "."
	}
	return p
}
func (f *projectFS) read(p string) ([]byte, error) {
	b, e := f.root.ReadFile(fsPath(p))
	if e != nil {
		return nil, e
	}
	if projectedConfig(p) {
		return projectJSON(b, false, f.projectMCP)
	}
	return b, nil
}
func (f *projectFS) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	p, e := cleanPath(r.Filepath)
	if e != nil {
		return nil, e
	}
	if projectedConfig(p) {
		b, e := f.read(p)
		return bytes.NewReader(b), e
	}
	return f.root.Open(fsPath(p))
}

type configHandle struct {
	fs     *projectFS
	path   string
	data   []byte
	mu     sync.Mutex
	failed bool
	mode   os.FileMode
}

func (h *configHandle) ReadAt(b []byte, off int64) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return bytes.NewReader(h.data).ReadAt(b, off)
}
func (h *configHandle) WriteAt(b []byte, off int64) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if off < 0 || off+int64(len(b)) > 16<<20 {
		return 0, fmt.Errorf("configuration file exceeds 16 MiB")
	}
	end := int(off) + len(b)
	if end > len(h.data) {
		h.data = append(h.data, make([]byte, end-len(h.data))...)
	}
	copy(h.data[int(off):], b)
	return len(b), nil
}
func (h *configHandle) TransferError(e error) { h.mu.Lock(); h.failed = true; h.mu.Unlock() }
func (h *configHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failed {
		return fmt.Errorf("incomplete configuration write was discarded")
	}
	b, e := projectJSON(h.data, true, h.fs.projectMCP)
	if e != nil {
		return e
	}
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	return h.fs.root.WriteFile(h.path, b, h.mode)
}
func (f *projectFS) openWrite(r *sftp.Request) (sftp.WriterAtReaderAt, error) {
	p, e := cleanPath(r.Filepath)
	if e != nil {
		return nil, e
	}
	a := r.Pflags()
	if projectedConfig(p) {
		mode := os.FileMode(0600)
		var b []byte
		st, err := f.root.Stat(p)
		if err == nil {
			mode = st.Mode().Perm()
			if a.Excl && a.Creat {
				return nil, os.ErrExist
			}
			if !a.Trunc {
				b, e = f.read(p)
			}
		} else if !os.IsNotExist(err) || !a.Creat {
			return nil, err
		}
		if e != nil {
			return nil, e
		}
		return &configHandle{fs: f, path: p, data: b, mode: mode}, nil
	}
	flags := os.O_WRONLY
	if a.Read {
		flags = os.O_RDWR
	}
	if a.Creat {
		flags |= os.O_CREATE
	}
	if a.Trunc {
		flags |= os.O_TRUNC
	}
	if a.Excl {
		flags |= os.O_EXCL
	}
	// The umask applies, as for any program creating a file.
	return f.root.OpenFile(p, flags, 0666)
}
func (f *projectFS) Filewrite(r *sftp.Request) (io.WriterAt, error)          { return f.openWrite(r) }
func (f *projectFS) OpenFile(r *sftp.Request) (sftp.WriterAtReaderAt, error) { return f.openWrite(r) }

type fileList []os.FileInfo

func (l fileList) ListAt(b []os.FileInfo, off int64) (int, error) {
	if off < 0 || off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(b, l[off:])
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

type projectedInfo struct {
	os.FileInfo
	size int64
}

func (i projectedInfo) Size() int64 { return i.size }
func (f *projectFS) stat(p string, follow bool) (os.FileInfo, error) {
	var st os.FileInfo
	var e error
	if follow {
		st, e = f.root.Stat(fsPath(p))
	} else {
		st, e = f.root.Lstat(fsPath(p))
	}
	if e != nil {
		return nil, e
	}
	if !st.IsDir() && projectedConfig(p) {
		if b, e := f.read(p); e == nil {
			return projectedInfo{st, int64(len(b))}, nil
		}
	}
	return st, nil
}
func (f *projectFS) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	p, e := cleanPath(r.Filepath)
	if e != nil {
		return nil, e
	}
	if r.Method == "List" {
		d, e := f.root.Open(fsPath(p))
		if e != nil {
			return nil, e
		}
		defer d.Close()
		a, e := d.Readdir(-1)
		if e != nil {
			return nil, e
		}
		for i, st := range a {
			if x, e := f.stat(path.Join(p, st.Name()), false); e == nil {
				a[i] = x
			}
		}
		return fileList(a), nil
	}
	st, e := f.stat(p, true)
	if e != nil {
		return nil, e
	}
	return fileList{st}, nil
}
func (f *projectFS) Lstat(r *sftp.Request) (sftp.ListerAt, error) {
	p, e := cleanPath(r.Filepath)
	if e != nil {
		return nil, e
	}
	st, e := f.stat(p, false)
	if e != nil {
		return nil, e
	}
	return fileList{st}, nil
}
func (f *projectFS) RealPath(p string) (string, error) { p, e := cleanPath(p); return "/" + p, e }
func (f *projectFS) Readlink(p string) (string, error) {
	p, e := cleanPath(p)
	if e != nil {
		return "", e
	}
	return f.root.Readlink(p)
}
func (f *projectFS) PosixRename(r *sftp.Request) error { return f.Filecmd(r) }
func (f *projectFS) Filecmd(r *sftp.Request) error {
	p, e := cleanPath(r.Filepath)
	if e != nil {
		return e
	}
	q, e := cleanPath(r.Target)
	if e != nil {
		return e
	}
	switch r.Method {
	case "Rename", "PosixRename":
		if projectedConfig(q) {
			b, e := f.root.ReadFile(p)
			if e != nil {
				return e
			}
			b, e = projectJSON(b, true, f.projectMCP)
			if e != nil {
				return e
			}
			if e = f.root.WriteFile(p, b, 0600); e != nil {
				return e
			}
		}
		return f.root.Rename(p, q)
	case "Remove", "Rmdir":
		return f.root.Remove(p)
	case "Mkdir":
		return f.root.Mkdir(p, 0777)
	case "Symlink":
		return f.root.Symlink(r.Filepath, q)
	case "Link":
		return f.root.Link(p, q)
	case "Setstat":
		a := r.AttrFlags()
		st := r.Attributes()
		if a.Size {
			h, e := f.root.OpenFile(p, os.O_WRONLY, 0)
			if e != nil {
				return e
			}
			e = h.Truncate(int64(st.Size))
			if ce := h.Close(); e == nil {
				e = ce
			}
			if e != nil {
				return e
			}
		}
		if a.Permissions {
			if e = f.root.Chmod(p, os.FileMode(st.Mode).Perm()); e != nil {
				return e
			}
		}
		if a.Acmodtime {
			return f.root.Chtimes(p, time.Unix(int64(st.Atime), 0), time.Unix(int64(st.Mtime), 0))
		}
		return nil
	}
	return sftp.ErrSSHFxOpUnsupported
}
