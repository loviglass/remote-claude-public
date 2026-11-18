package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Server mode runs Claude, its commands and its Git work on the VPS, so the
// workstation may disconnect. Every job is a container in one resource pool
// shared by all developers, so a project brings its toolchain as an image and
// the host needs none. Its terminal lives in a tmux server kept by systemd.
const jobSocket = "remote-claude-jobs"
const maxUserJobs = 2
const jobBaseImage = "remote-claude-job-base:latest"
const jobLabel = "remote-claude.uid"
const hostGH = "/usr/bin/gh"

const jobInstructions = "This session uses remote-claude server mode: you work autonomously on the server and the developer's computer may be offline, so nobody will answer questions or approve actions until you finish. The checkout starts from the developer's exact working state: their latest commit plus any uncommitted changes, which appear as uncommitted changes and belong to the task. Work only on the current branch, which was created for this task. You may run any command the developer's server account can: build, run tests, start services, inspect the server, its logs and databases, and install tools for this account (user-level package managers, ~/.local, language toolchains). Verify your work by actually running it. When the work is ready, commit it, push this branch to origin and open a pull request with `gh pr create`, summarising what you verified. Never merge a pull request, never force-push and never push to the default branch or any other existing branch: a human reviews and merges. The server is shared with other projects and this job runs under a resource budget, so avoid needless parallel builds and tests."

// Nobody is at the keyboard, so a job that must ask for approval simply
// stops. Everything is allowed for the account's own rights; deny rules win
// over allow rules in every permission mode and block the routes to a merge
// or a rewritten history that a command prefix can name. Branch protection
// on the Git host remains the actual guarantee.
const jobSettings = `{"permissions":{"defaultMode":"acceptEdits","allow":["Bash","Edit","Write","NotebookEdit","WebFetch","WebSearch"],"deny":["Bash(gh pr merge:*)","Bash(gh api:*merge*)","Bash(git push --force:*)","Bash(git push -f:*)","Bash(git push --force-with-lease:*)","Bash(git push --mirror:*)","Bash(git push --delete:*)","Bash(git push -d:*)"]}}`

var commitID = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
var jobSession = regexp.MustCompile(`^job-[0-9a-f]{12}$`)
var plainBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

func localGit(root string, args ...string) (string, error) {
	b, e := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
	return strings.TrimSpace(string(b)), e
}

func serverMode(p Profile, root string, claudeArgs []string, resume, forceNew bool, proxyAddress string) error {
	if os.Getenv(sessionWorkerEnv) != "1" {
		return superviseInteractiveSession(true)
	}
	reattachOnly := os.Getenv(serverReattachOnlyEnv) == "1"
	resume = resume || reattachOnly
	origin, e := localGit(root, "remote", "get-url", "origin")
	if e != nil || origin == "" {
		return fmt.Errorf("server mode clones the project from its Git remote \"origin\", which %s does not have", root)
	}
	commit, e := localGit(root, "rev-parse", "HEAD")
	if e != nil {
		return e
	}
	branch, _ := localGit(root, "rev-parse", "--abbrev-ref", "HEAD")
	c, e := sshClient(p, proxyAddress)
	if e != nil {
		return markSessionRetry(e, false)
	}
	defer c.Close()
	if e := installAvailableUpdate(c); e != nil {
		return e
	}
	// A reattach must not replace the running job's starting point.
	snapshot := ""
	if !resume {
		var e error
		if snapshot, e = uploadWorkingTree(c, root, commit, origin); e != nil {
			return e
		}
	}
	// The server account has no Git identity of its own; commits carry the
	// developer's.
	name, _ := localGit(root, "config", "user.name")
	email, _ := localGit(root, "config", "user.email")
	command := serverBinary + " server job --name " + shellQuote(name) + " --email " + shellQuote(email)
	if resume {
		command += " --resume"
	}
	if reattachOnly {
		command += " --reattach-only"
	}
	// Sent only when asked for: a server older than this flag rejects it.
	if forceNew {
		command += " --force-new"
	}
	if snapshot != "" {
		command += " --snapshot " + snapshot
	}
	command += " --"
	for _, a := range append([]string{origin, commit, branch}, claudeArgs...) {
		command += " " + shellQuote(a)
	}
	fmt.Fprintln(os.Stderr, "Server mode project:", root)
	fmt.Fprintln(os.Stderr, "Files and commands on the server. Detach: Ctrl+B, then D; this computer may then disconnect.")
	return markSessionRetry(runRemoteTerminal(c, command, true), false)
}

