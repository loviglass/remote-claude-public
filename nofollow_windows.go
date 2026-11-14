//go:build windows

package main

import "os"

func openNoFollow(p string) (*os.File, error) { return os.Open(p) }
