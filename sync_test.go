package main

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

type syncPair struct {
	t                   *testing.T
	pcDir, repDir, live string
	state, stash        string
	pc                  *syncPC
	srv                 *syncServer
	pcRoot              *os.Root
	pcConn, srvConn     net.Conn
}

func newSyncPair(t *testing.T) *syncPair {
	t.Helper()
	d := t.TempDir()
	p := &syncPair{t: t, pcDir: filepath.Join(d, "pc"), repDir: filepath.Join(d, "replica"), live: filepath.Join(d, "live"), state: filepath.Join(d, "state.gob"), stash: filepath.Join(d, "stash")}
	if err := os.MkdirAll(p.pcDir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.disconnect()
		if p.srv != nil {
			p.srv.close()
		}
	})
	return p
}

func (p *syncPair) openServer() {
	p.t.Helper()
	srv, err := openSyncServer(p.repDir, p.live, p.pcDir, p.state, p.stash)
	if err != nil {
		p.t.Fatal(err)
	}
	p.srv = srv
}

func (p *syncPair) connect() {
	p.t.Helper()
	p.connectWith(func(path string, data []byte, unwrap bool) ([]byte, error) {
		return projectJSON(data, unwrap)
	})
}

func (p *syncPair) connectWith(project func(string, []byte, bool) ([]byte, error)) {
	p.t.Helper()
	if p.srv == nil {
		p.openServer()
	}
	root, err := os.OpenRoot(p.pcDir)
	if err != nil {
		p.t.Fatal(err)
	}
	p.pcRoot = root
	p.pc = newSyncPC(root, p.pcDir, p.repDir, &ignoreRules{}, project)
	a, b := net.Pipe()
	p.pcConn, p.srvConn = a, b
	go p.pc.serve(a)
	p.srv.serve(b)
	select {
	case <-p.pc.ready:
	case <-time.After(10 * time.Second):
		p.t.Fatal("initial synchronisation did not finish")
	}
}