func jobCheckout(origin string) string {
	name := strings.TrimSuffix(path.Base(strings.ReplaceAll(origin, ":", "/")), ".git")
	if !validName(name) {
		name = "project"
	}
	h := sha256.Sum256([]byte(origin))
	return fmt.Sprintf("%s-%x", name, h[:4])
}

func jobBranch(base string, now time.Time) string {
	if base == "HEAD" || !plainBranch.MatchString(base) {
		base = "task"
	}
	return "claude/" + base + "-" + now.UTC().Format("20060102-150405")
}

func jobTmux(args ...string) *exec.Cmd {
	return exec.Command("tmux", append([]string{"-L", jobSocket}, args...)...)
}

func serverJob(args []string) error {
	fs := flag.NewFlagSet("job", flag.ContinueOnError)
	name := fs.String("name", "", "commit author name")
	email := fs.String("email", "", "commit author email")
	resume := fs.Bool("resume", false, "continue the existing server checkout and Claude conversation")
	reattachOnly := fs.Bool("reattach-only", false, "attach to the running tmux job without starting it")
	forceNew := fs.Bool("force-new", false, "start a job although another account runs the same project")
	snapshot := fs.String("snapshot", "", "commit holding the workstation's working tree, uploaded with job-upload")
	fs.Bool("dirty", false, "accepted from older clients")
	if e := fs.Parse(args); e != nil {
		return e
	}
	args = fs.Args()
	if len(args) < 3 || strings.HasPrefix(args[0], "-") || !commitID.MatchString(args[1]) {
		return fmt.Errorf("usage: server job [--name NAME] [--email EMAIL] ORIGIN COMMIT BRANCH [Claude arguments]")
	}
	home, e := os.UserHomeDir()
	if e != nil {
		return e
	}
	dir := filepath.Join(home, "server-jobs", jobCheckout(args[0]))
	return startJob(dir, args[3:], func() error {
		if e := guardAgainstParallelJob(filepath.Base(dir), *forceNew); e != nil {
			return e
		}
		if *resume {
			return prepareCheckout(dir, args[0], args[1], args[2], *name, *email, "", true)
		}
		if *snapshot != "" && !commitID.MatchString(*snapshot) {
			return fmt.Errorf("invalid snapshot")
		}
		if *snapshot != "" {
			fmt.Fprintln(os.Stderr, "The job starts from the workstation's working tree, including uncommitted and unpushed changes.")
		}
		return prepareCheckout(dir, args[0], args[1], args[2], *name, *email, *snapshot, false)
	}, *reattachOnly)
}

func serverNight(args []string) error {
	if len(args) == 0 || !filepath.IsAbs(args[0]) {
		return fmt.Errorf("supply an absolute path to a separate Git checkout on the VPS")
	}
	root, e := filepath.EvalSymlinks(args[0])
	if e != nil {
		return e
	}
	if strings.Contains(root, "/.local/share/remote-claude/projects/") {
		return fmt.Errorf("night mode needs a VPS checkout; a mounted workstation project cannot run after disconnect")
	}
	if _, e = os.Stat(filepath.Join(root, ".git")); e != nil {
		return fmt.Errorf("not a Git checkout: %s", root)
	}
	return startJob(root, args[1:], func() error { return nil }, false)
}

