package main

import (
	"strings"
	"testing"
)

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		candidate, current string
		newer              bool
	}{
		{"0.1.8", "0.1.7", true}, {"0.1.10", "0.1.9", true}, {"0.2", "0.1.9", true}, {"1.0.0", "0.9.9", true},
		{"0.1.7", "0.1.7", false}, {"0.1.6", "0.1.7", false}, {"0.1", "0.1.0", false}, {"0.1.7", "0.1.10", false},
	} {
		if newerVersion(c.candidate, c.current) != c.newer {
			t.Fatalf("newerVersion(%q, %q) != %v", c.candidate, c.current, c.newer)
		}
	}
}

func TestUpdateFilesRejectsDevelopmentManifest(t *testing.T) {
	executable, _ := clientFileNames()
	digest := strings.Repeat("ab", 32)
	development := []byte(`{"version":"0.2.0","commit":"0123456789abcdef0123456789abcdef01234567","development":true,"files":{"linux-amd64":{"` + executable + `":"` + digest + `"}}}`)
	if release, _ := updateFiles(development, "0.1.14", "linux-amd64"); release != "" {
		t.Fatalf("development manifest offered an update: %q", release)
	}
	release := []byte(`{"version":"0.2.0","commit":"0123456789abcdef0123456789abcdef01234567","development":false,"files":{"linux-amd64":{"` + executable + `":"` + digest + `"}}}`)
	if version, files := updateFiles(release, "0.1.14", "linux-amd64"); version != "0.2.0" || files[executable] != digest {
		t.Fatalf("release manifest with provenance was rejected: %q %v", version, files)
	}
}

func TestUpdateFiles(t *testing.T) {
	executable, _ := clientFileNames()
	digest := strings.Repeat("ab", 32)
	manifest := func(release, platform, sum string) []byte {
		return []byte(`{"version":"` + release + `","files":{"` + platform + `":{"` + executable + `":"` + sum + `"}}}`)
	}
	if release, files := updateFiles(manifest("0.1.8", "linux-amd64", digest), "0.1.7", "linux-amd64"); release != "0.1.8" || files[executable] != digest {
		t.Fatalf("newer build not offered: %q %v", release, files)
	}
	for name, b := range map[string][]byte{
		"same version":     manifest("0.1.7", "linux-amd64", digest),
		"older version":    manifest("0.1.6", "linux-amd64", digest),
		"other platform":   manifest("0.1.8", "darwin-arm64", digest),
		"short checksum":   manifest("0.1.8", "linux-amd64", "abcd"),
		"path in version":  manifest("0.1.8/../x", "linux-amd64", digest),
		"shell in version": manifest("0.1.8;id", "linux-amd64", digest),
		"not a manifest":   []byte("cat: no such file"),
		"empty":            nil,
	} {
		if release, _ := updateFiles(b, "0.1.7", "linux-amd64"); release != "" {
			t.Fatalf("%s: offered %q", name, release)
		}
	}
}
