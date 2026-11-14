package main

import (
	"net"
	"path/filepath"
)

func profileAddress(host string) string {
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "22")
	}
	return host
}

func loadProfile(name string) (Profile, error) {
	path := filepath.Join(configDir(), name, "config.json")
	var p Profile
	if err := readJSON(path, &p); err != nil {
		return p, err
	}
	return p, nil
}
