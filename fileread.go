package main

import (
	"bytes"
	"io"
	"sync"
)

const maxReadSnapshot = 256 << 10

// Snapshot only one open descriptor, never cache content between opens. This
// combines SFTP OPEN/READ/CLOSE round trips for typical source files, while
// edits on the workstation are visible on the next open. Large files stream.
func (w *worker) readFileSnapshot(q ExecRequest, out io.Writer) ExecResult {
	p, ok := q.Spec["path"].(string)
	if !ok || projectedConfig(p) {
		return ExecResult{Code: 3}
	}
	p, err := cleanPath(p)
	if err != nil {
		return ExecResult{Code: 3}
	}
	st, err := w.fs.root.Stat(fsPath(p))
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxReadSnapshot {
		return ExecResult{Code: 3}
	}
	f, err := w.fs.root.Open(fsPath(p))
	if err != nil {
		return ExecResult{Code: 3}
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxReadSnapshot+1))
	if err != nil || len(b) > maxReadSnapshot {
		return ExecResult{Code: 3}
	}
	if _, err := out.Write(b); err != nil {
		return ExecResult{Code: 1, Error: err.Error()}
	}
	return ExecResult{Code: 0}
}

type boundedReadBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *boundedReadBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := maxReadSnapshot - b.Len()
	if len(p) > left {
		b.overflow = true
		p = p[:left]
	}
	b.Buffer.Write(p)
	return n, nil
}

type snapshotReader struct {
	*bytes.Reader
	once    sync.Once
	release func()
}

func (r *snapshotReader) Close() error { r.once.Do(r.release); return nil }
