package main

import (
	"bufio"
	"encoding/gob"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// syncServer keeps the replica Claude works on. It outlives workstation
// connections: Claude keeps reading and editing the replica while the
// workstation is away, and the next connection reconciles both sides.
type syncServer struct {
	replica   string
	live      string
	localRoot string
	tree      *syncTree
	scanner   *treeScanner
	statePath string
	stashDir  string

	mu      sync.Mutex
	base    map[string]*baseEntry
	pending map[string]*pendingPush
	// fresh means no agreed state exists, so replica leftovers are disposable.
	fresh   bool
	dirty   bool
	conn    *syncConn
	notices []string
	pushID  uint64
	// rejected remembers replica states the workstation refused (for example
	// a case-only variant on a case-insensitive disk) so they are not resent.
	rejected map[string]fileStat

	scans   chan scanRequest
	kick    chan struct{}
	replied chan struct{}
	done    chan struct{}
	watcher *treeWatcher
}

type pendingPush struct {
	id    uint64
	del   bool
	kind  entryKind
	mode  uint32
	rhash contentHash
	rep   fileStat
}

type syncConn struct {
	// epoch distinguishes connections: barrier sequence numbers are only
	// meaningful within one.
	epoch string
	out   *outQueue
	in    *seqWaiter
	ready chan struct{}
	done  chan struct{}
	once  sync.Once
}

func (c *syncConn) close() {
	c.once.Do(func() {
		close(c.done)
		c.out.close()
		c.in.close()
	})
}

type scanRequest struct {
	conn  *syncConn
	reply chan uint64
}

type syncState struct {
	Version int
	Base    map[string]*baseEntry
}

const syncStateVersion = 1

func openSyncServer(replica, live, localRoot, statePath, stashDir string) (*syncServer, error) {
	if err := os.MkdirAll(replica, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(replica)
	if err != nil {
		return nil, err
	}
	s := &syncServer{
		replica: replica, live: live, localRoot: localRoot,
		tree:      &syncTree{root: root},
		scanner:   &treeScanner{root: replica, liveRoot: live},
		statePath: statePath, stashDir: stashDir,
		base: map[string]*baseEntry{}, pending: map[string]*pendingPush{}, rejected: map[string]fileStat{},
		scans: make(chan scanRequest), kick: make(chan struct{}, 1), replied: make(chan struct{}, 1), done: make(chan struct{}),
	}
	var st syncState
	if f, err := os.Open(statePath); err == nil {
		err = gob.NewDecoder(bufio.NewReader(f)).Decode(&st)
		f.Close()
		if err == nil && st.Version == syncStateVersion && st.Base != nil {
			s.base = st.Base
		}
	}
	s.fresh = len(s.base) == 0
	s.watcher = newTreeWatcher(replica, s.requestScan)
	go s.loop()
	return s, nil
}

func (s *syncServer) close() {
	s.mu.Lock()
	c := s.conn
	s.conn = nil
	s.mu.Unlock()
	if c != nil {
		c.close()
	}
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	s.watcher.close()
	s.save()
	s.tree.root.Close()
}

func (s *syncServer) save() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	st := syncState{Version: syncStateVersion, Base: make(map[string]*baseEntry, len(s.base))}
	for k, v := range s.base {
		c := *v
		st.Base[k] = &c
	}
	s.dirty = false
	s.mu.Unlock()
	tmp := s.statePath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Printf("save sync state: %v", err)
		return
	}
	w := bufio.NewWriter(f)
	err = gob.NewEncoder(w).Encode(&st)
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, s.statePath)
	}
	if err != nil {
		log.Printf("save sync state: %v", err)
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
	}
}

func (s *syncServer) requestScan() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *syncServer) current() *syncConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn
}

func (s *syncServer) loop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	ticks := 0
	for {
		select {
		case <-s.done:
			return
		case req := <-s.scans:
			waiting := []scanRequest{req}
		drain:
			for {
				select {
				case r := <-s.scans:
					waiting = append(waiting, r)
				default:
					break drain
				}
			}
			c := s.readyConn()
			if c != nil {
				s.scanOnce(c)
			}
			for _, r := range waiting {
				if r.conn == c && c != nil {
					r.reply <- c.out.last()
				} else {
					r.reply <- 0
				}
			}
		case <-s.kick:
			if c := s.readyConn(); c != nil {
				s.scanOnce(c)
			}
		case <-ticker.C:
			ticks++
			// Events cover changes promptly; this pass is a safety net and
			// the fallback when the watch limit is exhausted.
			if c := s.readyConn(); c != nil && (!s.watcher.healthy() || ticks%5 == 0) {
				s.scanOnce(c)
			}
			s.save()
		}
	}
}

