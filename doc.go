// Package webdav implements a read/write WebDAV server (RFC 4918) that
// exports any [github.com/go-filesystems/interface.Filesystem] as an
// [net/http.Handler].
//
// It turns every go-filesystems driver — ext4, xfs, btrfs, zfs, ntfs, fat32,
// exfat, hfsplus, apfs, iso9660, squashfs, ufs, ffs, uefi, oci — into
// something a browser can read, a Finder or Explorer window can mount, and
// `curl` can drive, from one pure-Go binary with no cgo and no root.
//
// # Why WebDAV alongside NFS
//
// [github.com/go-filesystems/nfs] already makes a Filesystem mountable. This
// module exists because HTTP gives three things NFSv3 structurally cannot,
// and each of them decided part of this design:
//
//   - Authentication and confidentiality. NFSv3's AUTH_UNIX is a claim, not a
//     proof: the client says "uid 501" and the wire cannot disagree, and
//     there is no encryption at all. That is acceptable on 127.0.0.1 and
//     unusable anywhere else. WebDAV is HTTP, so it gets Basic and Bearer
//     authentication and TLS from the transport underneath it — see
//     [WithBasicAuth], [WithBearerAuth] and the TLS note below.
//   - A write model that matches the contract as it stands today. NFS WRITE
//     names an offset, and [github.com/go-filesystems/interface.Filesystem]
//     has no positional write, so the NFS server has to read the file, splice
//     and write it back — O(filesize) per request, measured at 90 kB/s over a
//     real mount. WebDAV PUT sends the whole body and is therefore exactly
//     [github.com/go-filesystems/interface.Filesystem.WriteFile]: one write
//     of one file, at the driver's own speed. And where a client does want a
//     partial write, a PUT carrying a Content-Range is served through
//     [github.com/go-filesystems/interface.WritableFile] — one WriteAt, not a
//     read-splice-write — or refused 501 if the driver has no positional
//     write, rather than emulated at the cost the client was trying to avoid.
//   - A client every machine already has. A GET on a file returns its bytes,
//     so a browser, `curl`, `wget` and every HTTP library are clients without
//     mounting anything.
//
// The cost is equally honest: WebDAV is not a POSIX filesystem. There are no
// file descriptors, no byte-range locks, no partial writes, and a mounted
// WebDAV volume rewrites a whole file on every save.
//
// # Serving one
//
// The handler is an ordinary [net/http.Handler], which is what makes it cheap
// to embed and cheap to run many of: one image per tenant, one Handler per
// image, either on one mux or one process each.
//
//	fsys, err := fat32.Open("disk.img", -1)
//	if err != nil {
//		return err
//	}
//	defer fsys.Close()
//
//	h, err := webdav.New(fsys, webdav.ReadWrite())
//	if err != nil {
//		return err
//	}
//	return http.ListenAndServe("127.0.0.1:8080", h)
//
// # TLS is the caller's, deliberately
//
// This package does not open a listener and has no TLS configuration of its
// own. A [net/http.Handler] is served over TLS by the [net/http.Server] that
// wraps it, which is where a caller's certificates, key material, ALPN, and
// client-certificate policy already live:
//
//	srv := &http.Server{Addr: ":8443", Handler: h, TLSConfig: myTLSConfig}
//	return srv.ListenAndServeTLS("", "")
//
// Owning a TLSConfig here would mean either reading key material from a fixed
// path — which an embedded caller must never have decided for it — or
// duplicating a configuration surface the standard library already has.
// Nothing in this package reads a file, an environment variable or a
// well-known location: every credential, every certificate and the filesystem
// itself arrive as arguments.
//
// # Isolation
//
// The isolation boundary is the image file and the process, not this code.
// A Handler can reach exactly one Filesystem and nothing else: it never opens
// a host path, never resolves a name against the host, and every request path
// is normalised with ".." clamped at the root before it is used, so no URL —
// including one that spells the traversal "%2e%2e" — can name anything
// outside the image. Symbolic links are read, not followed: a link's target
// is reported as a property and is never resolved by this server.
//
// # Security posture
//
// Exports are read-only unless [ReadWrite] is passed; most of what this is
// pointed at is a forensic or build artefact, and an accidental write to one
// is unrecoverable.
//
// Authentication is optional and, when configured, refused over cleartext:
// Basic sends the password in every request with nothing but base64 over it,
// so a Handler with credentials configured answers 426 Upgrade Required on a
// plaintext connection unless the peer is loopback or the caller passed
// [AllowInsecureAuth]. See [WithBasicAuth].
package webdav
