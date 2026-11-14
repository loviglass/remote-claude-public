package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
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

const jobInstructions = "This session uses remote-claude server mode: Claude, the checkout and every command run on the server, and the developer's computer may be offline. Work only on the current branch, which was created for this task. When the work is ready, commit it, push this branch to origin and open a pull request with `gh pr create`. Never merge a pull request and never push to the default branch or to any other existing branch: a human reviews and merges. The job runs in a container built from the project's .remote-claude/Dockerfile or a default image; tools missing from the image cannot be installed system-wide, so say so instead of working around it. The server is shared with other projects and this job runs under a resource budget, so avoid needless parallel builds and tests."

// The job must reach a pull request with nobody at the keyboard, so the Git
// steps of that path are approved in advance. Permission rules cannot stop
// every route to a merge; branch protection on the Git host is the actual
// guarantee.
const jobSettings = `{"permissions":{"allow":["Bash(git status:*)","Bash(git diff:*)","Bash(git log:*)","Bash(git add:*)","Bash(git commit:*)","Bash(git push:*)","Bash(gh pr create:*)","Bash(gh pr view:*)"],"deny":["Bash(gh pr merge:*)","Bash(git push --force:*)","Bash(git push -f:*)"]}}`

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
	dirty, _ := localGit(root, "status", "--porcelain")
	c, e := sshClient(p, proxyAddress)
	if e != nil {
		return markSessionRetry(e, false)
	}
	defer c.Close()
	if e := installAvailableUpdate(c); e != nil {
		return e
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
	if dirty != "" {
		command += " --dirty"
	}
	command += " --"
	for _, a := range append([]string{origin, commit, branch}, claudeArgs...) {
		command += " " + shellQuote(a)
	}
	fmt.Fprintln(os.Stderr, "Server mode project:", root)
	fmt.Fprintln(os.Stderr, "Files and commands on VPS. Detach: Ctrl+B, then D; this computer may then disconnect.")
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
	dirty := fs.Bool("dirty", false, "the workstation has uncommitted changes")
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
			return prepareCheckout(dir, args[0], args[1], args[2], *name, *email, true)
		}
		if *dirty {
			fmt.Fprintln(os.Stderr, "Local uncommitted changes stay on this computer; the server starts from the pushed commit.")
		}
		return prepareCheckout(dir, args[0], args[1], args[2], *name, *email, false)
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

func prepareCheckout(dir, origin, commit, branch, name, email string, resume bool) error {
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
	if exec.Command("git", "-C", dir, "cat-file", "-e", commit+"^{commit}").Run() != nil {
		return fmt.Errorf("commit %.12s is not on origin; push it first", commit)
	}
	for key, value := range map[string]string{"user.name": name, "user.email": email} {
		if current, _ := localGit(dir, "config", key); current == "" && value != "" {
			if e := git("-C", dir, "config", key, value); e != nil {
				return e
			}
		}
	}
	return git("-C", dir, "switch", "--quiet", "-c", jobBranch(branch, time.Now()), commit)
}

// startJob attaches to the job of this checkout, creating it when absent.
func startJob(dir string, claudeArgs []string, prepare func() error, reattachOnly bool) error {
	// tmux starts a missing server on demand, and one started from an SSH login
	// would run outside the pool.
	unit := fmt.Sprintf("remote-claude-jobs@%d.service", os.Getuid())
	if exec.Command("systemctl", "is-active", "--quiet", unit).Run() != nil {
		return fmt.Errorf("server mode is not enabled for this account; the administrator enables it with configure-resources.py")
	}
	if exec.Command("sudo", "-n", "-l", serverBinary, "server", "job-container", "check").Run() != nil {
		return fmt.Errorf("job containers are not enabled for this account; the administrator enables them as described in ADMIN.ru.md")
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
		image := "base"
		if _, e := os.Stat(filepath.Join(dir, ".remote-claude", "Dockerfile")); e == nil {
			if e := buildJobImage(dir, session); e != nil {
				return fmt.Errorf("build .remote-claude/Dockerfile: %w", e)
			}
			image = "project"
		}
		run := "sudo -n " + shellQuote(serverBinary) + " server job-container " + image + " " + session + " " + shellQuote(dir)
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
