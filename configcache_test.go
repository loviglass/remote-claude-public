package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

func testCachedTransport(t *testing.T, c SessionConfig) *sftp.Client {
	t.Helper()
	client, _ := testCachedTransportAndCache(t, c)
	return client
}

func testCachedTransportAndCache(t *testing.T, c SessionConfig) (*sftp.Client, *cachedSFTP) {
	t.Helper()
	ssh, e := dialBridge(c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { ssh.Close() })
	backend, e := sftp.NewClient(ssh)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { backend.Close() })
	cache := &cachedSFTP{ssh: ssh, files: backend, gitSnapshot: true, extraConfig: true}
	a, b := net.Pipe()
	srv := sftp.NewRequestServer(a, sftp.Handlers{FileGet: cache, FilePut: cache, FileCmd: cache, FileList: cache}, sftp.WithConcurrentReads())
	go func() { srv.Serve(); srv.Close() }()
	t.Cleanup(func() { a.Close(); b.Close() })
	client, e := sftp.NewClientPipe(b, b)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { client.Close() })
	return client, cache
}

func TestMissingPathsAreRememberedBriefly(t *testing.T) {
	c, _ := testWorker(t)
	client, cache := testCachedTransportAndCache(t, c)
	outside := filepath.Join(c.LocalRoot, "f.log")
	if _, e := client.Lstat("/f.log"); !os.IsNotExist(e) {
		t.Fatal(e)
	}
	// A change the VPS did not cause stays hidden no longer than the lifetime.
	if e := os.WriteFile(outside, nil, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := client.Lstat("/f.log"); !os.IsNotExist(e) {
		t.Fatalf("repeated miss reached the workstation: %v", e)
	}
	time.Sleep(missLifetime)
	if _, e := client.Lstat("/f.log"); e != nil {
		t.Fatalf("miss outlived its lifetime: %v", e)
	}
	// Workstation commands may create anything, so they forget every miss.
	if _, e := client.Stat("/out.txt"); !os.IsNotExist(e) {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(c.LocalRoot, "out.txt"), nil, 0600)
	cache.forgetMisses()
	if _, e := client.Stat("/out.txt"); e != nil {
		t.Fatalf("command output hidden by an old miss: %v", e)
	}
	// A file created through the mount is visible before its handle closes.
	if _, e := client.Lstat("/new.txt"); !os.IsNotExist(e) {
		t.Fatal(e)
	}
	h, e := client.Create("/new.txt")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := client.Lstat("/new.txt"); e != nil {
		t.Fatalf("created file hidden by an old miss: %v", e)
	}
	h.Close()
}

func TestMissFromBeforeACommandIsNotRemembered(t *testing.T) {
	cache := &cachedSFTP{}
	epoch := cache.missEpochNow()
	cache.forgetMisses()
	cache.rememberMiss("f.log", epoch)
	if cache.recentMiss("f.log") {
		t.Fatal("lookup overlapping a command was cached")
	}
}

func TestConfigurationSnapshotCompleteAndWritable(t *testing.T) {
	c, _ := testWorker(t)
	root := c.LocalRoot
	dir := filepath.Join(root, ".claude", "commands")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 180; i++ {
		if e := os.WriteFile(filepath.Join(dir, fmt.Sprintf("command-%03d.md", i)), []byte(fmt.Sprintf("Description %d\n", i)), 0600); e != nil {
			t.Fatal(e)
		}
	}
	settings := filepath.Join(root, ".claude", "settings.json")
	if e := os.WriteFile(settings, []byte(`{"model":"sonnet","hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo OK"}]}]}}`), 0600); e != nil {
		t.Fatal(e)
	}
	client := testCachedTransport(t, c)
	read := func(name string) []byte {
		t.Helper()
		f, e := client.Open(name)
		if e != nil {
			t.Fatal(e)
		}
		b, e := io.ReadAll(f)
		f.Close()
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	list, e := client.ReadDir("/.claude/commands")
	if e != nil || len(list) != 180 {
		t.Fatalf("incomplete listing: %d, %v", len(list), e)
	}
	for i := 0; i < 180; i++ {
		want := fmt.Sprintf("Description %d\n", i)
		if got := string(read(fmt.Sprintf("/.claude/commands/command-%03d.md", i))); got != want {
			t.Fatalf("file %d: %q", i, got)
		}
	}
	projected := read("/.claude/settings.json")
	if !bytes.Contains(projected, []byte("dispatch")) {
		t.Fatal("hook projection lost")
	}
	f, e := client.Create("/.claude/settings.json")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Write(bytes.ReplaceAll(projected, []byte("sonnet"), []byte("haiku"))); e != nil {
		t.Fatal(e)
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
	raw, _ := os.ReadFile(settings)
	if bytes.Contains(raw, []byte("dispatch")) || !bytes.Contains(raw, []byte("haiku")) {
		t.Fatalf("projected write was not restored: %s", raw)
	}
	if !bytes.Contains(read("/.claude/settings.json"), []byte("haiku")) {
		t.Fatal("own write was stale")
	}
	if e = client.Rename("/.claude/commands/command-000.md", "/.claude/commands/renamed.md"); e != nil {
		t.Fatal(e)
	}
	if string(read("/.claude/commands/renamed.md")) != "Description 0\n" {
		t.Fatal("rename was stale")
	}
	if e = client.Remove("/.claude/commands/renamed.md"); e != nil {
		t.Fatal(e)
	}
	if _, e = client.Stat("/.claude/commands/renamed.md"); !os.IsNotExist(e) {
		t.Fatalf("deleted entry persisted: %v", e)
	}
	// Same size and timestamp, different bytes: content hashes must detect it.
	p := filepath.Join(dir, "command-001.md")
	st, _ := os.Stat(p)
	if e = os.WriteFile(p, []byte("Replacement 1\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Chtimes(p, st.ModTime(), st.ModTime()); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(3 * time.Second)
	for string(read("/.claude/commands/command-001.md")) != "Replacement 1\n" {
		if time.Now().After(deadline) {
			t.Fatal("workstation edit was not refreshed within the visibility window")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if runtime.GOOS != "windows" {
		if e = os.WriteFile(filepath.Join(root, "external.txt"), []byte("linked content"), 0600); e != nil {
			t.Fatal(e)
		}
		if e = client.Symlink("../../external.txt", "/.claude/commands/link.md"); e != nil {
			t.Fatal(e)
		}
		if string(read("/.claude/commands/link.md")) != "linked content" {
			t.Fatal("symlink was not resolved by workstation")
		}
	}
}

func TestCachedConfigurationDisconnectDiscardsPartialWrite(t *testing.T) {
	c, _ := testWorker(t)
	settings := filepath.Join(c.LocalRoot, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settings), 0700)
	original := []byte(`{"model":"sonnet"}`)
	os.WriteFile(settings, original, 0600)
	client := testCachedTransport(t, c)
	f, e := client.Create("/.claude/settings.json")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = io.Copy(f, strings.NewReader(`{"model":`)); e != nil {
		t.Fatal(e)
	}
	client.Close()
	time.Sleep(100 * time.Millisecond)
	if b, e := os.ReadFile(settings); e != nil || !bytes.Equal(b, original) {
		t.Fatalf("partial write committed: %s, %v", b, e)
	}
}

func TestEmptyConfigurationAndUncachedSource(t *testing.T) {
	c, _ := testWorker(t)
	file := filepath.Join(c.LocalRoot, "source.txt")
	if e := os.WriteFile(file, []byte("before"), 0600); e != nil {
		t.Fatal(e)
	}
	client := testCachedTransport(t, c)
	if _, e := client.Stat("/.claude"); !os.IsNotExist(e) {
		t.Fatalf("empty configuration: %v", e)
	}
	read := func() string {
		t.Helper()
		f, e := client.Open("/source.txt")
		if e != nil {
			t.Fatal(e)
		}
		b, e := io.ReadAll(f)
		f.Close()
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	if read() != "before" {
		t.Fatal("wrong initial source data")
	}
	if e := os.WriteFile(file, []byte("after!"), 0600); e != nil {
		t.Fatal(e)
	}
	if read() != "after!" {
		t.Fatal("ordinary source content was cached")
	}
}

func TestGitMetadataAndLiveIndex(t *testing.T) {
	c, _ := testWorker(t)
	git := filepath.Join(c.LocalRoot, ".git")
	refs := filepath.Join(git, "refs", "heads")
	if e := os.MkdirAll(refs, 0700); e != nil {
		t.Fatal(e)
	}
	index := filepath.Join(git, "index")
	os.WriteFile(index, []byte("index one"), 0600)
	os.WriteFile(filepath.Join(git, "HEAD"), []byte("ref: refs/heads/main\n"), 0600)
	os.WriteFile(filepath.Join(refs, "main"), []byte("first\n"), 0600)
	client := testCachedTransport(t, c)
	read := func(p string) string {
		t.Helper()
		f, e := client.Open(p)
		if e != nil {
			t.Fatal(e)
		}
		b, e := io.ReadAll(f)
		f.Close()
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	if st, e := client.Stat("/"); e != nil || !st.IsDir() {
		t.Fatalf("root metadata: %v, %v", st, e)
	}
	if st, e := client.Stat("/.git/index"); e != nil || st.Size() != 9 {
		t.Fatalf("index metadata: %v, %v", st, e)
	}
	if _, e := client.Stat("/.git/worktrees"); !os.IsNotExist(e) {
		t.Fatalf("negative Git metadata: %v", e)
	}
	if got := read("/.git/HEAD"); got != "ref: refs/heads/main\n" {
		t.Fatalf("HEAD: %q", got)
	}
	if got := read("/.git/index"); got != "index one" {
		t.Fatalf("index content was mistaken for empty cached data: %q", got)
	}
	os.WriteFile(index, []byte("index two"), 0600)
	if got := read("/.git/index"); got != "index two" {
		t.Fatalf("index content was cached: %q", got)
	}
	os.WriteFile(filepath.Join(c.LocalRoot, "new-source.txt"), []byte("live"), 0600)
	list, e := client.ReadDir("/")
	found := false
	for _, st := range list {
		found = found || st.Name() == "new-source.txt"
	}
	if e != nil || !found {
		t.Fatalf("root listing was cached: %v", e)
	}
	os.WriteFile(filepath.Join(refs, "main"), []byte("second\n"), 0600)
	deadline := time.Now().Add(3 * time.Second)
	for read("/.git/refs/heads/main") != "second\n" {
		if time.Now().After(deadline) {
			t.Fatal("local ref change exceeded the visibility window")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A 0.1.2 client never sends Git/root entries. It must keep delegating
	// these paths rather than returning ENOENT from its config-only snapshot.
	backendSSH, e := dialBridge(c)
	if e != nil {
		t.Fatal(e)
	}
	defer backendSSH.Close()
	backend, e := sftp.NewClient(backendSSH)
	if e != nil {
		t.Fatal(e)
	}
	defer backend.Close()
	oldClientCache := &cachedSFTP{ssh: backendSSH, files: backend}
	if _, cached, e := oldClientCache.lookup(".git/HEAD"); e != nil || cached {
		t.Fatalf("old client Git cache was incorrectly enabled: %v, %v", cached, e)
	}
}

func TestAdditionalInstructionsStayCompleteAndHelpersStayLive(t *testing.T) {
	c, _ := testWorker(t)
	dir := filepath.Join(c.LocalRoot, ".claude")
	if e := os.MkdirAll(filepath.Join(dir, "helpers"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.MkdirAll(filepath.Join(c.LocalRoot, ".git", "info"), 0700); e != nil {
		t.Fatal(e)
	}
	files := map[string]string{"AGENTS.md": "root instructions", ".claude/CLAUDE.md": "project instructions", ".claude/scheduled_tasks.json": "[]", ".claude/helper.txt": "live helper", ".gitignore": "ignored-root", ".rgignore": "ignored-search", ".ignore": "ignored-generic", ".git/info/exclude": "ignored-local"}
	for p, contents := range files {
		if e := os.WriteFile(filepath.Join(c.LocalRoot, filepath.FromSlash(p)), []byte(contents), 0600); e != nil {
			t.Fatal(e)
		}
	}
	client := testCachedTransport(t, c)
	read := func(p string) string {
		t.Helper()
		f, e := client.Open("/" + p)
		if e != nil {
			t.Fatal(e)
		}
		defer f.Close()
		data, e := io.ReadAll(f)
		if e != nil {
			t.Fatal(e)
		}
		return string(data)
	}
	for p, want := range files {
		if got := read(p); got != want {
			t.Fatalf("additional file %s: %q", p, got)
		}
	}
	os.WriteFile(filepath.Join(dir, "helper.txt"), []byte("edited helper"), 0600)
	os.WriteFile(filepath.Join(dir, "helpers", "new.txt"), []byte("new"), 0600)
	if got := read(".claude/helper.txt"); got != "edited helper" {
		t.Fatalf("helper content was cached: %q", got)
	}
	if entries, e := client.ReadDir("/.claude/helpers"); e != nil || len(entries) != 1 || entries[0].Name() != "new.txt" {
		t.Fatalf("helper listing was cached: %v, %v", entries, e)
	}
	os.WriteFile(filepath.Join(c.LocalRoot, "AGENTS.md"), []byte("edited instructions"), 0600)
	deadline := time.Now().Add(3 * time.Second)
	for read("AGENTS.md") != "edited instructions" {
		if time.Now().After(deadline) {
			t.Fatal("instruction edit exceeded the visibility window")
		}
		time.Sleep(20 * time.Millisecond)
	}
	backend, e := dialBridge(c)
	if e != nil {
		t.Fatal(e)
	}
	defer backend.Close()
	backendFiles, e := sftp.NewClient(backend)
	if e != nil {
		t.Fatal(e)
	}
	defer backendFiles.Close()
	old := &cachedSFTP{ssh: backend, files: backendFiles, gitSnapshot: true}
	for p := range files {
		if _, cached, e := old.lookup(p); e != nil || cached {
			t.Fatalf("old client was assumed to supply %s: %v, %v", p, cached, e)
		}
	}
	// A previous client supplies .git/info metadata but no children. Its
	// directory listing must keep coming from the workstation.
	snapshot, e := old.snapshot()
	if e != nil {
		t.Fatal(e)
	}
	for name := range snapshot.Entries {
		if strings.HasPrefix(name, ".git/info/") {
			delete(snapshot.Entries, name)
		}
	}
	old.current.Store(&configCacheState{snapshot: snapshot, checked: time.Now()})
	listing, e := old.Filelist(sftp.NewRequest("List", "/.git/info"))
	if e != nil {
		t.Fatal(e)
	}
	entries := make([]os.FileInfo, 4)
	if n, e := listing.ListAt(entries, 0); n != 1 || (e != nil && e != io.EOF) || entries[0].Name() != "exclude" {
		t.Fatalf("old client Git info listing was incomplete: %d, %v", n, e)
	}
}

func TestConfigurationRefreshNeverServesIndefinitelyStaleData(t *testing.T) {
	c, _ := testWorker(t)
	ssh, e := dialBridge(c)
	if e != nil {
		t.Fatal(e)
	}
	cache := &cachedSFTP{ssh: ssh, gitSnapshot: true}
	initial, e := cache.snapshot()
	if e != nil {
		t.Fatal(e)
	}
	ssh.Close()
	cache.current.Store(&configCacheState{snapshot: initial, checked: time.Now().Add(-2 * time.Second)})
	if s, e := cache.snapshot(); e != nil || s.Digest != initial.Digest {
		t.Fatalf("background revalidation blocked a usable cache: %v", e)
	}
	cache.current.Store(&configCacheState{snapshot: initial, checked: time.Now().Add(-6 * time.Second)})
	if _, e := cache.snapshot(); e == nil {
		t.Fatal("disconnected cache continued serving data past its freshness limit")
	}
	cache.current.Store(&configCacheState{snapshot: initial, checked: time.Now()})
	cache.invalidate()
	if _, e := cache.snapshot(); e == nil {
		t.Fatal("own write invalidation incorrectly allowed a stale cached response")
	}
}
