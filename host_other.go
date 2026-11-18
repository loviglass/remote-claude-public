//go:build !linux

package main

import "fmt"

var errServerOnly = fmt.Errorf("session hosts run on the Linux server")

func serverHost(string) error               { return errServerOnly }
func serverAttachHost(string, string) error { return errServerOnly }
func serverRunHost(string, string) error    { return errServerOnly }
func ensureHost(string) error               { return errServerOnly }
func hostCall(string, hostRequest) (hostReply, error) {
	return hostReply{}, errServerOnly
}
