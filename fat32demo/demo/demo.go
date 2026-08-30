// Package demo serves a FAT32 image over WebDAV so it can be read by a real
// client — curl, a browser, the macOS Finder, the Windows Explorer.
//
// It lives in its own module, mirroring go-filesystems/nfs's fat32demo, so
// that the core webdav module never acquires a dependency on a concrete
// driver: a driver's `replace github.com/go-filesystems/interface => ../interface`
// does not survive transitive importing, which is exactly the breakage that
// split exists to avoid.
//
// It is also this repository's real-image harness. What proves the server is
// not merely self-consistent is a client that was not written here reading
// bytes out of a genuine on-disk image and getting the same digest the driver
// gives directly.
package demo

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	fat32 "github.com/go-filesystems/fat32"
	filesystem "github.com/go-filesystems/interface"
	"github.com/go-filesystems/webdav"
)

// newHandler is [webdav.New], indirected so the failure path in Setup that
// cannot happen in production — a dead CSPRNG — is still reachable from a
// test. An error path that has never been executed is an error path that has
// never been shown to clean up after itself, and this one must close the
// driver it already opened.
var newHandler = webdav.New

// Setup opens the image, builds the handler and returns it with a listener
// bound to addr, without accepting anything yet. Splitting it out of [Main]
// is what makes the whole program reachable from a test: a caller can take
// the real listener's address before a single request arrives.
//
// The returned handler owns nothing but the export; the caller closes the
// filesystem and the listener.
func Setup(image, addr string, readWrite bool, out io.Writer) (http.Handler, filesystem.Filesystem, net.Listener, error) {
	fi, err := os.Stat(image)
	if err != nil {
		return nil, nil, nil, err
	}
	fsys, err := fat32.Open(image, -1)
	if err != nil {
		return nil, nil, nil, err
	}
	// The image's size is the one capacity figure actually known here; the
	// Filesystem contract has no statfs, so without this a mounted volume
	// would report an unknown size. Free space is genuinely unknown, hence 0.
	opts := []webdav.Option{webdav.WithCapacity(uint64(fi.Size()), 0)}
	if readWrite {
		opts = append(opts, webdav.ReadWrite())
	}
	h, err := newHandler(fsys, opts...)
	if err != nil {
		fsys.Close()
		return nil, nil, nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fsys.Close()
		return nil, nil, nil, err
	}
	fmt.Fprintf(out, "serving %s on http://%s/\n", image, ln.Addr())
	fmt.Fprintf(out, "  curl:   curl -s http://%s/\n", ln.Addr())
	fmt.Fprintf(out, "  Finder: Go > Connect to Server > http://%s/\n", ln.Addr())
	fmt.Fprintf(out, "  mount:  sudo mount_webdav -S http://%s/ /Volumes/img   (macOS, needs root)\n", ln.Addr())
	return h, fsys, ln, nil
}

// Digest reports the SHA-256 of a file as the *driver* returns it, with no
// HTTP anywhere in the path.
//
// It exists so that a verification run has something to compare the client's
// bytes against that is not itself the server. Two numbers from the same code
// path always agree; this is the other number.
func Digest(fsys filesystem.Filesystem, path string) (string, error) {
	data, err := fsys.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// serveHTTP is [net/http.Server.Serve], indirected so a test can end the
// loop.
//
// Main returns only when its listener dies, and a test that had to kill the
// process to reach that return would be testing the process, not the program.
// With the indirection a test can drive a real request through Main's own
// server and then close it, which is the whole of what Main does.
var serveHTTP = func(srv *http.Server, ln net.Listener) error { return srv.Serve(ln) }

// Main parses args and serves until the listener fails. It returns a process
// exit status.
func Main(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("fat32demo", flag.ContinueOnError)
	fs.SetOutput(errOut)
	image := fs.String("image", "", "path to a FAT32 image")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	rw := fs.Bool("rw", false, "export read-write (default read-only)")
	digest := fs.String("digest", "", "print the driver's SHA-256 of this path and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// In digest mode the banner is suppressed: the only thing on stdout must
	// be the digest, so that a shell can compare it with what a client got
	// without having to parse anything out of the way.
	banner := out
	if *digest != "" {
		banner = io.Discard
	}
	h, fsys, ln, err := Setup(*image, *addr, *rw, banner)
	if err != nil {
		fmt.Fprintln(errOut, "fat32demo:", err)
		return 1
	}
	defer fsys.Close()
	if *digest != "" {
		ln.Close()
		sum, err := Digest(fsys, *digest)
		if err != nil {
			fmt.Fprintln(errOut, "fat32demo:", err)
			return 1
		}
		fmt.Fprintln(out, sum)
		return 0
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintln(errOut, "fat32demo:", serveHTTP(srv, ln))
	return 1
}