func (p *syncPair) disconnect() {
	if p.pcConn != nil {
		p.pcConn.Close()
		p.srvConn.Close()
		p.pc.close()
		p.pcRoot.Close()
		p.pcConn = nil
		deadline := time.Now().Add(5 * time.Second)
		for p.srv.current() != nil && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// toServer is the barrier after a workstation command.
func (p *syncPair) toServer() {
	p.t.Helper()
	seq, epoch, ok := p.pc.barrier(5 * time.Second)
	if !ok || !p.srv.waitApplied(seq, epoch, 5*time.Second) {
		p.t.Fatal("workstation changes did not reach the replica")
	}
}

// toPC is the barrier before a workstation command.
func (p *syncPair) toPC() {
	p.t.Helper()
	seq, epoch, ok := p.srv.flush(5 * time.Second)
	if !ok || !p.pc.waitApplied(seq, epoch, 5*time.Second) {
		p.t.Fatal("replica changes did not reach the workstation")
	}
	// Acknowledgements travel back before the next step inspects state.
	p.toServer()
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertMissing(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatalf("%s should not exist (err=%v)", p, err)
	}
}

func TestSyncInitialReplicaAndLinks(t *testing.T) {
	p := newSyncPair(t)
	writeTree(t, p.pcDir, map[string]string{
		"main.go":                 "package main\n",
		"src/a/b.txt":             "nested",
		".gitignore":              "node_modules/\n*.log\n",
		"debug.log":               "ignored",
		"node_modules/x/index.js": "dep",
		".git/HEAD":               "ref: refs/heads/main\n",
		".env":                    "SECRET=1",
	})
	if runtime.GOOS != "windows" {
		os.Symlink("main.go", filepath.Join(p.pcDir, "alias.go"))
		os.Chmod(filepath.Join(p.pcDir, "main.go"), 0755)
	}
	p.connect()
	if got := readFile(t, filepath.Join(p.repDir, "src/a/b.txt")); got != "nested" {
		t.Fatalf("nested file: %q", got)
	}
	for _, link := range []string{"node_modules", "debug.log", ".git"} {
		target, err := os.Readlink(filepath.Join(p.repDir, link))
		if err != nil || target != filepath.Join(p.live, link) {
			t.Fatalf("%s should link into the live view, got %q %v", link, target, err)
		}
	}
	if got := readFile(t, filepath.Join(p.repDir, ".env")); got != "SECRET=1" {
		t.Fatalf("unignored dotfile: %q", got)
	}
	if runtime.GOOS != "windows" {
		if target, _ := os.Readlink(filepath.Join(p.repDir, "alias.go")); target != "main.go" {
			t.Fatalf("relative symlink: %q", target)
		}
		if st, _ := os.Stat(filepath.Join(p.repDir, "main.go")); st.Mode().Perm()&0100 == 0 {
			t.Fatal("executable bit was not replicated")
		}
	}
}

func TestSyncWorkstationChangesReachReplica(t *testing.T) {
	p := newSyncPair(t)
	writeTree(t, p.pcDir, map[string]string{"a.txt": "one", "dir/b.txt": "two", "dir/sub/c.txt": "three"})
	p.connect()
	writeTree(t, p.pcDir, map[string]string{"a.txt": "one changed", "new/file.txt": "created"})
	os.RemoveAll(filepath.Join(p.pcDir, "dir"))
	p.toServer()
	if got := readFile(t, filepath.Join(p.repDir, "a.txt")); got != "one changed" {
		t.Fatalf("modified file: %q", got)
	}
	if got := readFile(t, filepath.Join(p.repDir, "new/file.txt")); got != "created" {
		t.Fatalf("created file: %q", got)
	}
	assertMissing(t, filepath.Join(p.repDir, "dir"))
	// A path becoming ignored turns into a link; the copy disappears.
	writeTree(t, p.pcDir, map[string]string{".gitignore": "new/\n"})
	p.toServer()
	if target, err := os.Readlink(filepath.Join(p.repDir, "new")); err != nil || target != filepath.Join(p.live, "new") {
		t.Fatalf("newly ignored directory: %q %v", target, err)
	}
}

func TestSyncReplicaChangesReachWorkstation(t *testing.T) {
	p := newSyncPair(t)
	writeTree(t, p.pcDir, map[string]string{"a.txt": "one", "keep.txt": "keep"})
	p.connect()
	writeTree(t, p.repDir, map[string]string{"a.txt": "edited by claude", "made/by/claude.txt": "new"})
	p.toPC()
	if got := readFile(t, filepath.Join(p.pcDir, "a.txt")); got != "edited by claude" {
		t.Fatalf("edit: %q", got)
	}
	if got := readFile(t, filepath.Join(p.pcDir, "made/by/claude.txt")); got != "new" {
		t.Fatalf("new file: %q", got)
	}
	// No echo: the workstation copy written by synchronisation is not sent back.
	if n := len(p.srv.takeNotices()); n != 0 {
		t.Fatalf("unexpected conflict notices: %d", n)
	}
	os.Remove(filepath.Join(p.repDir, "keep.txt"))
	p.toPC()
	assertMissing(t, filepath.Join(p.pcDir, "keep.txt"))
}

func TestSyncConflictKeepsWorkstationVersion(t *testing.T) {
	p := newSyncPair(t)
	writeTree(t, p.pcDir, map[string]string{"shared.txt": "base"})
	p.connect()
	p.disconnect()
	// Both sides edit before either change is synchronised.
	writeTree(t, p.repDir, map[string]string{"shared.txt": "claude"})
	writeTree(t, p.pcDir, map[string]string{"shared.txt": "human"})
	p.connect()
	p.toServer()
	if got := readFile(t, filepath.Join(p.pcDir, "shared.txt")); got != "human" {
		t.Fatalf("workstation copy: %q", got)
	}
	if got := readFile(t, filepath.Join(p.repDir, "shared.txt")); got != "human" {
		t.Fatalf("replica copy: %q", got)
	}
	notices := p.srv.takeNotices()
	if len(notices) != 1 || !strings.Contains(notices[0], "shared.txt") {
		t.Fatalf("conflict notice: %v", notices)
	}
	stashed, _ := filepath.Glob(filepath.Join(p.stash, "*", "shared.txt"))
	if len(stashed) != 1 || readFile(t, stashed[0]) != "claude" {
		t.Fatalf("Claude's version was not kept: %v", stashed)
	}
}

func TestSyncReconnectReconcilesOfflineEdits(t *testing.T) {
	p := newSyncPair(t)
	writeTree(t, p.pcDir, map[string]string{"pc.txt": "pc", "claude.txt": "claude", "both.txt": "same", "gone.txt": "x"})
	p.connect()
	p.disconnect()
	// While disconnected Claude edits one file and the developer another.
	writeTree(t, p.repDir, map[string]string{"claude.txt": "claude offline edit", "offline-new.txt": "new"})
	writeTree(t, p.pcDir, map[string]string{"pc.txt": "pc offline edit"})
	os.Remove(filepath.Join(p.pcDir, "gone.txt"))
	// The host process restarts: state comes from disk.
	p.srv.close()
	p.srv = nil
	p.connect()
	p.toServer()
	if got := readFile(t, filepath.Join(p.pcDir, "claude.txt")); got != "claude offline edit" {
		t.Fatalf("offline Claude edit: %q", got)
	}
	if got := readFile(t, filepath.Join(p.pcDir, "offline-new.txt")); got != "new" {
		t.Fatalf("offline Claude file: %q", got)
	}
	if got := readFile(t, filepath.Join(p.repDir, "pc.txt")); got != "pc offline edit" {
		t.Fatalf("offline workstation edit: %q", got)
	}
	assertMissing(t, filepath.Join(p.repDir, "gone.txt"))
	if n := p.srv.takeNotices(); len(n) != 0 {
		t.Fatalf("no conflict expected: %v", n)
	}
}

func TestSyncFreshReplicaDropsLeftovers(t *testing.T) {
	p := newSyncPair(t)
	writeTree(t, p.repDir, map[string]string{"stale.txt": "from an old session", "main.go": "old"})
	writeTree(t, p.pcDir, map[string]string{"main.go": "current"})
	p.connect()
	assertMissing(t, filepath.Join(p.repDir, "stale.txt"))
	assertMissing(t, filepath.Join(p.pcDir, "stale.txt"))
	if got := readFile(t, filepath.Join(p.repDir, "main.go")); got != "current" {
		t.Fatalf("replica: %q", got)
	}
}

func TestSyncProjectsClaudeHooks(t *testing.T) {
	p := newSyncPair(t)
	settings := `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"./check.sh"}]}]}}`
	writeTree(t, p.pcDir, map[string]string{".claude/settings.json": settings})
	p.connect()
	replica := readFile(t, filepath.Join(p.repDir, ".claude/settings.json"))
	if !strings.Contains(replica, "dispatch") || strings.Contains(replica, "./check.sh") {
		t.Fatalf("replica settings are not projected: %s", replica)
	}
	// Claude edits the projected file; the workstation receives plain settings.
	edited := strings.Replace(replica, `"PreToolUse"`, `"PostToolUse"`, 1)
	writeTree(t, p.repDir, map[string]string{".claude/settings.json": edited})
	p.toPC()
	local := readFile(t, filepath.Join(p.pcDir, ".claude/settings.json"))
	if !strings.Contains(local, "./check.sh") || !strings.Contains(local, "PostToolUse") || strings.Contains(local, "dispatch") {
		t.Fatalf("workstation settings: %s", local)
	}
	// And the round trip produced no further transfer or conflict.
	p.toServer()
	if n := p.srv.takeNotices(); len(n) != 0 {
		t.Fatalf("conflict after projection round trip: %v", n)
	}
}

func TestSyncLargeFileStaysLinked(t *testing.T) {
	p := newSyncPair(t)
	big := make([]byte, maxReplicaFile+1)
	if err := os.WriteFile(filepath.Join(p.pcDir, "big.bin"), big, 0644); err != nil {
		t.Fatal(err)
	}
	p.connect()
	if target, err := os.Readlink(filepath.Join(p.repDir, "big.bin")); err != nil || target != filepath.Join(p.live, "big.bin") {
		t.Fatalf("large file should be linked: %q %v", target, err)
	}
}

func TestCleanSyncPathRejectsEscapes(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "../x", "/etc/passwd", "a/../../x", "a//b", "x/.rc-sync-abc"} {
		if _, ok := cleanSyncPath(bad); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
	if p, ok := cleanSyncPath("a/b.txt"); !ok || p != "a/b.txt" {
		t.Fatal("valid path rejected")
	}
}

func TestSyncReprojectsConfigurationOnEveryConnection(t *testing.T) {
	p := newSyncPair(t)
	writeTree(t, p.pcDir, map[string]string{".mcp.json": `{"mcpServers":{"x":{"command":"tool"}}}`})
	generation := "one"
	project := func(path string, data []byte, unwrap bool) ([]byte, error) {
		if unwrap {
			return []byte(strings.ReplaceAll(string(data), "-"+generation, "")), nil
		}
		return []byte(strings.Replace(string(data), `"tool"`, `"tool-`+generation+`"`, 1)), nil
	}
	p.connectWith(project)
	if got := readFile(t, filepath.Join(p.repDir, ".mcp.json")); !strings.Contains(got, "tool-one") {
		t.Fatalf("first projection: %s", got)
	}
	p.disconnect()
	generation = "two"
	p.connectWith(project)
	if got := readFile(t, filepath.Join(p.repDir, ".mcp.json")); !strings.Contains(got, "tool-two") {
		t.Fatalf("projection was not renewed for the new connection: %s", got)
	}
	if got := readFile(t, filepath.Join(p.pcDir, ".mcp.json")); strings.Contains(got, "tool-") {
		t.Fatalf("workstation file changed: %s", got)
	}
}

// settle runs barriers until both sides stop exchanging changes.
func (p *syncPair) settle() {
	p.t.Helper()
	for i := 0; i < 4; i++ {
		p.toPC()
		p.toServer()
	}
}

func TestSyncRapidReplicaEditsConverge(t *testing.T) {
	p := newSyncPair(t)
	writeTree(t, p.pcDir, map[string]string{"f.txt": "v0"})
	p.connect()
	for i := 1; i <= 20; i++ {
		writeTree(t, p.repDir, map[string]string{"f.txt": "v" + string(rune('a'+i))})
		if i%3 == 0 {
			p.srv.flush(5 * time.Second) // a push in flight while the next edit lands
		}
	}
	p.settle()
	want := "v" + string(rune('a'+20))
	if got := readFile(t, filepath.Join(p.pcDir, "f.txt")); got != want {
		t.Fatalf("workstation: %q, want %q", got, want)
	}
	// The base must agree with both copies: a later workstation edit wins
	// without a conflict.
	writeTree(t, p.pcDir, map[string]string{"f.txt": "human"})
	p.settle()
	if got := readFile(t, filepath.Join(p.repDir, "f.txt")); got != "human" {
		t.Fatalf("replica after workstation edit: %q", got)
	}
	if n := p.srv.takeNotices(); len(n) != 0 {
		t.Fatalf("spurious conflict: %v", n)
	}
}

func TestSyncPlainHookWrittenByClaudeIsProjected(t *testing.T) {
	p := newSyncPair(t)
	writeTree(t, p.pcDir, map[string]string{"a.txt": "x"})
	p.connect()
	plain := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"./notify.sh"}]}]}}`
	writeTree(t, p.repDir, map[string]string{".claude/settings.json": plain})
	p.settle()
	if got := readFile(t, filepath.Join(p.pcDir, ".claude/settings.json")); !strings.Contains(got, "./notify.sh") || strings.Contains(got, "dispatch") {
		t.Fatalf("workstation settings: %s", got)
	}
	if got := readFile(t, filepath.Join(p.repDir, ".claude/settings.json")); !strings.Contains(got, "dispatch") {
		t.Fatalf("replica keeps a hook that would run on the server: %s", got)
	}
}

func TestSyncReplicaDeleteKeepsWorkstationOnlyFiles(t *testing.T) {
	p := newSyncPair(t)
	writeTree(t, p.pcDir, map[string]string{".gitignore": "d/.env\n", "d/a.txt": "a", "d/.env": "SECRET"})
	p.connect()
	os.RemoveAll(filepath.Join(p.repDir, "d"))
	p.settle()
	if got := readFile(t, filepath.Join(p.pcDir, "d/.env")); got != "SECRET" {
		t.Fatalf("ignored workstation file: %q", got)
	}
	// The directory stays, with its link to the workstation-only file; a.txt
	// may be deleted (Claude deleted it) but both sides must agree.
	if target, err := os.Readlink(filepath.Join(p.repDir, "d/.env")); err != nil || target != filepath.Join(p.live, "d/.env") {
		t.Fatalf("replica link: %q %v", target, err)
	}
	_, pcErr := os.Stat(filepath.Join(p.pcDir, "d/a.txt"))
	_, repErr := os.Stat(filepath.Join(p.repDir, "d/a.txt"))
	if (pcErr == nil) != (repErr == nil) {
		t.Fatalf("sides disagree about d/a.txt: workstation %v, replica %v", pcErr, repErr)
	}
}

func TestSyncReplicaCannotReplaceWorkstationFileWithDirectory(t *testing.T) {
	p := newSyncPair(t)
	writeTree(t, p.pcDir, map[string]string{"a.txt": "a"})
	p.connect()
	p.disconnect()
	writeTree(t, p.pcDir, map[string]string{"foo": "workstation file"})
	writeTree(t, p.repDir, map[string]string{"foo/new.txt": "claude"})
	p.connect()
	p.settle()
	if got := readFile(t, filepath.Join(p.pcDir, "foo")); got != "workstation file" {
		t.Fatalf("workstation file replaced: %q", got)
	}
	if got := readFile(t, filepath.Join(p.repDir, "foo")); got != "workstation file" {
		t.Fatalf("replica: %q", got)
	}
}

func TestSyncShallowCopiesNothing(t *testing.T) {
	p := newSyncPair(t)
	writeTree(t, p.pcDir, map[string]string{"Downloads/big.iso": "data", ".ssh/id_ed25519": "secret", "notes.txt": "n"})
	p.openServer()
	root, err := os.OpenRoot(p.pcDir)
	if err != nil {
		t.Fatal(err)
	}
	p.pcRoot = root
	p.pc = newSyncPC(root, p.pcDir, p.repDir, &ignoreRules{}, nil)
	p.pc.scanner.shallow = true
	a, b := net.Pipe()
	p.pcConn, p.srvConn = a, b
	go p.pc.serve(a)
	p.srv.serve(b)
	<-p.pc.ready
	for _, name := range []string{"Downloads", ".ssh", "notes.txt"} {
		target, err := os.Readlink(filepath.Join(p.repDir, name))
		if err != nil || target != filepath.Join(p.live, name) {
			t.Fatalf("%s should be a link into the live view: %q %v", name, target, err)
		}
	}
	// A file Claude creates at the top reaches the workstation and becomes a
	// link too; nothing stays copied on the server.
	writeTree(t, p.repDir, map[string]string{"todo.md": "from claude"})
	p.settle()
	if got := readFile(t, filepath.Join(p.pcDir, "todo.md")); got != "from claude" {
		t.Fatalf("workstation: %q", got)
	}
	if target, err := os.Readlink(filepath.Join(p.repDir, "todo.md")); err != nil || target != filepath.Join(p.live, "todo.md") {
		t.Fatalf("todo.md should become a link: %q %v", target, err)
	}
}

func TestReplicaOnlyForReasonableGitProjects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if ok, _ := replicaSuitable(home, &ignoreRules{git: true}); ok {
		t.Fatal("home directory replicated")
	}
	if ok, _ := replicaSuitable("/", &ignoreRules{git: true}); ok {
		t.Fatal("filesystem root replicated")
	}
	plain := filepath.Join(home, "plain")
	writeTree(t, plain, map[string]string{"a.txt": "a"})
	if ok, _ := replicaSuitable(plain, &ignoreRules{}); ok {
		t.Fatal("directory outside Git replicated")
	}
	if ok, why := replicaSuitable(plain, &ignoreRules{git: true}); !ok {
		t.Fatalf("small Git project not replicated: %s", why)
	}
	big := filepath.Join(home, "big")
	files := map[string]string{}
	for i := 0; i <= replicaMaxEntries; i++ {
		files[filepath.Join("d", string(rune('a'+i%26)), strconv.Itoa(i))] = ""
	}
	writeTree(t, big, files)
	if ok, _ := replicaSuitable(big, &ignoreRules{git: true}); ok {
		t.Fatal("oversized project replicated")
	}
}
