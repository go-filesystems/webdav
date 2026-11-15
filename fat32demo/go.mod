module github.com/go-filesystems/webdav/fat32demo

// Raised with the root module on 2026-11-15 for GO-2026-6859 (net/http, fixed
// in 1.27.4); the reasoning is in ../go.mod.
//
// ⛔ A NESTED MODULE KEEPS ITS OWN DIRECTIVE, so a raise that stops at the root
// leaves this one behind and the sweep keeps reporting the repository.
go 1.27.4

require (
	github.com/go-filesystems/fat32 v0.13.0
	github.com/go-filesystems/interface v0.6.0
	github.com/go-filesystems/webdav v0.7.0
)

require (
	github.com/go-filesystems/hostcopy v0.1.0 // indirect
	github.com/go-volumes/gpt v0.3.0 // indirect
	github.com/go-volumes/safeio v0.0.0-20261011024942-5635ea21db3b // indirect
	golang.org/x/sys v0.48.0 // indirect
)

// The parent module is this repository; it is not resolvable from the proxy
// at a version that contains the code under test, and never will be for the
// commit being built.
replace github.com/go-filesystems/webdav => ..