func (s *syncServer) readyConn() *syncConn {
	c := s.current()
	if c == nil {
		return nil
	}
	select {
	case <-c.ready:
		return c
	default:
		return nil
	}
}

// flush sends every replica change made so far and returns the sequence number
// the workstation must apply before running a command.
func (s *syncServer) flush(timeout time.Duration) (uint64, string, bool) {
	c := s.current()
	if c == nil {
		return 0, "", false
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-c.ready:
	case <-c.done:
		return 0, "", false
	case <-t.C:
		return 0, "", false
	}
	for {
		req := scanRequest{conn: c, reply: make(chan uint64, 1)}
		select {
		case s.scans <- req:
		case <-t.C:
			return 0, "", false
		case <-s.done:
			return 0, "", false
		}
		var seq uint64
		select {
		case seq = <-req.reply:
		case <-t.C:
			return 0, "", false
		}
		if seq == 0 {
			return 0, "", false
		}
		// A path edited again while its previous push is unanswered was
		// not sent by this scan; wait for the reply and scan again.
		if !s.stalePending() {
			return seq, c.epoch, true
		}
		select {
		case <-s.replied:
		case <-c.done:
			return 0, "", false
		case <-t.C:
			return 0, "", false
		}
	}
}

func (s *syncServer) signalReplied() {
	select {
	case s.replied <- struct{}{}:
	default:
	}
}

func (s *syncServer) stalePending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p, pend := range s.pending {
		cur := s.scanner.lstatEntry(p)
		if pend.del && cur.Kind != kindNone || !pend.del && !cur.same(pend.rep) {
			return true
		}
	}
	return false
}

// waitApplied waits until the workstation's changes up to seq are in the
// replica. A sequence from an earlier connection is settled by the new
// connection's reconciliation instead.
func (s *syncServer) waitApplied(seq uint64, epoch string, timeout time.Duration) bool {
	c := s.current()
	if c == nil {
		return false
	}
	if c.epoch != epoch {
		select {
		case <-c.ready:
			return true
		case <-time.After(timeout):
			return false
		}
	}
	return c.in.wait(seq, timeout)
}

func (s *syncServer) takeNotices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.notices
	s.notices = nil
	return n
}

