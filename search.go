package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const serverRipgrep = "/opt/remote-claude/rg"

func workstationSearchAvailable() bool {
	p, err := workstationRipgrep()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, p, "--version").Run() == nil
}

func workstationRipgrep() (string, error) {
	if exe, err := os.Executable(); err == nil {
		name := "remote-claude-rg"
		if strings.HasSuffix(strings.ToLower(exe), ".exe") {
			name += ".exe"
		}
		p := filepath.Join(filepath.Dir(exe), name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, nil
		}
	}
	return exec.LookPath("rg")
}

// Only file paths are translated. Regexes, replacement strings and glob
// patterns are opaque. Unknown options retain the original server behavior.
// Relative output paths are intentional: no rewriting of matching source
// text, JSON records, NUL-delimited names or binary output is ever necessary.
func searchArgs(args []string, cwd, root string) ([]string, bool) {
	return prepareSearch(args, cwd, root, nil)
}

func prepareSearch(args []string, cwd, root string, pathsToCheck *[]string) ([]string, bool) {
	inside := func(p string) bool { return p == root || strings.HasPrefix(p, root+"/") }
	if !inside(cwd) {
		return nil, false
	}
	booleans := strings.Fields("--hidden --no-hidden --no-ignore --no-ignore-vcs --no-ignore-parent --no-ignore-global --no-ignore-dot --ignore-case --case-sensitive --smart-case --fixed-strings --word-regexp --line-regexp --multiline --multiline-dotall --pcre2 --auto-hybrid-regex --crlf --text --binary --no-unicode --unicode --line-number --no-line-number --column --no-column --with-filename --no-filename --heading --no-heading --files-with-matches --files-without-match --count --count-matches --only-matching --quiet --null --null-data --json --stats --no-messages --no-config --trim --vimgrep --line-buffered --block-buffered --no-require-git --one-file-system")
	values := strings.Fields("--glob --iglob --type --type-not --type-add --type-clear --encoding --engine --max-count --max-depth --max-filesize --max-columns --threads --context --after-context --before-context --context-separator --field-match-separator --field-context-separator --color --colors --sort --sortr --path-separator --regex-size-limit --dfa-size-limit --replace")
	contains := func(a []string, s string) bool {
		for _, x := range a {
			if x == s {
				return true
			}
		}
		return false
	}
	patternSet, files := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if a == "--files" {
			files = true
		}
		key, _, eq := strings.Cut(a, "=")
		if !eq && (key == "--regexp" || key == "--file" || key == "--ignore-file" || contains(values, key)) {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") {
			for j := 1; j < len(a); j++ {
				if strings.ContainsRune("efgtTEmABCjMr", rune(a[j])) {
					if j == len(a)-1 {
						i++
					}
					break
				}
			}
		}
	}
	out := make([]string, 0, len(args)+1)
	paths := 0
	mapPath := func(p string) (string, bool) {
		if p == "-" {
			return "", false
		}
		absolute := p
		if !path.IsAbs(p) {
			absolute = path.Join(cwd, p)
		}
		absolute = path.Clean(absolute)
		if !inside(absolute) {
			return "", false
		}
		// filepath.Rel is operating on server (Linux) paths here.
		rel, err := filepath.Rel(cwd, absolute)
		if err != nil {
			return "", false
		}
		if rel != "." && !strings.HasPrefix(rel, ".") {
			rel = "./" + rel
		}
		rel = filepath.ToSlash(rel)
		if pathsToCheck != nil {
			*pathsToCheck = append(*pathsToCheck, rel)
		}
		return rel, true
	}
	end := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !end && a == "--" {
			end = true
			out = append(out, a)
			continue
		}
		if !end && strings.HasPrefix(a, "--") {
			key, val, eq := strings.Cut(a, "=")
			if key == "--files" || contains(booleans, key) {
				if eq {
					return nil, false
				}
				out = append(out, a)
				continue
			}
			if key == "--regexp" || key == "--file" || key == "--ignore-file" || contains(values, key) {
				if !eq {
					i++
					if i == len(args) {
						return nil, false
					}
					val = args[i]
				}
				if key == "--regexp" || key == "--file" {
					patternSet = true
				}
				if key == "--file" || key == "--ignore-file" {
					var ok bool
					val, ok = mapPath(val)
					if !ok {
						return nil, false
					}
				}
				out = append(out, key, val)
				continue
			}
			return nil, false
		}
		if !end && strings.HasPrefix(a, "-") && a != "-" {
			for j := 1; j < len(a); j++ {
				flag := a[j]
				if strings.ContainsRune("efgtTEmABCjMr", rune(flag)) {
					val := a[j+1:]
					if val == "" {
						i++
						if i == len(args) {
							return nil, false
						}
						val = args[i]
					}
					if flag == 'e' || flag == 'f' {
						patternSet = true
					}
					if flag == 'f' {
						var ok bool
						val, ok = mapPath(val)
						if !ok {
							return nil, false
						}
					}
					out = append(out, "-"+string(flag), val)
					break
				}
				if !strings.ContainsRune("0abcFhHiIlnNoPqsSUuvwx", rune(flag)) {
					return nil, false
				}
				out = append(out, "-"+string(flag))
			}
			continue
		}
		if !files && !patternSet {
			patternSet = true
			out = append(out, a)
			continue
		}
		mapped, ok := mapPath(a)
		if !ok {
			return nil, false
		}
		out = append(out, mapped)
		paths++
	}
	if paths == 0 {
		out = append(out, ".")
		if pathsToCheck != nil {
			*pathsToCheck = append(*pathsToCheck, ".")
		}
	} // Never let rg wait on Claude's open stdin.
	return out, true
}

func bridgeSearch(args []string) error {
	c, err := sessionConfig()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	var paths []string
	mapped, supported := prepareSearch(args, cwd, c.RemoteRoot, &paths)
	if !c.LocalSearch || !supported || (len(args) == 1 && (args[0] == "--version" || args[0] == "--help")) {
		cmd := exec.Command(serverRipgrep, args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			if x, ok := err.(*exec.ExitError); ok {
				return &exitError{x.ExitCode()}
			}
			return err
		}
		return nil
	}
	r, err := bridgeExec(c, ExecRequest{Kind: "search", Args: mapped, Cwd: cwd, Spec: map[string]any{"paths": paths}}, nil, os.Stdout, os.Stderr)
	if err != nil {
		return fmt.Errorf("workstation search: %w", err)
	}
	if r.Code != 0 {
		return &exitError{r.Code}
	}
	return nil
}
