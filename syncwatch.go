package main

import (
	"path/filepath"
	"runtime"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// treeWatcher turns file events in the synchronised directories into scan
// requests. Scans remain the source of truth: events only make them prompt.
// macOS kqueue needs a descriptor per file, and on Windows a watched
// directory cannot be renamed, which would break git checkout and mv there;
// both rely on adaptive periodic scans instead.
type treeWatcher struct {
	mu      sync.Mutex
	w       *fsnotify.Watcher
	root    string
	watched map[string]bool
	failed  bool
}

func newTreeWatcher(root string, onEvent func()) *treeWatcher {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return nil
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil
	}
	t := &treeWatcher{w: w, root: root, watched: map[string]bool{}}
	go func() {
		for {
			select {
			case _, ok := <-w.Events:
				if !ok {
					return
				}
				onEvent()
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
				// A queue overflow loses events; a scan recovers them.
				onEvent()
			}
		}
	}()
	return t
}

// healthy reports whether events cover every synchronised directory.
func (t *treeWatcher) healthy() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.failed
}

func (t *treeWatcher) update(r scanResult) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failed {
		return
	}
	want := map[string]bool{"": true}
	for _, e := range r.list {
		if e.St.Kind == kindDir {
			want[e.Path] = true
		}
	}
	for p := range t.watched {
		if !want[p] {
			t.w.Remove(filepath.Join(t.root, filepath.FromSlash(p)))
			delete(t.watched, p)
		}
	}
	for p := range want {
		if t.watched[p] {
			continue
		}
		if err := t.w.Add(filepath.Join(t.root, filepath.FromSlash(p))); err != nil {
			// Usually the inotify watch limit: fall back to frequent scans.
			t.failed = true
			t.w.Close()
			return
		}
		t.watched[p] = true
	}
}

func (t *treeWatcher) close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.failed {
		t.w.Close()
	}
	t.failed = true
}
