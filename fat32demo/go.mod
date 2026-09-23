module github.com/go-filesystems/webdav/fat32demo

go 1.26.4

require (
	github.com/go-filesystems/fat32 v0.4.0
	github.com/go-filesystems/interface v0.3.0
	github.com/go-filesystems/webdav v0.1.0
)

require (
	github.com/go-volumes/gpt v0.2.0 // indirect
	github.com/go-volumes/safeio v0.0.0-20260831125406-d8f54b2890d4 // indirect
)

// The parent module is this repository; it is not resolvable from the proxy
// at a version that contains the code under test, and never will be for the
// commit being built.
replace github.com/go-filesystems/webdav => ..
