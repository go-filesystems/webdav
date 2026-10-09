module github.com/go-filesystems/webdav/fat32demo

go 1.27.1

require (
	github.com/go-filesystems/fat32 v0.5.0
	github.com/go-filesystems/interface v0.5.0
	github.com/go-filesystems/webdav v0.5.0
)

require (
	github.com/go-filesystems/hostcopy v0.1.0 // indirect
	github.com/go-volumes/gpt v0.2.0 // indirect
	github.com/go-volumes/safeio v0.0.0-20260831125406-d8f54b2890d4 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

// The parent module is this repository; it is not resolvable from the proxy
// at a version that contains the code under test, and never will be for the
// commit being built.
replace github.com/go-filesystems/webdav => ..
