package main

import (
	"fmt"
	"regexp"
)

// resumeFlag is --resume with an optional value. Without a value it asks
// Claude Code for its conversation picker; --resume=ID resumes that
// conversation. IsBoolFlag lets the flag package accept the bare form, at the
// price that the id must be attached with "=" (a separate word is the project).
type resumeFlag struct {
	picker bool
	id     string
}

var sessionID = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]{0,127}$`)

func (r *resumeFlag) IsBoolFlag() bool { return true }
func (r *resumeFlag) String() string {
	if r == nil {
		return ""
	}
	if r.id != "" {
		return r.id
	}
	return fmt.Sprint(r.picker)
}
func (r *resumeFlag) Set(v string) error {
	switch v {
	case "true":
		r.picker, r.id = true, ""
	case "false":
		r.picker, r.id = false, ""
	default:
		r.picker, r.id = false, v
	}
	return nil
}
func (r resumeFlag) set() bool { return r.picker || r.id != "" }

// sessionArgs turns the user's choice into Claude Code arguments. afterDrop is
// true when the supervisor restarts a worker after a lost link: the user
// already chose, so the most recent conversation of the directory is the one
// they were in and the picker is not shown again.
func sessionArgs(r resumeFlag, continueLatest, afterDrop bool) ([]string, error) {
	if r.set() && continueLatest {
		return nil, fmt.Errorf("--resume and --continue exclude each other: --resume lets you choose, --continue takes the latest conversation")
	}
	if r.id != "" && !sessionID.MatchString(r.id) {
		return nil, fmt.Errorf("invalid session id for --resume")
	}
	switch {
	case r.id != "":
		return []string{"--resume", r.id}, nil
	case r.picker && !afterDrop:
		return []string{"--resume"}, nil
	case r.picker || continueLatest || afterDrop:
		return []string{"--continue"}, nil
	}
	return nil, nil
}