func prepareCheckout(dir, origin, commit, branch, name, email, snapshot string, resume bool) error {
	git := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
		return cmd.Run()
	}
	if _, e := os.Stat(filepath.Join(dir, ".git")); e != nil {
		if resume {
			return fmt.Errorf("no earlier server job for this project; start one with remote-claude --server PROJECT")
		}
		if e := os.MkdirAll(filepath.Dir(dir), 0700); e != nil {
			return e
		}
		if e := git("clone", "--", origin, dir); e != nil {
			return fmt.Errorf("clone %s: %w; give this server account access to the Git host first (remote-claude ssh)", origin, e)
		}
	} else if resume {
		currentOrigin, e := localGit(dir, "remote", "get-url", "origin")
		if e != nil || currentOrigin != origin {
			return fmt.Errorf("existing server checkout has a different origin: %s", dir)
		}
		currentBranch, e := localGit(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
		if e != nil || !strings.HasPrefix(currentBranch, "claude/") {
			return fmt.Errorf("existing server checkout is not on a job branch: %s", dir)
		}
		fmt.Fprintln(os.Stderr, "Resuming server branch", currentBranch, "with its existing files")
		return nil
	} else {
		if b, e := exec.Command("git", "-C", dir, "status", "--porcelain").Output(); e != nil || len(b) > 0 {
			return fmt.Errorf("the previous job left uncommitted changes in %s; review them with remote-claude ssh", dir)
		}
		if e := git("-C", dir, "fetch", "--prune", "origin"); e != nil {
			return e
		}
	}
	if snapshot != "" {
		bundle := uploadedSnapshot(snapshot)
		defer os.Remove(bundle)
		if _, e := os.Stat(bundle); e == nil {
			if e := git("-C", dir, "fetch", "--quiet", bundle, snapshotRef+":"+snapshotRef); e != nil {
				return fmt.Errorf("unpack the working tree snapshot: %w", e)
			}
		}
	}
	if exec.Command("git", "-C", dir, "cat-file", "-e", commit+"^{commit}").Run() != nil {
		return fmt.Errorf("commit %.12s is neither on origin nor in the uploaded snapshot", commit)
	}
	for key, value := range map[string]string{"user.name": name, "user.email": email} {
		if current, _ := localGit(dir, "config", key); current == "" && value != "" {
			if e := git("-C", dir, "config", key, value); e != nil {
				return e
			}
		}
	}
	if e := git("-C", dir, "switch", "--quiet", "-c", jobBranch(branch, time.Now()), commit); e != nil {
		return e
	}
	if snapshot == "" || snapshot == commit {
		return nil
	}
	// Check out the snapshot's files, then point the index back at the base
	// commit: the developer's work in progress shows as uncommitted changes.
	if e := git("-C", dir, "read-tree", "-u", "--reset", snapshot); e != nil {
		return e
	}
	if e := git("-C", dir, "reset", "--quiet"); e != nil {
		return e
	}
	exec.Command("git", "-C", dir, "update-ref", "-d", snapshotRef).Run()
	return nil
}

const snapshotRef = "refs/remote-claude/snapshot"

func uploadedSnapshot(sha string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "server-jobs", ".incoming", sha+".bundle")
}

type uploadReply struct {
	Stored  bool `json:"stored"`
	Full    bool `json:"full,omitempty"`
	Running bool `json:"running,omitempty"`
}

// uploadWorkingTree sends the working tree snapshot for a new job and returns
// its commit, or "" when the job is already running or nothing differs.
func uploadWorkingTree(c *ssh.Client, root, head, origin string) (string, error) {
	for _, full := range []bool{false, true} {
		bundle, w, e := workingTreeBundle(root, head, full)
		if e != nil {
			return "", fmt.Errorf("snapshot the working tree: %w", e)
		}
		if w == head && bundle == nil {
			return "", nil
		}
		command := serverBinary + " server job-upload " + w + " " + shellQuote(origin)
		if full {
			command += " --full"
		}
		b, e := runSSH(c, command, bundle)
		if e != nil {
			return "", fmt.Errorf("upload the working tree snapshot: %w", e)
		}
		var r uploadReply
		if e := json.Unmarshal(b, &r); e != nil {
			return "", fmt.Errorf("upload the working tree snapshot: %w", e)
		}
		if r.Running {
			return "", nil
		}
		if r.Stored {
			return w, nil
		}
	}
	return "", fmt.Errorf("the server could not use the working tree snapshot")
}

