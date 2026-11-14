package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Client builds are published on the VPS, so an update arrives over the same
// pinned SSH connection as everything else and needs no other access.
const clientReleases = "/opt/remote-claude/clients"

var errRestart = errors.New("restart after update")

type clientRelease struct {
	Version     string                       `json:"version"`
	Commit      string                       `json:"commit,omitempty"`
	Development bool                         `json:"development,omitempty"`
	Files       map[string]map[string]string `json:"files"`
}

var releaseVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}$`)

func newerVersion(candidate, current string) bool {
	a, b := strings.Split(candidate, "."), strings.Split(current, ".")
	for i := 0; i < len(a) || i < len(b); i++ {
		x, y := 0, 0
		if i < len(a) {
			x, _ = strconv.Atoi(a[i])
		}
		if i < len(b) {
			y, _ = strconv.Atoi(b[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func clientFileNames() (string, string) {
	if runtime.GOOS == "windows" {
		return "remote-claude.exe", "remote-claude-rg.exe"
	}
	return "remote-claude", "remote-claude-rg"
}

// updateFiles returns the checksums of a newer build for this platform. Any
// malformed or incomplete manifest means there is nothing to offer.
func updateFiles(manifest []byte, current, platform string) (string, map[string]string) {
	var r clientRelease
	if json.Unmarshal(manifest, &r) != nil || r.Development || !releaseVersion.MatchString(r.Version) || !newerVersion(r.Version, current) {
		return "", nil
	}
	executable, _ := clientFileNames()
	files := r.Files[platform]
	if len(files[executable]) != sha256.Size*2 {
		return "", nil
	}
	return r.Version, files
}

func availableUpdate(c *ssh.Client) (string, map[string]string) {
	b, e := runSSH(c, "cat "+clientReleases+"/manifest.json", nil)
	if e != nil {
		return "", nil
	}
	return updateFiles(b, version, runtime.GOOS+"-"+runtime.GOARCH)
}

// Every command that connects to a pinned server can update before doing
// work. A failed download leaves the old executable in place and does not
// prevent a connection to a compatible server.
func installAvailableUpdate(c *ssh.Client) error {
	release, files := availableUpdate(c)
	if release == "" {
		return nil
	}
	if e := applyUpdate(c, release, files); e != nil {
		fmt.Fprintln(os.Stderr, "remote-claude: update to", release, "failed; continuing with", version+":", e)
		return nil
	}
	fmt.Fprintln(os.Stderr, "remote-claude: updated", version, "to", release, "and restarting")
	return errRestart
}

func fileDigest(path string) string {
	b, e := os.ReadFile(path)
	if e != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func applyUpdate(c *ssh.Client, release string, files map[string]string) error {
	current, e := os.Executable()
	if e != nil {
		return e
	}
	if current, e = filepath.EvalSymlinks(current); e != nil {
		return e
	}
	dir := filepath.Dir(current)
	executable, search := clientFileNames()
	// The executable goes last: a failure before it leaves the old client intact.
	for _, name := range []string{search, executable} {
		want := files[name]
		target := filepath.Join(dir, name)
		if name == executable {
			target = current
		}
		if want == "" || fileDigest(target) == want {
			continue
		}
		b, e := runSSH(c, "cat "+clientReleases+"/"+release+"/"+runtime.GOOS+"-"+runtime.GOARCH+"/"+name, nil)
		if e != nil {
			return e
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != want {
			return fmt.Errorf("checksum mismatch for %s", name)
		}
		f, e := os.CreateTemp(dir, ".rc-update-*")
		if e != nil {
			return e
		}
		temporary := f.Name()
		_, e = f.Write(b)
		if x := f.Close(); e == nil {
			e = x
		}
		if e == nil {
			e = os.Chmod(temporary, 0755)
		}
		if e == nil && runtime.GOOS == "windows" && name == executable {
			// A running executable cannot be overwritten on Windows, only renamed.
			os.Remove(target + ".old")
			e = os.Rename(target, target+".old")
		}
		if e == nil {
			e = replaceFile(temporary, target)
		}
		if e != nil {
			os.Remove(temporary)
			return e
		}
	}
	return nil
}
