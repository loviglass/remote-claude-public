package main

import (
	"bufio"
	"encoding/gob"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// syncPC is the workstation side: it owns the project, detects its changes by
// scanning and applies the replica's changes when they do not overwrite a
// newer workstation edit.
type syncPC struct {
	tree    *syncTree
	scanner *treeScanner
	// project renders Claude configuration for the server (unwrap=false) and
	// back (unwrap=true); nil leaves content unchanged.
	project    func(p string, data []byte, unwrap bool) ([]byte, error)
	localRoot  string
	remoteRoot string

	mu   sync.Mutex
	base map[string]*baseEntry

	out     *outQueue
	in      *seqWaiter
	scans   chan chan uint64
	kick    chan struct{}
	ready   chan struct{}
	done    chan struct{}
	once    sync.Once
	watcher *treeWatcher

	// The first scan of a connection re-renders projected configuration:
	// it embeds loopback ports that belong to this connection.
	initial bool
	// reproject lists configuration Claude wrote in plain form; the replica
	// must receive its projected form.
	reproject map[string]bool
	// sending counts changes decided but not yet queued; sent holds the
	// sequence number of the last queued change of each path.
	sending map[string]int
	sent    map[string]uint64
	// epoch is the server's name for this connection, set before any
	// command can arrive.
	epochMu sync.Mutex
	epoch   string

	// Initial transfer progress, for the terminal.
	sentFiles atomic.Int64
	sentBytes atomic.Int64
}

func newSyncPC(root *os.Root, localRoot, remoteRoot string, ignores *ignoreRules, project func(string, []byte, bool) ([]byte, error)) *syncPC {
	s := &syncPC{
		tree:       &syncTree{root: root},
		scanner:    &treeScanner{root: localRoot, ignores: ignores, noModes: noPermissionBits()},
		project:    project,
		localRoot:  localRoot,
		remoteRoot: remoteRoot,
		base:       map[string]*baseEntry{},
		reproject:  map[string]bool{},
		sending:    map[string]int{},
		sent:       map[string]uint64{},
		out:        newOutQueue(),
		in:         newSeqWaiter(),
		scans:      make(chan chan uint64),
		kick:       make(chan struct{}, 1),
		ready:      make(chan struct{}),
		done:       make(chan struct{}),
	}
	s.watcher = newTreeWatcher(localRoot, s.requestScan)
	return s
}

func (s *syncPC) close() {
	s.once.Do(func() {
		close(s.done)
		s.out.close()
		s.in.close()
		s.watcher.close()
	})
}

// serve runs one synchronisation connection until it fails or closes.
func (s *syncPC) serve(rw io.ReadWriteCloser) error {
	defer s.close()
	defer rw.Close()
	go func() {
		s.out.run(rw)
		rw.Close()
	}()
	dec := gob.NewDecoder(bufio.NewReaderSize(rw, 256<<10))
	for {
		var m syncMsg
		if err := dec.Decode(&m); err != nil {
			return err
		}
		switch m.Op {
		case opBase:
			if p, ok := cleanSyncPath(m.Path); ok {
				s.mu.Lock()
				s.base[p] = &baseEntry{Kind: m.Kind, Mode: m.Mode, Hash: m.Hash, PC: m.PC}
				s.mu.Unlock()
			}
		case opBaseEnd:
			s.epochMu.Lock()
			s.epoch = m.Text
			s.epochMu.Unlock()
			go s.loop()
		case opPut, opDel:
			s.applyRemote(&m)
		case opReady:
			select {
			case <-s.ready:
			default:
				close(s.ready)
			}
		}
		s.in.set(m.Seq)
	}
}

func (s *syncPC) loop() {
	s.initial = true
	s.scanOnce()
	s.initial = false
	s.out.push(&syncMsg{Op: opScanDone})
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	// Without file events (macOS, Windows, exhausted watch limits) periodic
	// scans notice editor changes; their pace adapts to the tree's size.
	lastScan := time.Duration(0)
	nextPeriodic := time.Now()
	for {
		select {
		case <-s.done:
			return
		case req := <-s.scans:
			waiting := []chan uint64{req}
		drain:
			for {
				select {
				case r := <-s.scans:
					waiting = append(waiting, r)
				default:
					break drain
				}
			}
			s.scanOnce()
			seq := s.out.last()
			for _, r := range waiting {
				r <- seq
			}
		case <-s.kick:
			s.scanOnce()
		case <-ticker.C:
			if time.Now().Before(nextPeriodic) {
				continue
			}
			start := time.Now()
			s.scanOnce()
			lastScan = time.Since(start)
			interval := max(time.Second, 20*lastScan)
			if s.watcher.healthy() {
				interval = max(15*time.Second, interval)
			}
			nextPeriodic = time.Now().Add(interval)
		}
	}
}

func (s *syncPC) currentEpoch() string {
	s.epochMu.Lock()
	defer s.epochMu.Unlock()
	return s.epoch
}

func (s *syncPC) requestScan() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// barrier makes every change made so far on the workstation visible to the
// server and returns the sequence number the server must apply first.
func (s *syncPC) barrier(timeout time.Duration) (uint64, string, bool) {
	seq, ok := s.barrierSeq(timeout)
	return seq, s.currentEpoch(), ok
}

func (s *syncPC) barrierSeq(timeout time.Duration) (uint64, bool) {
	r := make(chan uint64, 1)
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case s.scans <- r:
	case <-s.done:
		return 0, false
	case <-t.C:
		return 0, false
	}
	select {
	case seq := <-r:
		return seq, true
	case <-s.done:
		return 0, false
	case <-t.C:
		return 0, false
	}
}