// serverJobUpload stores a snapshot bundle for the next job of ORIGIN. It
// checks the bundle against a fetched checkout: a bundle built against stale
// local remote-tracking refs is refused so the client sends a full one.
func serverJobUpload(args []string) error {
	full := len(args) == 3 && args[2] == "--full"
	if len(args) < 2 || len(args) > 3 || !commitID.MatchString(args[0]) || len(args) == 3 && !full {
		return fmt.Errorf("usage: server job-upload COMMIT ORIGIN [--full] < bundle")
	}
	reply := func(r uploadReply) error { return json.NewEncoder(os.Stdout).Encode(r) }
	home, e := os.UserHomeDir()
	if e != nil {
		return e
	}
	dir := filepath.Join(home, "server-jobs", jobCheckout(args[1]))
	h := sha256.Sum256([]byte(dir))
	if jobTmux("has-session", "-t", fmt.Sprintf("=job-%x", h[:6])).Run() == nil {
		io.Copy(io.Discard, os.Stdin)
		return reply(uploadReply{Running: true})
	}
	p := uploadedSnapshot(args[0])
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return e
	}
	// Snapshots of jobs that never started are not kept forever.
	if old, _ := filepath.Glob(filepath.Join(filepath.Dir(p), "*.bundle")); len(old) > 0 {
		for _, f := range old {
			if st, e := os.Stat(f); e == nil && time.Since(st.ModTime()) > 24*time.Hour {
				os.Remove(f)
			}
		}
	}
	f, e := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = io.Copy(f, io.LimitReader(os.Stdin, 2<<30)); e == nil {
		e = f.Sync()
	}
	if ce := f.Close(); e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if !full && !bundleUsable(dir, args[1], f.Name()) {
		return reply(uploadReply{Full: true})
	}
	if e := os.Rename(f.Name(), p); e != nil {
		return e
	}
	return reply(uploadReply{Stored: true})
}

// bundleUsable fetches origin into the job checkout without prompting and
// verifies that the bundle's prerequisites are there.
func bundleUsable(dir, origin, bundle string) bool {
	quiet := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
		return cmd.Run()
	}
	if _, e := os.Stat(filepath.Join(dir, ".git")); e != nil {
		os.MkdirAll(filepath.Dir(dir), 0700)
		if quiet("clone", "--quiet", "--", origin, dir) != nil {
			// The job clones interactively and reports a failure there.
			return true
		}
	} else {
		quiet("-C", dir, "fetch", "--quiet", "--prune", "origin")
	}
	return quiet("-C", dir, "bundle", "verify", "--quiet", bundle) == nil
}

// workingTreeBundle records the working tree (tracked and untracked files
// that Git does not ignore) as a commit on top of HEAD, using a copy of the
// index so the developer's index, worktree and branches stay untouched. The
// returned bundle carries that commit and any commits origin lacks; it is
// nil when origin already has everything.
func workingTreeBundle(root, head string, full bool) ([]byte, string, error) {
	indexPath, e := localGit(root, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if e != nil {
		return nil, "", e
	}
	tmp, e := os.CreateTemp("", "remote-claude-index-*")
	if e != nil {
		return nil, "", e
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if b, e := os.ReadFile(indexPath); e == nil {
		// A copy keeps Git's stat cache, so unchanged files are not rehashed.
		if e := os.WriteFile(tmp.Name(), b, 0600); e != nil {
			return nil, "", e
		}
	} else {
		os.Remove(tmp.Name())
	}
	withIndex := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+tmp.Name())
		b, e := cmd.Output()
		return strings.TrimSpace(string(b)), e
	}
	if _, e := withIndex("add", "--all"); e != nil {
		return nil, "", e
	}
	tree, e := withIndex("write-tree")
	if e != nil {
		return nil, "", e
	}
	headTree, _ := localGit(root, "rev-parse", head+"^{tree}")
	snapshot := head
	if tree != headTree {
		cmd := exec.Command("git", "-C", root, "commit-tree", tree, "-p", head, "-m", "remote-claude: working tree snapshot")
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=remote-claude", "GIT_AUTHOR_EMAIL=remote-claude@localhost", "GIT_COMMITTER_NAME=remote-claude", "GIT_COMMITTER_EMAIL=remote-claude@localhost")
		b, e := cmd.Output()
		if e != nil {
			return nil, "", e
		}
		snapshot = strings.TrimSpace(string(b))
	}
	// A bundle names its tips by ref; a private ref exists only meanwhile.
	if _, e := localGit(root, "update-ref", snapshotRef, snapshot); e != nil {
		return nil, "", e
	}
	defer localGit(root, "update-ref", "-d", snapshotRef)
	exclude := []string{"--not", "--remotes=origin"}
	if full {
		exclude = nil
	} else if missing, _ := localGit(root, "rev-list", "--count", snapshotRef, "--not", "--remotes=origin"); missing == "0" {
		return nil, snapshot, nil
	}
	out, e := os.CreateTemp("", "remote-claude-bundle-*")
	if e != nil {
		return nil, "", e
	}
	out.Close()
	defer os.Remove(out.Name())
	cmd := exec.Command("git", append([]string{"-C", root, "bundle", "create", "--quiet", out.Name(), snapshotRef}, exclude...)...)
	if b, e := cmd.CombinedOutput(); e != nil {
		return nil, "", fmt.Errorf("git bundle: %v: %s", e, b)
	}
	b, e := os.ReadFile(out.Name())
	return b, snapshot, e
}

