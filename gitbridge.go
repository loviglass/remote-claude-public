package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Claude starts background Git processes directly, without SHELL_PREFIX. Keep
// these on the workstation too: status/log over SSHFS can stall the whole UI.
func gitShimDir() (string, error) {
	// Keep concurrent release/canary sessions independent.
	digest := sha256.Sum256([]byte(serverBinary))
	dir := filepath.Join(stateDir(), fmt.Sprintf("bin-%x", digest[:6]))
	if e := os.MkdirAll(dir, 0700); e != nil {
		return "", e
	}
	for _, name := range []string{"git", "rg", "shell"} {
		link := filepath.Join(dir, name)
		if target, e := os.Readlink(link); e == nil && target == serverBinary {
			continue
		}
		if e := os.Symlink(serverBinary, link); e != nil {
			// Another session can create the same immutable link concurrently.
			if target, err := os.Readlink(link); err != nil || target != serverBinary {
				return "", e
			}
		}
	}
	return dir, nil
}

func bridgeGit(args []string) error {
	c, e := sessionConfig()
	if e != nil {
		return e
	}
	cwd, e := os.Getwd()
	if e != nil {
		return e
	}
	// Claude also uses Git for its own plugin repositories on the VPS. Those
	// do not belong to the exported project and must keep using the server Git.
	effectiveCwd, target, resolvedArgs := gitLocation(cwd, args)
	inProject := target == c.RemoteRoot || strings.HasPrefix(target, c.RemoteRoot+"/")
	if !inProject {
		cmd := exec.Command("/usr/bin/git", args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if e := cmd.Run(); e != nil {
			if x, ok := e.(*exec.ExitError); ok {
				return &exitError{x.ExitCode()}
			}
			return e
		}
		return nil
	}
	localRoot := gitLocalRoot(c)
	localArgs := make([]string, len(resolvedArgs))
	for i, a := range resolvedArgs {
		localArgs[i] = strings.ReplaceAll(a, c.RemoteRoot, localRoot)
	}
	if effectiveCwd != c.RemoteRoot && !strings.HasPrefix(effectiveCwd, c.RemoteRoot+"/") {
		// An absolute --git-dir/--work-tree can select this repository even
		// when the caller starts elsewhere on the VPS.
		effectiveCwd = c.RemoteRoot
	}
	env := map[string]string{}
	// Preserve Git's defensive flags/environment without exporting VPS PATH,
	// credentials or user configuration over the bridge.
	for _, k := range []string{"GIT_OPTIONAL_LOCKS", "GIT_TERMINAL_PROMPT", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GIT_LITERAL_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_NO_LAZY_FETCH"} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	if n, _ := strconv.Atoi(env["GIT_CONFIG_COUNT"]); n > 0 && n <= 256 {
		for i := 0; i < n; i++ {
			for _, prefix := range []string{"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_"} {
				k := prefix + strconv.Itoa(i)
				env[k] = os.Getenv(k)
			}
		}
	}
	var pathOutput bytes.Buffer
	var out io.Writer = os.Stdout
	// Only path-valued rev-parse output is mapped back. Log/blob/diff bytes
	// must remain untouched, including text that happens to contain a path.
	paths := gitReturnsPaths(args)
	if paths {
		out = &pathOutput
	}
	var in io.Reader = os.Stdin
	if gitMetadataWithoutInput(resolvedArgs) {
		// Claude leaves stdin open on background Git children. Close the RPC
		// input for operations that never read it, so the workstation can reap
		// Git without waiting for its still-open stdin forwarding goroutine.
		in = nil
	}
	r, e := bridgeExec(c, ExecRequest{Kind: "exec", Command: "git", Args: localArgs, Cwd: effectiveCwd, Env: env}, in, out, os.Stderr)
	if paths {
		io.WriteString(os.Stdout, strings.ReplaceAll(pathOutput.String(), localRoot, c.RemoteRoot))
	}
	if e != nil {
		return fmt.Errorf("workstation Git: %w", e)
	}
	if r.Code != 0 {
		return &exitError{r.Code}
	}
	return nil
}

func gitMetadataWithoutInput(args []string) bool {
	for _, a := range args {
		if a == "-" || a == "--stdin" || strings.HasPrefix(a, "--batch") {
			return false
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-c" || a == "--config-env" || a == "--namespace" || a == "--super-prefix" {
			i++
			continue
		}
		if a == "--version" {
			return true
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		switch a {
		case "version", "status", "log", "ls-files", "rev-parse", "diff", "show", "branch", "remote", "config", "symbolic-ref", "show-ref", "check-ignore", "cat-file", "describe", "rev-list", "worktree":
			return true
		}
		return false
	}
	return false
}

func gitLocalRoot(c SessionConfig) string {
	if c.OS == "windows" {
		// This code runs on Linux, whose filepath.ToSlash does not recognize
		// Windows backslashes. Git for Windows reports forward-slash paths.
		return strings.ReplaceAll(c.LocalRoot, "\\", "/")
	}
	return c.LocalRoot
}

// Resolve global -C options before deciding where Git runs. Commands targeting
// a VPS plugin directory from a project cwd still belong to the VPS.
func gitLocation(cwd string, args []string) (string, string, []string) {
	var gitDir, workTree string
	resolved := make([]string, 0, len(args))
	absolute := func(p string) string {
		if path.IsAbs(p) {
			return path.Clean(p)
		}
		return path.Join(cwd, p)
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-C" && i+1 < len(args) {
			i++
			if args[i] != "" {
				cwd = absolute(args[i])
			}
			continue
		}
		if strings.HasPrefix(a, "-C") && len(a) > 2 {
			cwd = absolute(a[2:])
			continue
		}
		for _, option := range []string{"--git-dir", "--work-tree"} {
			if a == option && i+1 < len(args) {
				i++
				a = option + "=" + absolute(args[i])
			} else if strings.HasPrefix(a, option+"=") {
				a = option + "=" + absolute(strings.TrimPrefix(a, option+"="))
			}
			if strings.HasPrefix(a, option+"=") {
				if option == "--git-dir" {
					gitDir = strings.TrimPrefix(a, option+"=")
				} else {
					workTree = strings.TrimPrefix(a, option+"=")
				}
			}
		}
		resolved = append(resolved, a)
		if a == "-c" || a == "--config-env" || a == "--namespace" || a == "--super-prefix" {
			if i+1 < len(args) {
				i++
				resolved = append(resolved, args[i])
			}
		} else if !strings.HasPrefix(a, "-") || a == "--" {
			resolved = append(resolved, args[i+1:]...)
			break
		}
	}
	target := cwd
	if gitDir != "" {
		target = gitDir
	}
	if workTree != "" {
		target = workTree
	}
	return cwd, target, resolved
}

func gitReturnsPaths(args []string) bool {
	revParse := false
	for _, a := range args {
		if a == "rev-parse" {
			revParse = true
		}
		if revParse {
			switch a {
			case "--show-toplevel", "--git-dir", "--absolute-git-dir", "--git-common-dir", "--show-superproject-working-tree", "--git-path":
				return true
			}
		}
	}
	return false
}