// waitApplied waits for the server's changes up to seq. A sequence from an
// earlier connection is covered by this connection's reconciliation.
func (s *syncPC) waitApplied(seq uint64, epoch string, timeout time.Duration) bool {
	if epoch != s.currentEpoch() {
		select {
		case <-s.ready:
			return true
		case <-time.After(timeout):
			return false
		}
	}
	return s.in.wait(seq, timeout)
}

const scanChunkBytes = 8 << 20

func (s *syncPC) scanOnce() {
	r := s.scanner.scan()
	s.watcher.update(r)
	for i := 0; i < len(r.list); {
		var msgs []*syncMsg
		size := 0
		s.mu.Lock()
		for ; i < len(r.list) && size < scanChunkBytes; i++ {
			if m := s.diffEntryLocked(r.list[i].Path, r.list[i].St); m != nil {
				msgs = append(msgs, m)
				size += len(m.Data)
				if m.Op != opTouch {
					s.sending[m.Path]++
				}
			}
		}
		s.mu.Unlock()
		s.send(msgs)
	}
	s.mu.Lock()
	msgs := s.deletionsLocked(r)
	for _, m := range msgs {
		s.sending[m.Path]++
	}
	s.mu.Unlock()
	s.send(msgs)
}

func (s *syncPC) send(msgs []*syncMsg) {
	for _, m := range msgs {
		if m.Op == opPut {
			s.sentFiles.Add(1)
			s.sentBytes.Add(int64(len(m.Data)))
		}
		seq := s.out.push(m)
		if m.Op == opTouch {
			continue // no content change: nothing for a push to cross
		}
		s.mu.Lock()
		s.sent[m.Path] = seq
		if s.sending[m.Path]--; s.sending[m.Path] <= 0 {
			delete(s.sending, m.Path)
		}
		s.mu.Unlock()
	}
}

// crossedLocked reports whether the workstation changed p, or anything below
// it, after the server made push m: the workstation's change wins.
func (s *syncPC) crossedLocked(p string, ack uint64) bool {
	prefix := p + "/"
	for q, n := range s.sending {
		if n > 0 && (q == p || strings.HasPrefix(q, prefix)) {
			return true
		}
	}
	for q, seq := range s.sent {
		if seq > ack && (q == p || strings.HasPrefix(q, prefix)) {
			return true
		}
	}
	return false
}