// startJob attaches to the job of this checkout, creating it when absent.
func startJob(dir string, claudeArgs []string, prepare func() error, reattachOnly bool) error {
	// tmux starts a missing server on demand, and one started from an SSH login
	// would run outside the pool.
	unit := fmt.Sprintf("remote-claude-jobs@%d.service", os.Getuid())
	if exec.Command("systemctl", "is-active", "--quiet", unit).Run() != nil {
		return fmt.Errorf("server mode is not enabled for this account; the administrator enables it with configure-resources.py")
	}
	h := sha256.Sum256([]byte(dir))
	session := fmt.Sprintf("job-%x", h[:6])
	if jobTmux("has-session", "-t", "="+session).Run() != nil {
		if reattachOnly {
			return fmt.Errorf("server job is no longer running; use remote-claude --server --resume to restart its Claude session on the same branch")
		}
		running, _ := jobTmux("list-sessions", "-F", "#{session_name}").Output()
		if n := len(strings.Fields(string(running))); n >= maxUserJobs {
			return fmt.Errorf("%d server jobs of this account are already running; finish one first", n)
		}
		if e := prepare(); e != nil {
			return e
		}
		// By default a job runs directly under the developer's account, like
		// an SSH login: it sees the server, its services and databases and
		// the account's tools. A project that wants an isolated toolchain
		// provides .remote-claude/Dockerfile.
		run := shellQuote(serverBinary) + " server job-run " + shellQuote(dir)
		if _, e := os.Stat(filepath.Join(dir, ".remote-claude", "Dockerfile")); e == nil {
			if exec.Command("sudo", "-n", "-l", serverBinary, "server", "job-container", "check").Run() != nil {
				return fmt.Errorf("this project asks for a job container (.remote-claude/Dockerfile), which the administrator has not enabled for this account; see ADMIN.ru.md")
			}
			if e := buildJobImage(dir, session); e != nil {
				return fmt.Errorf("build .remote-claude/Dockerfile: %w", e)
			}
			run = "sudo -n " + shellQuote(serverBinary) + " server job-container project " + session + " " + shellQuote(dir)
		}
		for _, a := range claudeArgs {
			run += " " + shellQuote(a)
		}
		// The pause keeps a startup error readable instead of closing the
		// session with it.
		run += ` || read -r -p "Server job ended with an error; press Enter to close. " _`
		if b, e := jobTmux("new-session", "-d", "-s", session, "-c", dir, "exec /bin/bash -lc "+shellQuote(run)).CombinedOutput(); e != nil {
			return fmt.Errorf("start server job: %v: %s", e, strings.TrimSpace(string(b)))
		}
	} else {
		fmt.Fprintln(os.Stderr, "Reattaching to the running server job")
	}
	attach := jobTmux("attach-session", "-t", "="+session)
	attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, os.Stdout, os.Stderr
	return attach.Run()
}

// The image context is only .remote-claude, archived with the developer's own
// rights; root never reads a path the developer chose.
func buildJobImage(dir, session string) error {
	archive := exec.Command("tar", "-C", filepath.Join(dir, ".remote-claude"), "-c", ".")
	build := exec.Command("sudo", "-n", serverBinary, "server", "job-image", session)
	pipe, e := archive.StdoutPipe()
	if e != nil {
		return e
	}
	build.Stdin, build.Stdout, build.Stderr = pipe, os.Stderr, os.Stderr
	archive.Stderr = os.Stderr
	if e = archive.Start(); e != nil {
		return e
	}
	e = build.Run()
	if x := archive.Wait(); e == nil {
		e = x
	}
	return e
}

// jobAccount names the developer behind sudo. The two commands below run as
// root: the developer chooses the image contents and Claude's arguments, and
// nothing else that reaches Docker.
func jobAccount(session string) (*user.User, error) {
	if !jobSession.MatchString(session) {
		return nil, fmt.Errorf("invalid job name")
	}
	return jobCaller()
}

