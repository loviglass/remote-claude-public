package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

type blockedSource struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (f *blockedSource) ReadAt(b []byte, off int64) (int, error) {
	f.once.Do(func() { close(f.started) })
	<-f.release
	if off != 0 {
		return 0, io.EOF
	}
	return copy(b, "slow"), io.EOF
}

type concurrentFixtureFS struct {
	*projectFS
	source *blockedSource
}

func (f *concurrentFixtureFS) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	if r.Filepath == "/slow-source" {
		return f.source, nil
	}
	return f.projectFS.Fileread(r)
}

func TestSlowReadDoesNotBlockMetadataAndWritesRemainOrdered(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, ".git"), 0700)
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600)
	root, e := os.OpenRoot(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer root.Close()
	source := &blockedSource{started: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	unblock := func() { release.Do(func() { close(source.release) }) }
	defer unblock()
	fs := &concurrentFixtureFS{projectFS: &projectFS{root: root}, source: source}
	a, b := net.Pipe()
	srv := sftp.NewRequestServer(a, sftp.Handlers{FileGet: fs, FilePut: fs, FileCmd: fs, FileList: fs}, sftp.WithConcurrentReads())
	go func() { srv.Serve(); srv.Close() }()
	defer a.Close()
	defer b.Close()
	client, e := sftp.NewClientPipe(b, b)
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	other, e := client.Open("/.git/HEAD")
	if e != nil {
		t.Fatal(e)
	}
	f, e := client.Open("/slow-source")
	if e != nil {
		t.Fatal(e)
	}
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 4)
		n, e := f.ReadAt(buf, 0)
		if n == 4 && string(buf) == "slow" {
			e = nil
		}
		readDone <- e
	}()
	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("source read did not start")
	}
	// Closing an unrelated read-only file must not stall all later requests.
	closeDone := make(chan error, 1)
	go func() { closeDone <- other.Close() }()
	select {
	case e := <-closeDone:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		unblock()
		t.Fatal("unrelated read-only close waited for a slow source read")
	}
	thisClose := make(chan error, 1)
	go func() { thisClose <- f.Close() }()
	select {
	case e := <-thisClose:
		t.Fatalf("close passed its own pending read: %v", e)
	case <-time.After(100 * time.Millisecond):
	}
	statDone := make(chan error, 1)
	go func() { _, e := client.Stat("/.git/HEAD"); statDone <- e }()
	select {
	case e := <-statDone:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		unblock()
		t.Fatal("cached metadata waited behind an unrelated source read")
	}
	writeDone := make(chan error, 1)
	go func() {
		f, e := client.Create("/barrier.txt")
		if e == nil {
			_, e = f.Write([]byte("ordered"))
			if e == nil {
				e = f.Close()
			}
		}
		writeDone <- e
	}()
	select {
	case e := <-writeDone:
		t.Fatalf("mutation passed the outstanding read barrier: %v", e)
	case <-time.After(100 * time.Millisecond):
	}
	if _, e := os.Stat(filepath.Join(dir, "barrier.txt")); !os.IsNotExist(e) {
		t.Fatalf("mutation became visible before the prior read finished: %v", e)
	}
	unblock()
	if e := <-readDone; e != nil {
		t.Fatal(e)
	}
	if e := <-writeDone; e != nil {
		t.Fatal(e)
	}
	if e := <-thisClose; e != nil {
		t.Fatal(e)
	}
	if b, e := os.ReadFile(filepath.Join(dir, "barrier.txt")); e != nil || string(b) != "ordered" {
		t.Fatalf("write result: %q, %v", b, e)
	}
}

type blockedClose struct {
	started chan struct{}
	release chan struct{}
}

func (f *blockedClose) ReadAt(b []byte, off int64) (int, error) { return 0, io.EOF }
func (f *blockedClose) Close() error {
	close(f.started)
	<-f.release
	return nil
}

type slowCloseFS struct {
	*projectFS
	file *blockedClose
}

func (f *slowCloseFS) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	if r.Filepath == "/slow-close" {
		return f.file, nil
	}
	return f.projectFS.Fileread(r)
}

func TestReadOnlyCloseDoesNotLockOtherHandles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cached.txt"), []byte("cached"), 0600)
	root, e := os.OpenRoot(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer root.Close()
	slow := &blockedClose{started: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(slow.release) }) }
	defer unblock()
	fs := &slowCloseFS{projectFS: &projectFS{root: root}, file: slow}
	a, b := net.Pipe()
	srv := sftp.NewRequestServer(a, sftp.Handlers{FileGet: fs, FilePut: fs, FileCmd: fs, FileList: fs}, sftp.WithConcurrentReads())
	go func() { srv.Serve(); srv.Close() }()
	defer a.Close()
	defer b.Close()
	client, e := sftp.NewClientPipe(b, b)
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	f, e := client.Open("/slow-close")
	if e != nil {
		t.Fatal(e)
	}
	closed := make(chan error, 1)
	go func() { closed <- f.Close() }()
	select {
	case <-slow.started:
	case <-time.After(time.Second):
		t.Fatal("close did not start")
	}
	read := make(chan error, 1)
	go func() {
		file, e := client.Open("/cached.txt")
		if e == nil {
			var data []byte
			data, e = io.ReadAll(file)
			file.Close()
			if e == nil && string(data) != "cached" {
				e = io.ErrUnexpectedEOF
			}
		}
		read <- e
	}()
	select {
	case e := <-read:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		unblock()
		t.Fatal("another handle waited for a slow read-only close")
	}
	unblock()
	if e := <-closed; e != nil {
		t.Fatal(e)
	}
}