func (s *syncPC) diffEntryLocked(p string, st fileStat) *syncMsg {
	b := s.base[p]
	switch st.Kind {
	case kindLink:
		if b != nil && b.Kind == kindLink {
			return nil
		}
		dropChildren(s.base, p)
		s.base[p] = &baseEntry{Kind: kindLink}
		return &syncMsg{Op: opPut, Path: p, Kind: kindLink, Had: b != nil, Prev: baseHash(b)}
	case kindDir:
		if b != nil && b.Kind == kindDir {
			return nil
		}
		if s.scanner.lstatEntry(p).Kind != kindDir {
			return nil // the scan predates a change applied meanwhile
		}
		s.base[p] = &baseEntry{Kind: kindDir}
		return &syncMsg{Op: opPut, Path: p, Kind: kindDir, Had: b != nil, Prev: baseHash(b)}
	case kindSymlink:
		if b != nil && b.Kind == kindSymlink && b.PC.Target == st.Target {
			return nil
		}
		if now := s.scanner.lstatEntry(p); now.Kind != kindSymlink || now.Target != st.Target {
			return nil
		}
		h := hashString(st.Target)
		s.base[p] = &baseEntry{Kind: kindSymlink, Hash: h, PC: st}
		return &syncMsg{Op: opPut, Path: p, Kind: kindSymlink, Hash: h, Had: b != nil, Prev: baseHash(b), Target: translateTarget(st.Target, s.localRoot, s.remoteRoot), PC: st}
	case kindFile:
		// The scan may predate a file this side just wrote.
		if now := s.scanner.lstatEntry(p); now.Kind != kindFile {
			return nil
		} else {
			st = now
		}
		reproject := (s.initial || s.reproject[p]) && s.project != nil && projectedConfig(p)
		delete(s.reproject, p)
		if b != nil && b.Kind == kindFile && b.PC.same(st) && !reproject {
			return nil
		}
		data, err := s.tree.read(p)
		if err != nil {
			return nil
		}
		h := hashBytes(data)
		if b != nil && b.Kind == kindFile && b.Hash == h && b.Mode == st.Mode && !reproject {
			b.PC = st
			return &syncMsg{Op: opTouch, Path: p, PC: st}
		}
		send := data
		if s.project != nil && projectedConfig(p) {
			if projected, err := s.project(p, data, false); err == nil {
				send = projected
			}
		}
		packed, z := packData(send)
		s.base[p] = &baseEntry{Kind: kindFile, Mode: st.Mode, Hash: h, PC: st}
		return &syncMsg{Op: opPut, Path: p, Kind: kindFile, Mode: st.Mode, Hash: h, Had: b != nil, Prev: baseHash(b), Data: packed, Z: z, PC: st}
	}
	return nil
}

// deletionsLocked reports the topmost paths that disappeared.
func (s *syncPC) deletionsLocked(r scanResult) []*syncMsg {
	var gone []string
	for p := range s.base {
		// The scan may predate a file that synchronisation just wrote.
		if _, ok := r.byPath[p]; !ok && !existsExactly(s.localRoot, p) {
			gone = append(gone, p)
		}
	}
	sort.Strings(gone)
	var msgs []*syncMsg
	for _, p := range gone {
		b, ok := s.base[p]
		if !ok {
			continue // removed with an ancestor
		}
		if parent := path.Dir(p); parent != "." {
			if pb, ok := s.base[parent]; ok {
				if _, alive := r.byPath[parent]; !alive || pb.Kind == kindLink {
					continue
				}
			}
		}
		msgs = append(msgs, &syncMsg{Op: opDel, Path: p, Had: true, Prev: b.Hash})
		dropTree(s.base, p)
	}
	return msgs
}

// untouchedLocked reports whether the workstation copy still equals the base.
func (s *syncPC) untouchedLocked(p string, b *baseEntry, cur fileStat) bool {
	if b == nil {
		return cur.Kind == kindNone
	}
	if cur.same(b.PC) {
		return true
	}
	switch b.Kind {
	case kindDir:
		return cur.Kind == kindDir && s.onlyKnownBelowLocked(p)
	case kindFile:
		if cur.Kind != kindFile {
			return false
		}
		data, err := s.tree.read(p)
		return err == nil && hashBytes(data) == b.Hash
	case kindSymlink:
		return cur.Kind == kindSymlink && cur.Target == b.PC.Target
	}
	return false
}