// jobCaller is the managed account behind sudo, with no job named.
func jobCaller() (*user.User, error) {
	id := os.Getenv("SUDO_UID")
	if os.Getuid() != 0 || id == "" || id == "0" {
		return nil, fmt.Errorf("this command is started by remote-claude through sudo")
	}
	u, e := user.LookupId(id)
	if e != nil {
		return nil, e
	}
	if _, e = os.Lstat(filepath.Join(u.HomeDir, ".remote-claude-managed")); e != nil {
		return nil, fmt.Errorf("not a managed account: %s", u.Username)
	}
	return u, nil
}

func jobImage(u *user.User, session string) string {
	return "remote-claude-job-u" + u.Uid + ":" + session
}

func serverJobImage(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: server job-image JOB < context.tar")
	}
	u, e := jobAccount(args[0])
	if e != nil {
		return e
	}
	// BuildKit runs build steps outside the cgroup named here, which would
	// let a heavy build escape the pool.
	cmd := exec.Command("docker", "build", "--cgroup-parent", "remoteclaude-u"+u.Uid+".slice", "--tag", jobImage(u, args[0]), "-")
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=0")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func jobContainer(u *user.User, kind, session, dir string, claudeArgs []string) ([]string, error) {
	if kind != "base" && kind != "project" {
		return nil, fmt.Errorf("unknown image kind")
	}
	if filepath.Clean(dir) != dir || !strings.HasPrefix(dir, u.HomeDir+"/") {
		return nil, fmt.Errorf("the job checkout must be inside %s", u.HomeDir)
	}
	image := jobBaseImage
	if kind == "project" {
		image = jobImage(u, session)
	}
	// The container has the developer's identity and home and no way to gain
	// more: whatever the image holds runs with the rights of an SSH login.
	// Host networking keeps the account's own network rules in force.
	run := []string{"run", "--rm", "--interactive", "--tty", "--init",
		"--name", "remote-claude-job-u" + u.Uid + "-" + session, "--label", jobLabel + "=" + u.Uid,
		"--cgroup-parent", "remoteclaude-u" + u.Uid + ".slice", "--pids-limit", "1024",
		"--user", u.Uid + ":" + u.Gid, "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--network", "host",
		"--volume", u.HomeDir + ":" + u.HomeDir,
		"--volume", "/etc/passwd:/etc/passwd:ro", "--volume", "/etc/group:/etc/group:ro",
		"--volume", serverBinary + ":" + serverBinary + ":ro", "--volume", claudeBinary + ":" + claudeBinary + ":ro",
		"--env", "HOME=" + u.HomeDir, "--env", "USER=" + u.Username, "--env", "TERM=" + os.Getenv("TERM"), "--env", "LANG=C.UTF-8",
		"--workdir", dir, "--entrypoint", serverBinary}
	if _, e := os.Stat(hostGH); e == nil {
		run = append(run, "--volume", hostGH+":"+hostGH+":ro")
	}
	return append(append(run, image, "server", "job-run", dir), claudeArgs...), nil
}

func serverJobContainer(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage: server job-container base|project JOB CHECKOUT [Claude arguments]")
	}
	u, e := jobAccount(args[1])
	if e != nil {
		return e
	}
	run, e := jobContainer(u, args[0], args[1], args[2], args[3:])
	if e != nil {
		return e
	}
	// A container that outlived its terminal would keep the job name taken.
	exec.Command("docker", "rm", "--force", "remote-claude-job-u"+u.Uid+"-"+args[1]).Run()
	cmd := exec.Command("docker", run...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func serverJobRun(args []string) error {
	if len(args) == 0 || !filepath.IsAbs(args[0]) {
		return fmt.Errorf("job-run requires the VPS working directory")
	}
	if _, e := exec.LookPath("git"); e != nil {
		return fmt.Errorf("the job image has no git; base the project image on %s or install git in it", jobBaseImage)
	}
	claude := append([]string{"--append-system-prompt", jobInstructions, "--settings", jobSettings}, args[1:]...)
	cmd := exec.Command(claudeBinary, claude...)
	cmd.Dir = args[0]
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1", "CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS=8", "CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH=1")
	return cmd.Run()
}
