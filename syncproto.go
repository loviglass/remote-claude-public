package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/gob"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Synchronisation protocol between the workstation (owner of the project) and
// the server replica Claude works on. Both sides track the last version they
// agree on (the base). A change on one side is sent with the base hash it
// replaces; a side applies it only if its own copy still matches that base.
// The workstation wins every conflict: the server keeps Claude's losing copy
// and tells Claude about it. Each direction is one ordered stream with
// sequence numbers, so a command barrier is "wait until sequence N is applied".
const (
	opBase uint8 = iota + 1
	opBaseEnd
	opPut
	opDel
	opTouch
	opAck
	opReject
	opScanDone
	opReady
)

type syncMsg struct {
	Op     uint8
	Seq    uint64
	Path   string
	Kind   entryKind
	Mode   uint32
	Hash   contentHash
	Prev   contentHash
	Had    bool
	Data   []byte
	Z      bool
	Target string
	PC     fileStat
	Text   string
	// ID names a server push; the workstation's Ack or Reject repeats it so
	// a reply never settles a newer push of the same path.
	ID uint64
	// Ack, on a server push, is the last workstation message the server had
	// applied when it made the push. A push that crossed a newer workstation
	// change of the same path is refused.
	Ack uint64
}

// Hash is the workstation form of the content; RHash is the replica form, which
// differs only for projected Claude configuration.
type baseEntry struct {
	Kind  entryKind
	Mode  uint32
	Hash  contentHash
	RHash contentHash
	PC    fileStat
	Rep   fileStat
}

func baseHash(b *baseEntry) contentHash {
	if b == nil {
		return contentHash{}
	}
	return b.Hash
}

// outQueue serialises one direction. Data messages wait while the queue holds
// more than its budget; control messages never wait, so a receive loop that
// answers with them cannot deadlock against the peer.
type outQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	msgs   []*syncMsg
	bytes  int
	seq    uint64
	closed bool
}

const outQueueBudget = 32 << 20

func newOutQueue() *outQueue {
	q := &outQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *outQueue) push(m *syncMsg) uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(m.Data) > 0 {
		for q.bytes > outQueueBudget && !q.closed {
			q.cond.Wait()
		}
	}
	if q.closed {
		return q.seq
	}
	q.seq++
	m.Seq = q.seq
	q.msgs = append(q.msgs, m)
	q.bytes += len(m.Data)
	q.cond.Broadcast()
	return m.Seq
}

func (q *outQueue) last() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.seq
}

func (q *outQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.msgs = nil
	q.cond.Broadcast()
	q.mu.Unlock()
}

func (q *outQueue) run(w io.Writer) error {
	bw := bufio.NewWriterSize(w, 256<<10)
	enc := gob.NewEncoder(bw)
	for {
		q.mu.Lock()
		if len(q.msgs) == 0 && !q.closed {
			q.mu.Unlock()
			if err := bw.Flush(); err != nil {
				q.close()
				return err
			}
			q.mu.Lock()
			for len(q.msgs) == 0 && !q.closed {
				q.cond.Wait()
			}
		}
		if q.closed {
			q.mu.Unlock()
			return nil
		}
		m := q.msgs[0]
		q.msgs[0] = nil
		q.msgs = q.msgs[1:]
		q.bytes -= len(m.Data)
		q.cond.Broadcast()
		q.mu.Unlock()
		if err := enc.Encode(m); err != nil {
			q.close()
			return err
		}
	}
}

type seqWaiter struct {
	mu      sync.Mutex
	cond    *sync.Cond
	applied uint64
	closed  bool
}

func newSeqWaiter() *seqWaiter {
	w := &seqWaiter{}
	w.cond = sync.NewCond(&w.mu)
	return w
}

func (w *seqWaiter) set(seq uint64) {
	w.mu.Lock()
	if seq > w.applied {
		w.applied = seq
	}
	w.cond.Broadcast()
	w.mu.Unlock()
}

func (w *seqWaiter) current() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.applied
}

func (w *seqWaiter) close() {
	w.mu.Lock()
	w.closed = true
	w.cond.Broadcast()
	w.mu.Unlock()
}

// wait reports whether sequence seq was applied before the timeout or close.
func (w *seqWaiter) wait(seq uint64, timeout time.Duration) bool {
	expired := false
	t := time.AfterFunc(timeout, func() {
		w.mu.Lock()
		expired = true
		w.cond.Broadcast()
		w.mu.Unlock()
	})
	defer t.Stop()
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.applied < seq && !w.closed && !expired {
		w.cond.Wait()
	}
	return w.applied >= seq
}

func packData(b []byte) ([]byte, bool) {
	if len(b) < 512 {
		return b, false
	}
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestSpeed)
	w.Write(b)
	w.Close()
	if buf.Len() >= len(b) {
		return b, false
	}
	return buf.Bytes(), true
}

func unpackData(m *syncMsg) ([]byte, error) {
	if !m.Z {
		return m.Data, nil
	}
	r := flate.NewReader(bytes.NewReader(m.Data))
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, 2*maxReplicaFile+1))
	if err == nil && len(b) > 2*maxReplicaFile {
		err = fmt.Errorf("synchronised file is too large")
	}
	return b, err
}

