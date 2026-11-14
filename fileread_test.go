package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
)

func TestPerOpenSnapshotsAreFreshAndReleaseBudget(t *testing.T) {
	c, _ := testWorker(t)
	client, err := dialBridge(c)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	files, err := sftp.NewClient(client)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	f := &cachedSFTP{ssh: client, files: files, readSlots: make(chan struct{}, 1)}
	p := filepath.Join(c.LocalRoot, "source.txt")
	os.WriteFile(p, []byte("before"), 0600)
	first, err := f.Fileread(sftp.NewRequest("Get", "/source.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.readSlots) != 1 {
		t.Fatal("snapshot budget not reserved")
	}
	os.WriteFile(p, []byte("after edit, longer"), 0600)
	second, err := f.Fileread(sftp.NewRequest("Get", "/source.txt"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(io.NewSectionReader(second, 0, 100))
	if err != nil || string(b) != "after edit, longer" {
		t.Fatalf("%q %v", b, err)
	}
	second.(io.Closer).Close()
	b, err = io.ReadAll(io.NewSectionReader(first, 0, 100))
	if err != nil || string(b) != "before" {
		t.Fatalf("%q %v", b, err)
	}
	first.(io.Closer).Close()
	first.(io.Closer).Close()
	if len(f.readSlots) != 0 {
		t.Fatal("snapshot budget leaked")
	}
	third, err := f.Fileread(sftp.NewRequest("Get", "/source.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer third.(io.Closer).Close()
	b, _ = io.ReadAll(io.NewSectionReader(third, 0, 100))
	if string(b) != "after edit, longer" {
		t.Fatalf("stale: %q", b)
	}
}

func TestReadSnapshotFallsBackForLargeMissingAndProjectedFiles(t *testing.T) {
	c, _ := testWorker(t)
	os.WriteFile(filepath.Join(c.LocalRoot, "large.bin"), bytes.Repeat([]byte("x"), maxReadSnapshot+1), 0600)
	os.WriteFile(filepath.Join(c.LocalRoot, ".mcp.json"), []byte(`{"mcpServers":{}}`), 0600)
	for _, p := range []string{"large.bin", "absent", ".mcp.json"} {
		var out bytes.Buffer
		r, err := bridgeExec(c, ExecRequest{Kind: "read-file", Spec: map[string]any{"path": p}}, nil, &out, io.Discard)
		if err != nil || r.Code != 3 || out.Len() != 0 {
			t.Fatalf("%s: %+v %v bytes=%d", p, r, err, out.Len())
		}
	}
}
