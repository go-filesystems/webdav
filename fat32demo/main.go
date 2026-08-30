// Command fat32demo serves a FAT32 image over WebDAV.
//
//	fat32demo -image disk.img -addr 127.0.0.1:8080
//
//	curl -s http://127.0.0.1:8080/HELLO.TXT
//	curl -s -H 'Range: bytes=0-15' http://127.0.0.1:8080/SUB/BIG.BIN
//	open http://127.0.0.1:8080/                      # a browser reads it
//
// Everything it does lives in the demo package, which is where the tests
// reach it; this file is the shell.
package main

import (
	"os"

	"github.com/go-filesystems/webdav/fat32demo/demo"
)

func main() { os.Exit(demo.Main(os.Args[1:], os.Stdout, os.Stderr)) }
