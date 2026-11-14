package main

import (
	"flag"
	"fmt"
	"os"
)

func profileConnection(args []string, kind string) (Profile, string, []string, error) {
	fs := flag.NewFlagSet(kind, flag.ContinueOnError)
	name := fs.String("profile", "default", "connection profile")
	proxy := addProxyFlags(fs)
	if e := fs.Parse(args); e != nil {
		return Profile{}, "", nil, e
	}
	proxyAddress, e := proxy.address()
	if e != nil {
		return Profile{}, "", nil, e
	}
	if !validName(*name) {
		return Profile{}, "", nil, fmt.Errorf("invalid profile")
	}
	p, e := loadProfile(*name)
	return p, proxyAddress, fs.Args(), e
}
func night(args []string) error {
	p, proxyAddress, rest, e := profileConnection(args, "night")
	if e != nil {
		return e
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: remote-claude night [--profile NAME] [--proxy [--proxy-host HOST] [--proxy-port PORT]] /absolute/VPS/repository [Claude arguments]")
	}
	c, e := sshClient(p, proxyAddress)
	if e != nil {
		return e
	}
	defer c.Close()
	if e := installAvailableUpdate(c); e != nil {
		return e
	}
	command := serverBinary + " server night"
	for _, a := range rest {
		command += " " + shellQuote(a)
	}
	fmt.Fprintln(os.Stderr, "Night mode: files and commands on VPS. Detach: Ctrl+B, then D.")
	return runRemoteTerminal(c, command, true)
}
func remoteSSH(args []string) error {
	p, proxyAddress, rest, e := profileConnection(args, "ssh")
	if e != nil {
		return e
	}
	c, e := sshClient(p, proxyAddress)
	if e != nil {
		return e
	}
	defer c.Close()
	if e := installAvailableUpdate(c); e != nil {
		return e
	}
	command := "exec /bin/bash -l"
	if len(rest) > 0 {
		command = ""
		for _, a := range rest {
			command += shellQuote(a) + " "
		}
	}
	return runRemoteTerminal(c, command, true)
}
