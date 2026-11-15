module github.com/go-filesystems/webdav

// ⛔ THE PATCH, AND THIS ONE IS THE SHARPEST CASE IN THE FLEET: a WebDAV server
// is an HTTP/1 server, and GO-2026-6859 is an HTTP/1 server crash — published
// 2026-11-11, CVE-2026-97054, "HTTP/1 server crash due to MaxBytesReader header
// race in net/http", stdlib, fixed in 1.26.11 and 1.27.4. Read at
// https://vuln.go.dev/ID/GO-2026-6859.json rather than quoted from notes.
//
// This line is the only one of the three Go pins a CONSUMER honours: `go
// install` with GOTOOLCHAIN=local obeys it, while the CI pin merely builds our
// artefacts and the toolchain stamped in a binary describes one executable.
//
// Measured, not assumed: `modfloor --floor 1.27.4` read the directive this
// module PUBLISHED at v0.7.0 as `go 1.27.1` — three patch releases behind the
// fix — so every consumer installing it under GOTOOLCHAIN=local built a WebDAV
// server against an unpatched net/http. Eight of the nine repositories in
// go-fleettools/fleettools/.github/vulnscan-exposed.txt read BELOW at that
// moment; only go-webengine/engine was already at 1.27.4.
//
// ⚠ Whether this server's handlers actually reach the racing path is a
// REACHABILITY question, which govulncheck answers and this line does not. The
// floor is raised because a consumer is entitled to build against the patched
// library, not on a claim that the crash was reachable here.
//
// ⛔ Renovate will never offer this edit, measured on 2026-11-15: its
// github-actions manager maintains the CI pin and its gomod manager SEES this
// directive while proposing nothing for it.
go 1.27.4

require (
	github.com/go-filesystems/hostcopy v0.1.0
	github.com/go-filesystems/interface v0.6.0
)

require golang.org/x/sys v0.48.0 // indirect
