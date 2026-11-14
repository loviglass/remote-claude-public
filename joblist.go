package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// jobInfo is one running job container, as reported by `server job-list`.
type jobInfo struct {
	Account  string    `json:"account"`
	UID      string    `json:"uid"`
	Checkout string    `json:"checkout"`
	Started  time.Time `json:"started"`
	Branch   string    `json:"branch,omitempty"`
}

var safeWord = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]{0,127}$`)
var headRef = regexp.MustCompile(`^ref: refs/heads/([0-9A-Za-z][0-9A-Za-z._/-]{0,199})\n?$`)

// readBranch reads a job checkout's branch as root. The checkout belongs to a
// developer, so the path may contain symlinks: the file is opened without
// following a final link, only 256 bytes are read and only a "ref:" line of a
// plain branch name is ever returned, so nothing else can leak through it.
func readBranch(dir string) string {
	f, e := openNoFollow(filepath.Join(dir, ".git", "HEAD"))
	if e != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 256))
	if m := headRef.FindStringSubmatch(string(b)); m != nil {
		return m[1]
	}
	return ""
}

type inspected struct {
	State  struct{ StartedAt time.Time }
	Config struct {
		Labels     map[string]string
		WorkingDir string
	}
}

// jobsFromInspect keeps only containers whose label names a managed account and
// whose working directory is a checkout under that account's server-jobs.
// Everything printed comes from root-owned Docker metadata or passes a strict
// pattern.
func jobsFromInspect(raw []byte, lookup func(uid string) (*user.User, error), branch func(dir string) string) ([]jobInfo, error) {
	var all []inspected
	if e := json.Unmarshal(raw, &all); e != nil {
		return nil, e
	}
	var out []jobInfo
	for _, c := range all {
		uid := c.Config.Labels[jobLabel]
		u, e := lookup(uid)
		if uid == "" || e != nil {
			continue
		}
		prefix := filepath.Join(u.HomeDir, "server-jobs") + "/"
		dir := filepath.Clean(c.Config.WorkingDir)
		if !strings.HasPrefix(dir, prefix) || strings.Contains(dir[len(prefix):], "/") || !safeWord.MatchString(filepath.Base(dir)) || !safeWord.MatchString(u.Username) {
			continue
		}
		out = append(out, jobInfo{Account: u.Username, UID: uid, Checkout: filepath.Base(dir), Started: c.State.StartedAt.UTC(), Branch: branch(dir)})
	}
	return out, nil
}

// serverJobList runs as root through sudo and is read-only. Every managed
// account may call it, since the point is that one account learns of the
// others' jobs on the same project.
func serverJobList(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: server job-list")
	}
	if _, e := jobCaller(); e != nil {
		return e
	}
	ids, e := exec.Command("docker", "ps", "--quiet", "--no-trunc", "--filter", "label="+jobLabel).Output()
	if e != nil {
		return e
	}
	fields := strings.Fields(string(ids))
	for _, id := range fields {
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(id) {
			return fmt.Errorf("unexpected container id")
		}
	}
	list := []jobInfo{}
	if len(fields) > 0 {
		raw, e := exec.Command("docker", append([]string{"inspect"}, fields...)...).Output()
		if e != nil {
			return e
		}
		if list, e = jobsFromInspect(raw, user.LookupId, readBranch); e != nil {
			return e
		}
	}
	return json.NewEncoder(os.Stdout).Encode(list)
}

// otherAccountJobs returns running jobs of the same checkout name held by
// accounts other than the caller.
func otherAccountJobs(list []jobInfo, checkout, ownUID string) []jobInfo {
	var out []jobInfo
	for _, j := range list {
		if j.Checkout == checkout && j.UID != ownUID {
			out = append(out, j)
		}
	}
	return out
}

func conflictMessage(jobs []jobInfo) string {
	var b strings.Builder
	b.WriteString("this project already has a server job running under another account:\n")
	for _, j := range jobs {
		branch := j.Branch
		if branch == "" {
			branch = "unknown branch"
		}
		fmt.Fprintf(&b, "  account %s, branch %s, running since %s\n", j.Account, branch, j.Started.Format("2006-01-02 15:04 MST"))
	}
	b.WriteString("Two jobs on one project duplicate each other's work. Continue it from the device enrolled as that account (remote-claude --profile <its profile> --server --resume PROJECT, if this computer has that profile), or start a parallel job knowingly with --force-new.")
	return b.String()
}

// guardAgainstParallelJob refuses a new job while another account runs the same
// project. A server without the sudo rule for job-list keeps working as before.
func guardAgainstParallelJob(checkout string, force bool) error {
	if force {
		return nil
	}
	out, e := exec.Command("sudo", "-n", serverBinary, "server", "job-list").Output()
	if e != nil {
		fmt.Fprintln(os.Stderr, "warning: cannot check for the same project running under another account (the administrator adds job-list to sudoers, see ADMIN.ru.md)")
		return nil
	}
	var list []jobInfo
	if json.Unmarshal(out, &list) != nil {
		fmt.Fprintln(os.Stderr, "warning: unreadable job-list answer; not checking for parallel jobs")
		return nil
	}
	if jobs := otherAccountJobs(list, checkout, fmt.Sprint(os.Getuid())); len(jobs) > 0 {
		return fmt.Errorf("%s", conflictMessage(jobs))
	}
	return nil
}