// cleanSyncPath accepts only a normalised relative slash path inside the tree.
func cleanSyncPath(p string) (string, bool) {
	if p == "" || strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") || path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	if runtime.GOOS == "windows" && strings.ContainsAny(p, "\\:") {
		return "", false
	}
	for _, part := range strings.Split(p, "/") {
		if strings.HasPrefix(part, syncTempPrefix) {
			return "", false
		}
	}
	return p, true
}

// syncTree applies changes through os.Root, so a symlink created by either
// side can never redirect a write outside the project.
type syncTree struct {
	root *os.Root
}

func (t *syncTree) osPath(p string) string { return filepath.FromSlash(p) }

func (t *syncTree) ensureParent(p string) error {
	dir := filepath.Dir(t.osPath(p))
	if dir == "." {
		return nil
	}
	if fi, err := t.root.Lstat(dir); err == nil && !fi.IsDir() {
		if err := t.root.RemoveAll(dir); err != nil {
			return err
		}
	}
	return t.root.MkdirAll(dir, 0777)
}

var errChangedMeanwhile = fmt.Errorf("file changed while it was being replaced")

// writeFile replaces p atomically. guard, if set, runs just before the
// replacement and can cancel it when the destination changed meanwhile.
func (t *syncTree) writeFile(p string, data []byte, mode os.FileMode, guard ...func() bool) error {
	if err := t.ensureParent(p); err != nil {
		return err
	}
	name := t.osPath(p)
	existing, err := t.root.Lstat(name)
	if err == nil && !existing.Mode().IsRegular() {
		if err := t.root.RemoveAll(name); err != nil {
			return err
		}
		existing = nil
	}
	if mode == 0 {
		mode = 0644
		if existing != nil {
			mode = existing.Mode().Perm()
		}
	}
	tmp := filepath.Join(filepath.Dir(name), syncTempPrefix+randomID()[:12])
	f, err := t.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && runtime.GOOS != "windows" {
		err = t.root.Chmod(tmp, mode)
	}
	if err == nil && len(guard) > 0 && !guard[0]() {
		err = errChangedMeanwhile
	}
	if err == nil {
		err = t.rename(tmp, name)
	}
	if err != nil {
		t.root.Remove(tmp)
		if existing != nil && runtime.GOOS == "windows" && err != errChangedMeanwhile {
			// An editor or scanner holding the file open blocks replacement
			// on Windows; writing in place still delivers the content.
			return t.root.WriteFile(name, data, mode)
		}
	}
	return err
}

func (t *syncTree) rename(from, to string) error {
	var err error
	for i := 0; i < 5; i++ {
		if err = t.root.Rename(from, to); err == nil || runtime.GOOS != "windows" {
			return err
		}
		time.Sleep(time.Duration(i+1) * 20 * time.Millisecond)
	}
	return err
}

func (t *syncTree) mkdir(p string) error {
	name := t.osPath(p)
	if fi, err := t.root.Lstat(name); err == nil {
		if fi.IsDir() {
			return nil
		}
		if err := t.root.RemoveAll(name); err != nil {
			return err
		}
	}
	if err := t.ensureParent(p); err != nil {
		return err
	}
	return t.root.Mkdir(name, 0777)
}

func (t *syncTree) symlink(p, target string) error {
	name := t.osPath(p)
	if err := t.ensureParent(p); err != nil {
		return err
	}
	if _, err := t.root.Lstat(name); err == nil {
		if err := t.root.RemoveAll(name); err != nil {
			return err
		}
	}
	return t.root.Symlink(target, name)
}

func (t *syncTree) remove(p string) error {
	return t.root.RemoveAll(t.osPath(p))
}

func (t *syncTree) read(p string) ([]byte, error) {
	return t.root.ReadFile(t.osPath(p))
}

// dropTree forgets p and everything below it.
func dropTree[V any](m map[string]V, p string) {
	delete(m, p)
	prefix := p + "/"
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			delete(m, k)
		}
	}
}

func dropChildren[V any](m map[string]V, p string) {
	prefix := p + "/"
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			delete(m, k)
		}
	}
}

// Symlink targets inside the project name the project root of their machine.
func translateTarget(target, from, to string) string {
	if from == "" || from == to {
		return target
	}
	if target == from {
		return to
	}
	sep := "/"
	if strings.Contains(from, "\\") {
		sep = "\\"
	}
	if strings.HasPrefix(target, from+sep) {
		rest := strings.TrimPrefix(target, from+sep)
		if strings.Contains(to, "\\") {
			rest = strings.ReplaceAll(rest, "/", "\\")
		} else {
			rest = strings.ReplaceAll(rest, "\\", "/")
		}
		return to + pathSep(to) + rest
	}
	return target
}

func pathSep(root string) string {
	if strings.Contains(root, "\\") {
		return "\\"
	}
	return "/"
}

// debouncer coalesces bursts of file events into one scan request.
type debouncer struct {
	mu    sync.Mutex
	timer *time.Timer
	delay time.Duration
	fire  func()
}

func (d *debouncer) trigger() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer == nil {
		d.timer = time.AfterFunc(d.delay, func() {
			d.mu.Lock()
			d.timer = nil
			d.mu.Unlock()
			d.fire()
		})
	}
}

func (d *debouncer) stop() {
	d.mu.Lock()
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	d.mu.Unlock()
}
