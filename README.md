<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems.png" alt="go-filesystems/webdav" width="720"></p>

# webdav

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/webdav.svg)](https://pkg.go.dev/github.com/go-filesystems/webdav)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-filesystems/webdav/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/webdav/actions/workflows/ci.yml)

Pure-Go (CGO=0) **WebDAV server** (RFC 4918) that exports any
[`go-filesystems/interface`](https://github.com/go-filesystems/interface)
`Filesystem` as an ordinary `net/http.Handler` — so every driver in the family
becomes something a **browser** can read, `curl` can drive, and the **Finder**
or **Explorer** can mount, with authentication and TLS.

No kernel extension. No FSKit entitlement. No cgo. No root.

## Why WebDAV alongside NFS

[`go-filesystems/nfs`](https://github.com/go-filesystems/nfs) already makes a
`Filesystem` mountable. This module exists because HTTP gives three things
NFSv3 structurally cannot, and each of them decided part of the design.

**1. Authentication and confidentiality.** NFSv3's `AUTH_UNIX` is a claim, not
a proof: the client says "uid 501" and the wire cannot disagree. There is no
encryption at all. That is acceptable on `127.0.0.1` and unusable anywhere
else. WebDAV is HTTP, so it gets Basic and Bearer authentication here and TLS
from the `http.Server` underneath.

**2. A write model that matches the contract.** NFS `WRITE` names an offset,
and `Filesystem` has no positional write, so the NFS server must read the file,
splice, and write it back — O(filesize) per request. `PUT` sends the whole body
and is therefore exactly `WriteFile`. Measured on the same 64 MiB FAT32 image,
same machine (16 MiB payload, fresh image per run):

| | throughput |
|---|---|
| `fat32.WriteFile`, no HTTP at all — the driver's own ceiling | **6.8 MB/s** |
| `PUT` over this server | **6.4 MB/s** (~95% of the ceiling) |
| `WRITE` over `go-filesystems/nfs` | 90 kB/s |

The transport is not the bottleneck; the driver is. That is the whole point.

**3. A client every machine already has.** `GET` on a file returns its bytes,
so a browser, `curl`, `wget` and every HTTP library are clients without
mounting anything.

The cost is equally honest: WebDAV is not a POSIX filesystem. No file
descriptors, no byte-range locks, no partial writes, and a mounted WebDAV
volume rewrites a whole file on every save.

## Install

```sh
go get github.com/go-filesystems/webdav
```

## Usage

```go
fsys, err := fat32.Open("disk.img", -1)
if err != nil {
    log.Fatal(err)
}
defer fsys.Close()

h, err := webdav.New(fsys)          // read-only by default
if err != nil {
    log.Fatal(err)
}
log.Fatal(http.ListenAndServe("127.0.0.1:8080", h))
```

Then, with no mount at all:

```sh
curl -s http://127.0.0.1:8080/HELLO.TXT
curl -s -H 'Range: bytes=0-15' http://127.0.0.1:8080/SUB/BIG.BIN
open http://127.0.0.1:8080/                       # a browser reads it
```

### Mounting

```sh
# macOS — Finder: Go ▸ Connect to Server (⌘K), then http://127.0.0.1:8080/
# macOS — command line (needs root, which is mount_webdav's requirement, not this server's):
sudo mount_webdav -S -v img http://127.0.0.1:8080/ /Volumes/img

# Linux
sudo mount -t davfs http://127.0.0.1:8080/ /mnt/img

# Windows
net use X: http://127.0.0.1:8080/
```

## Methods

`OPTIONS`, `PROPFIND` (Depth 0 and 1, `allprop` and `propname`), `GET` and
`HEAD` **with `Range`**, `PUT`, `MKCOL`, `DELETE`, `MOVE`, `COPY`, `PROPPATCH`,
`LOCK` and `UNLOCK`.

Read-only exports advertise `DAV: 1` and an `Allow` without the write verbs;
`ReadWrite()` exports advertise `DAV: 1, 2` — class 2 being the lock support
the macOS client insists on before it will write.

### Partial writes

A `PUT` carrying a `Content-Range` replaces a byte interval in place, through
the optional
[`filesystem.WritableFile`](https://pkg.go.dev/github.com/go-filesystems/interface#WritableFile)
capability that `interface` v0.3.0 added and `fat32` v0.3.0 implements:

```sh
printf 'ZZZZZZ' | curl -X PUT -H 'Content-Range: bytes 4-9/131072' \
    --data-binary @- http://127.0.0.1:8080/SUB/BIG.BIN
```

Six bytes changed in a 128 KiB file cost one `WriteAt`. This is precisely the
operation that reduces the NFS server to 90 kB/s — without a positional write,
patching sixteen bytes of a 4 GiB file means reading, splicing and writing
back 8 GiB — so a driver that lacks the capability is answered **501 Not
Implemented** rather than served the slow way. Quietly falling back would hand
a client that asked for the cheap operation the most expensive one the module
has, and hide it behind a `204`. A partial `PUT` never extends a resource: a
range past the end is **416**, like an unsatisfiable `GET`.

`Range` is served through the optional
[`filesystem.Opener`](https://pkg.go.dev/github.com/go-filesystems/interface#Opener)
capability, so a byte range costs a `ReadAt` and not a whole-file read. A
driver that does not implement `Opener` still works; it just reads the file.

## Locking is real, not a stub

The macOS WebDAV client will not write to a server that does not advertise
class 2, so locking is not optional in practice. This module implements
exclusive write locks with genuine enforcement — a `LOCK` creates a token, a
conflicting request without that token is refused **423 Locked**, `If:` headers
are parsed, locks are depth-aware, they expire on a timeout and are swept.
A shared-lock request is refused rather than silently granted as exclusive.

It is a *server*-side lock table, in memory, scoped to the `Handler`: it
coordinates the clients of one export and makes no claim about anything else
touching the image. Locks do not survive a restart. That is stated because a
lock that quietly means less than the client thinks is worse than no lock.

## Isolation

The isolation boundary is the image file and the process. A `Handler` can
reach exactly one `Filesystem` and nothing else:

- it never opens a host path and never resolves a name against the host;
- every request path is percent-decoded **first** and then normalised, with
  `..` clamped at the root, so no URL — including one spelling the traversal
  `%2e%2e`, `%2E%2E%2F`, or a doubly-encoded `%252e%252e` — can name anything
  outside the image;
- symbolic links are **read, not followed**: a link's target is reported as a
  property and is never resolved by this server, so a link pointing at
  `/etc/passwd` is a string, not a door.

This is tested as a security property, not as a formality — see
`isolation_test.go`.

Embedding many is cheap by construction: a `Handler` is an `http.Handler` over
one `Filesystem`, so one image per tenant and one `Handler` per image works
either on a single mux or in a process each.

## Security posture

Exports are **read-only unless `ReadWrite()` is passed**. Most of what this is
pointed at is a forensic or build artefact, and an accidental write to one is
unrecoverable.

Nothing in this package reads a file, an environment variable, or a well-known
location. Every credential, every certificate, and the filesystem itself
arrive as arguments — there is no configuration surface a deployment could get
wrong from a distance.

```go
h, err := webdav.New(fsys,
    webdav.ReadWrite(),
    webdav.WithBasicAuth("img", func(user, pass string) bool {
        // Constant-time: a == comparison leaks the secret one byte at a time.
        return webdav.Verify(user, wantUser) && webdav.Verify(pass, wantPass)
    }),
)
```

`Verify` exists so that the obvious verifier is also the correct one. A
`Handler` with credentials configured answers **426 Upgrade Required** on a
plaintext connection unless the peer is loopback or the caller passed
`AllowInsecureAuth()` — Basic sends the password on every request with nothing
but base64 over it.

TLS is deliberately the caller's, because a `net/http.Server` is already where
certificates, ALPN and client-certificate policy live:

```go
srv := &http.Server{Addr: ":8443", Handler: h, TLSConfig: myTLSConfig}
log.Fatal(srv.ListenAndServeTLS("", ""))
```

## Verified against a real client

`fat32demo/` serves a genuine FAT32 image and is this repository's end-to-end
harness. The proof that matters is a client nobody here wrote reading bytes
out of a real on-disk image and getting the digest the driver gives directly:

```
driver /SUB/BIG.BIN : a2706a20394e48179a86c71e82c360c2960d3652340f9b9fdb355a42e3ac7691
curl   /SUB/BIG.BIN : a2706a20394e48179a86c71e82c360c2960d3652340f9b9fdb355a42e3ac7691
```

CI runs that on every push against three clients nobody here wrote: `curl`,
**cadaver**, and a **davfs2** kernel mount — plus the isolation attempts below
and a `Content-Range` round trip.

One honest caveat, because it was measured rather than assumed. Under the
davfs2 mount, `ls` returns `EINVAL`. That is a davfs2 client bug, not a
response this server gets wrong: with `debug most` its log shows
`FUSE_READDIR` failing **without issuing any HTTP request**, moments after the
same process parsed our `Depth: 1` PROPFIND (207, read in full) and logged
`added /SUB/` and `added /HELLO.TXT`. The listing was sent, accepted and
understood; davfs2 1.7.0 cannot hand it to the kernel on this FUSE version.
Reads through that same mount return the driver's exact digest, and cadaver —
the other neon-based client — lists both collections correctly. CI therefore
asserts the mount, the lookup, the read and the digest, and reports the
`readdir` quirk without failing on it.

## License

BSD-3-Clause.
