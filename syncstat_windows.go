package main

import "io/fs"

func fileIno(fs.FileInfo) uint64 { return 0 }
