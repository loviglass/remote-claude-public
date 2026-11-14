package main

import (
	"flag"
	"reflect"
	"testing"
)

func parseResume(t *testing.T, args ...string) (resumeFlag, bool, []string) {
	t.Helper()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	var r resumeFlag
	fs.Var(&r, "resume", "")
	c := fs.Bool("continue", false, "")
	if e := fs.Parse(args); e != nil {
		t.Fatal(e)
	}
	return r, *c, fs.Args()
}

func TestResumeWithoutValueAsksClaudeToShowThePicker(t *testing.T) {
	r, c, rest := parseResume(t, "--resume", "proj")
	got, e := sessionArgs(r, c, false)
	if e != nil || !reflect.DeepEqual(got, []string{"--resume"}) {
		t.Fatalf("got %v, %v; want the picker (--resume), not --continue", got, e)
	}
	if !reflect.DeepEqual(rest, []string{"proj"}) {
		t.Fatalf("the project was swallowed: %v", rest)
	}
}

func TestResumeWithIDResumesExactlyThatConversation(t *testing.T) {
	r, c, _ := parseResume(t, "--resume=3f2a9c1e-0000-4000-8000-123456789abc", "proj")
	got, e := sessionArgs(r, c, false)
	if e != nil || !reflect.DeepEqual(got, []string{"--resume", "3f2a9c1e-0000-4000-8000-123456789abc"}) {
		t.Fatalf("got %v, %v", got, e)
	}
}

func TestExplicitContinueKeepsTheMostRecentBehaviour(t *testing.T) {
	r, c, _ := parseResume(t, "--continue", "proj")
	got, e := sessionArgs(r, c, false)
	if e != nil || !reflect.DeepEqual(got, []string{"--continue"}) {
		t.Fatalf("got %v, %v", got, e)
	}
}

func TestResumeAndContinueTogetherAreRefused(t *testing.T) {
	r, c, _ := parseResume(t, "--resume", "--continue", "proj")
	if _, e := sessionArgs(r, c, false); e == nil {
		t.Fatal("two conflicting choices were accepted")
	}
}

func TestSessionIDIsValidated(t *testing.T) {
	r, c, _ := parseResume(t, "--resume=-x;rm", "proj")
	if _, e := sessionArgs(r, c, false); e == nil {
		t.Fatal("a flag-like id was passed on to Claude")
	}
}

func TestReconnectNeverShowsThePickerAgain(t *testing.T) {
	r, c, _ := parseResume(t, "--resume", "proj")
	got, _ := sessionArgs(r, c, true)
	if !reflect.DeepEqual(got, []string{"--continue"}) {
		t.Fatalf("after a dropped link got %v", got)
	}
	r, c, _ = parseResume(t, "proj")
	got, _ = sessionArgs(r, c, true)
	if !reflect.DeepEqual(got, []string{"--continue"}) {
		t.Fatalf("plain session after a drop got %v", got)
	}
	r, c, _ = parseResume(t, "--resume=abc", "proj")
	got, _ = sessionArgs(r, c, true)
	if !reflect.DeepEqual(got, []string{"--resume", "abc"}) {
		t.Fatalf("explicit id after a drop got %v", got)
	}
}
