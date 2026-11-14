package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestConfigSnapshotCompressionCompatibility(t *testing.T) {
	c, _ := testWorker(t)
	dir := filepath.Join(c.LocalRoot, ".claude", "commands")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	content := []byte(strings.Repeat("Instructions with UTF-8: проект, команда.\n", 12000))
	if e := os.WriteFile(filepath.Join(dir, "large.md"), content, 0600); e != nil {
		t.Fatal(e)
	}
	client, e := dialBridge(c)
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	request := func(spec map[string]any) []byte {
		t.Helper()
		var out, errout bytes.Buffer
		r, e := bridgeExecClient(client, ExecRequest{Kind: "config-snapshot", Spec: spec}, nil, &out, &errout)
		if e != nil || r.Code != 0 {
			t.Fatalf("snapshot RPC: %v, exit %d, %s", e, r.Code, errout.String())
		}
		return out.Bytes()
	}
	// Old servers do not request compression and must still receive JSON.
	plain := request(nil)
	var legacy configSnapshot
	if e := json.Unmarshal(plain, &legacy); e != nil {
		t.Fatal(e)
	}
	decoded, e := decodeConfigSnapshot(plain)
	if e != nil || !reflect.DeepEqual(decoded, legacy) {
		t.Fatalf("plain response from an old worker: %v", e)
	}
	compressed := request(map[string]any{"compression": "gzip"})
	if !bytes.HasPrefix(compressed, []byte{0x1f, 0x8b}) || len(compressed) >= len(plain)/2 {
		t.Fatalf("configuration was not compressed: plain %d, compressed %d", len(plain), len(compressed))
	}
	decoded, e = decodeConfigSnapshot(compressed)
	if e != nil || !reflect.DeepEqual(decoded, legacy) {
		t.Fatalf("compressed metadata or contents changed: %v", e)
	}
	if !bytes.Equal(decoded.Entries[".claude/commands/large.md"].Data, content) {
		t.Fatal("configuration contents lost in transport")
	}
	unchanged := request(map[string]any{"compression": "gzip", "digest": decoded.Digest})
	var reply configSnapshot
	if e := json.Unmarshal(unchanged, &reply); e != nil || reply.Digest != decoded.Digest || reply.Entries != nil {
		t.Fatalf("unchanged snapshot should only return its digest: %v", e)
	}
	if got := request(map[string]any{"compression": "unknown"}); !bytes.Equal(got, plain) {
		t.Fatal("an unsupported compression request did not retain plain JSON")
	}
}

func TestDecodeConfigSnapshotRejectsBrokenCompression(t *testing.T) {
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	w.Write([]byte(`{"digest":"example","entries":{}}`))
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	valid := out.Bytes()
	badChecksum := append([]byte(nil), valid...)
	badChecksum[len(badChecksum)-8] ^= 1
	for name, data := range map[string][]byte{
		"header":   {0x1f, 0x8b, 0},
		"trailer":  valid[:len(valid)-8],
		"checksum": badChecksum,
		"json":     []byte(`{"digest":`),
		"digest":   []byte(`{"entries":{}}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, e := decodeConfigSnapshot(data); e == nil {
				t.Fatal("accepted incomplete or corrupted configuration")
			}
		})
	}
}

func TestDecodeConfigSnapshotBoundsExpansion(t *testing.T) {
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	block := bytes.Repeat([]byte(" "), 32<<10)
	for n := 0; n < maxConfigSnapshotBytes/len(block)+1; n++ {
		if _, e := w.Write(block); e != nil {
			t.Fatal(e)
		}
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e := decodeConfigSnapshot(out.Bytes()); e == nil || !strings.Contains(e.Error(), "exceeds 64 MiB") {
		t.Fatalf("unbounded decompression: %v", e)
	}
}