func (s *syncPC) applyRemote(m *syncMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := cleanSyncPath(m.Path)
	if !ok {
		return
	}
	reject := func() {
		s.out.push(&syncMsg{Op: opReject, ID: m.ID, Path: m.Path})
		// The workstation wins: forgetting the base makes the next scan send
		// what is here now. A path that is gone is reported as deleted.
		if existsExactly(s.localRoot, p) {
			dropTree(s.base, p)
		}
		s.requestScan()
	}
	b := s.base[p]
	cur := s.scanner.lstatEntry(p)
	var data, raw []byte
	var h contentHash
	if m.Op == opPut {
		switch m.Kind {
		case kindFile:
			var err error
			raw, err = unpackData(m)
			if err != nil {
				reject()
				return
			}
			data = raw
			if s.project != nil && projectedConfig(p) {
				if d, err := s.project(p, raw, true); err == nil {
					data = d
				}
			}
			h = hashBytes(data)
		case kindSymlink:
			h = hashString(translateTarget(m.Target, s.remoteRoot, s.localRoot))
		case kindDir:
		default:
			reject()
			return
		}
	}
	if s.crossedLocked(p, m.Ack) {
		// The workstation's own change of this path is on its way and wins.
		s.out.push(&syncMsg{Op: opReject, ID: m.ID, Path: m.Path})
		return
	}
	agreed := (b == nil) == !m.Had && (b == nil || b.Hash == m.Prev)
	// Replacing a directory must not delete what the server does not know:
	// ignored files or new files not yet scanned.
	if b != nil && b.Kind == kindDir && (m.Op == opDel || m.Kind != kindDir) && !s.onlyKnownBelowLocked(p) {
		agreed = false
	}
	// Nor may creating a path replace a workstation file in its way.
	for q := path.Dir(p); q != "."; q = path.Dir(q) {
		if k := s.scanner.lstatEntry(q).Kind; k != kindNone && k != kindDir {
			agreed = false
		}
	}
	if !agreed || !s.untouchedLocked(p, b, cur) {
		// Both sides may already hold the same result, e.g. after a lost ack.
		if m.Op == opPut && m.Kind == kindFile && cur.Kind == kindFile {
			if d, err := s.tree.read(p); err == nil && hashBytes(d) == h {
				s.base[p] = &baseEntry{Kind: kindFile, Mode: cur.Mode, Hash: h, PC: cur}
				s.out.push(&syncMsg{Op: opAck, ID: m.ID, Path: m.Path, Hash: h, PC: cur})
				return
			}
		}
		if m.Op == opDel && cur.Kind == kindNone {
			dropTree(s.base, p)
			s.out.push(&syncMsg{Op: opAck, ID: m.ID, Path: m.Path})
			return
		}
		reject()
		return
	}
	var err error
	switch {
	case m.Op == opDel:
		err = s.tree.remove(p)
	case m.Kind == kindFile:
		mode := os.FileMode(m.Mode & 0777)
		if noPermissionBits() {
			mode = 0
		}
		err = s.tree.writeFile(p, data, mode, func() bool { return s.untouchedLocked(p, b, s.scanner.lstatEntry(p)) })
	case m.Kind == kindDir:
		err = s.tree.mkdir(p)
	case m.Kind == kindSymlink:
		err = s.tree.symlink(p, translateTarget(m.Target, s.remoteRoot, s.localRoot))
	}
	if err != nil {
		reject()
		return
	}
	if m.Op == opDel {
		dropTree(s.base, p)
		s.out.push(&syncMsg{Op: opAck, ID: m.ID, Path: m.Path})
		return
	}
	st := s.scanner.lstatEntry(p)
	if b != nil && b.Kind == kindDir && m.Kind != kindDir {
		dropChildren(s.base, p)
	}
	s.base[p] = &baseEntry{Kind: m.Kind, Mode: st.Mode, Hash: h, PC: st}
	s.out.push(&syncMsg{Op: opAck, ID: m.ID, Path: m.Path, Hash: h, PC: st})
	if m.Kind == kindFile && s.project != nil && projectedConfig(p) {
		// Claude may have written executable entries in plain form; they
		// must run on the workstation, so the replica gets them projected.
		if projected, err := s.project(p, data, false); err == nil && string(projected) != string(raw) {
			s.reproject[p] = true
			s.requestScan()
		}
	}
}

// onlyKnownBelowLocked reports whether everything under directory p is in the
// base as replicated content (not a link to workstation-only files).
func (s *syncPC) onlyKnownBelowLocked(p string) bool {
	ok := true
	var walk func(rel string)
	walk = func(rel string) {
		entries, err := fs.ReadDir(s.tree.root.FS(), rel)
		if err != nil {
			return
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), syncTempPrefix) {
				continue
			}
			child := rel + "/" + e.Name()
			b := s.base[child]
			if b == nil || b.Kind == kindLink {
				ok = false
				return
			}
			if e.IsDir() {
				walk(child)
			}
			if !ok {
				return
			}
		}
	}
	walk(p)
	return ok
}