// serve runs one workstation connection. ready is closed when the replica and
// the workstation agree, so Claude never starts on a partial project.
func (s *syncServer) serve(rw io.ReadWriteCloser) (*syncConn, chan error) {
	c := &syncConn{epoch: randomID()[:12], out: newOutQueue(), in: newSeqWaiter(), ready: make(chan struct{}), done: make(chan struct{})}
	s.mu.Lock()
	old := s.conn
	s.conn = c
	s.pending = map[string]*pendingPush{}
	s.rejected = map[string]fileStat{}
	paths := make([]string, 0, len(s.base))
	for p := range s.base {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	base := make([]*syncMsg, 0, len(paths))
	for _, p := range paths {
		b := s.base[p]
		base = append(base, &syncMsg{Op: opBase, Path: p, Kind: b.Kind, Mode: b.Mode, Hash: b.Hash, PC: b.PC})
	}
	s.mu.Unlock()
	if old != nil {
		old.close()
	}
	finished := make(chan error, 1)
	go func() {
		c.out.run(rw)
		rw.Close()
	}()
	go func() {
		<-c.done
		rw.Close()
	}()
	for _, m := range base {
		c.out.push(m)
	}
	c.out.push(&syncMsg{Op: opBaseEnd, Text: c.epoch})
	go func() {
		err := s.receive(c, rw)
		s.mu.Lock()
		if s.conn == c {
			s.conn = nil
			s.pending = map[string]*pendingPush{}
		}
		s.mu.Unlock()
		c.close()
		finished <- err
	}()
	return c, finished
}

func (s *syncServer) receive(c *syncConn, r io.Reader) error {
	dec := gob.NewDecoder(bufio.NewReaderSize(r, 256<<10))
	for {
		var m syncMsg
		if err := dec.Decode(&m); err != nil {
			return err
		}
		switch m.Op {
		case opPut, opDel:
			s.applyPC(c, &m)
		case opTouch, opAck, opReject:
			s.applyReply(c, &m)
		case opScanDone:
			s.reconcile(c)
		}
		c.in.set(m.Seq)
	}
}

// reconcile runs after the workstation reported all of its changes: Claude's
// edits from the disconnected period go to the workstation now.
func (s *syncServer) reconcile(c *syncConn) {
	s.mu.Lock()
	fresh := s.fresh
	s.fresh = false
	s.mu.Unlock()
	if fresh {
		// Without an agreed base nothing in the replica can be Claude's
		// unsynchronised work; leftovers would resurrect deleted files.
		r := s.scanner.scan()
		s.mu.Lock()
		for _, e := range r.list {
			if _, ok := s.base[e.Path]; !ok {
				if parent := path.Dir(e.Path); parent == "." || s.base[parent] != nil {
					s.tree.remove(e.Path)
				}
			}
		}
		s.mu.Unlock()
	}
	s.scanOnce(c)
	c.out.push(&syncMsg{Op: opReady})
	close(c.ready)
	s.mu.Lock()
	s.dirty = true
	s.mu.Unlock()
	s.save()
}

func (s *syncServer) scanOnce(c *syncConn) {
	r := s.scanner.scan()
	s.watcher.update(r)
	for i := 0; i < len(r.list); {
		var msgs []*syncMsg
		size := 0
		ack := c.in.current()
		s.mu.Lock()
		if s.conn != c {
			s.mu.Unlock()
			return
		}
		for ; i < len(r.list) && size < scanChunkBytes; i++ {
			if m := s.diffEntryLocked(r.list[i].Path, r.list[i].St); m != nil {
				m.Ack = ack
				msgs = append(msgs, m)
				size += len(m.Data)
			}
		}
		s.mu.Unlock()
		for _, m := range msgs {
			c.out.push(m)
		}
	}
	ack := c.in.current()
	s.mu.Lock()
	var msgs []*syncMsg
	if s.conn == c {
		msgs = s.deletionsLocked(r)
	}
	s.mu.Unlock()
	for _, m := range msgs {
		m.Ack = ack
		c.out.push(m)
	}
}

func (s *syncServer) diffEntryLocked(p string, st fileStat) *syncMsg {
	b := s.base[p]
	// One push per path at a time: the reply settles exactly that push, and
	// the next scan sends whatever changed meanwhile.
	if s.pending[p] != nil || st.Kind == kindLink {
		return nil
	}
	if r, ok := s.rejected[p]; ok && r.same(st) {
		return nil
	}
	switch st.Kind {
	case kindDir, kindSymlink:
		// The scan may predate a change applied from the workstation.
		if now := s.scanner.lstatEntry(p); now.Kind != st.Kind || now.Target != st.Target {
			return nil
		}
	}
	s.pushID++
	switch st.Kind {
	case kindDir:
		if b != nil && b.Kind == kindDir {
			return nil
		}
		s.pending[p] = &pendingPush{id: s.pushID, kind: kindDir, rep: st}
		return &syncMsg{Op: opPut, ID: s.pushID, Path: p, Kind: kindDir, Had: b != nil, Prev: baseHash(b)}
	case kindSymlink:
		if b != nil && b.Kind == kindSymlink && b.Rep.Target == st.Target {
			return nil
		}
		s.pending[p] = &pendingPush{id: s.pushID, kind: kindSymlink, rhash: hashString(st.Target), rep: st}
		return &syncMsg{Op: opPut, ID: s.pushID, Path: p, Kind: kindSymlink, Had: b != nil, Prev: baseHash(b), Target: translateTarget(st.Target, s.replica, s.localRoot)}
	case kindFile:
		if now := s.scanner.lstatEntry(p); now.Kind != kindFile {
			return nil
		} else {
			st = now
		}
		if b != nil && b.Kind == kindFile && b.Rep.same(st) {
			return nil
		}
		if st.Size > maxReplicaFile {
			// Larger files are not synchronised; the workstation would
			// refuse them on every scan.
			return nil
		}
		data, err := s.tree.read(p)
		if err != nil {
			return nil
		}
		rh := hashBytes(data)
		if b != nil && b.Kind == kindFile && b.RHash == rh {
			b.Rep = st
			s.dirty = true
			return nil
		}
		packed, z := packData(data)
		s.pending[p] = &pendingPush{id: s.pushID, kind: kindFile, mode: st.Mode, rhash: rh, rep: st}
		return &syncMsg{Op: opPut, ID: s.pushID, Path: p, Kind: kindFile, Mode: st.Mode, Had: b != nil, Prev: baseHash(b), Data: packed, Z: z}
	}
	return nil
}

func (s *syncServer) deletionsLocked(r scanResult) []*syncMsg {
	var gone []string
	for p, b := range s.base {
		if _, ok := r.byPath[p]; !ok && b.Kind != kindLink && !existsExactly(s.replica, p) {
			gone = append(gone, p)
		}
	}
	sort.Strings(gone)
	var msgs []*syncMsg
	done := map[string]bool{}
	for _, p := range gone {
		if pend := s.pending[p]; pend != nil {
			if pend.del {
				done[p] = true
			}
			continue
		}
		skip := false
		for q := path.Dir(p); q != "."; q = path.Dir(q) {
			if done[q] {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		done[p] = true
		s.pushID++
		s.pending[p] = &pendingPush{id: s.pushID, del: true}
		msgs = append(msgs, &syncMsg{Op: opDel, ID: s.pushID, Path: p, Had: true, Prev: s.base[p].Hash})
	}
	return msgs
}

// applyPC applies a workstation change. The workstation always wins; a
// conflicting replica edit by Claude is kept aside and reported to Claude.
func (s *syncServer) applyPC(c *syncConn, m *syncMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := cleanSyncPath(m.Path)
	if !ok || s.conn != c {
		// Messages still buffered from a replaced connection are stale.
		return
	}
	delete(s.rejected, p)
	b := s.base[p]
	cur := s.scanner.lstatEntry(p)
	var data []byte
	var rh contentHash
	if m.Op == opPut && m.Kind == kindFile {
		d, err := unpackData(m)
		if err != nil {
			log.Printf("sync %s: %v", p, err)
			return
		}
		data, rh = d, hashBytes(d)
	}
	if !s.fresh && cur.Kind == kindFile && (b == nil || b.Kind != kindFile || !b.Rep.same(cur)) {
		if old, err := s.tree.read(p); err == nil {
			oh := hashBytes(old)
			if oh != rh && (b == nil || oh != b.RHash) {
				s.stashLocked(p, old)
			}
		}
	}
	var err error
	target := ""
	switch {
	case m.Op == opDel:
		err = s.tree.remove(p)
	case m.Kind == kindFile:
		err = s.tree.writeFile(p, data, os.FileMode(m.Mode&0777))
	case m.Kind == kindDir:
		err = s.tree.mkdir(p)
	case m.Kind == kindSymlink:
		target = m.Target
		err = s.tree.symlink(p, target)
	case m.Kind == kindLink:
		err = s.tree.symlink(p, filepath.Join(s.live, filepath.FromSlash(p)))
	default:
		return
	}
	if err != nil {
		log.Printf("sync %s: %v", p, err)
		return
	}
	s.dirty = true
	if m.Op == opDel {
		dropTree(s.base, p)
		dropTree(s.pending, p)
		return
	}
	if m.Kind != kindDir {
		dropChildren(s.base, p)
		dropChildren(s.pending, p)
	}
	delete(s.pending, p)
	s.signalReplied()
	st := s.scanner.lstatEntry(p)
	e := &baseEntry{Kind: m.Kind, Mode: m.Mode, Hash: m.Hash, RHash: rh, PC: m.PC, Rep: st}
	if m.Kind == kindSymlink {
		e.RHash = hashString(target)
	}
	s.base[p] = e
}

func (s *syncServer) applyReply(c *syncConn, m *syncMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := cleanSyncPath(m.Path)
	if !ok || s.conn != c {
		return
	}
	if m.Op == opTouch {
		if b := s.base[p]; b != nil {
			b.PC = m.PC
			s.dirty = true
		}
		return
	}
	pend := s.pending[p]
	if pend == nil || pend.id != m.ID {
		return
	}
	delete(s.pending, p)
	// Whatever changed while the push was in flight goes next.
	s.requestScan()
	s.signalReplied()
	switch m.Op {
	case opReject:
		if !pend.del {
			s.rejected[p] = pend.rep
		}
	case opAck:
		s.dirty = true
		if pend.del {
			dropTree(s.base, p)
			dropTree(s.pending, p)
			return
		}
		s.base[p] = &baseEntry{Kind: pend.kind, Mode: pend.mode, Hash: m.Hash, RHash: pend.rhash, PC: m.PC, Rep: pend.rep}
	}
}

// existsExactly reports whether p exists with exactly this spelling. On a
// case-insensitive disk lstat also finds a differently cased name.
func existsExactly(root, p string) bool {
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(path.Dir(p))))
	if err != nil {
		return false
	}
	name := path.Base(p)
	for _, e := range entries {
		if e.Name() == name {
			return true
		}
	}
	return false
}

func (s *syncServer) stashLocked(p string, data []byte) {
	dir := filepath.Join(s.stashDir, time.Now().UTC().Format("20060102-150405"))
	dest := filepath.Join(dir, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return
	}
	if err := os.WriteFile(dest, data, 0600); err != nil {
		return
	}
	s.notices = append(s.notices, fmt.Sprintf("remote-claude: %s was changed on the workstation while your unsynchronised edit was pending; the workstation version was kept. Your version is saved at %s.", p, dest))
}
