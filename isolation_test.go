package webdav_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-filesystems/webdav"
)

// The isolation boundary this module is deployed behind is the image file and
// the process: one disk image per tenant, one Handler per image, in user
// space. A Handler must therefore be able to reach exactly one Filesystem and
// nothing else — no host path, no name resolved against the host, and no URL
// that can name anything above the export root.
//
// A WebDAV URL is precisely where a client tries to escape, so the attempts
// are enumerated rather than argued about.

// spyFS records every path the handler asks its driver about, so a test can
// assert on what the driver was *asked*, not merely on what came back. A
// server that answered 404 while having asked its driver for
// "/../../etc/passwd" would pass a status-only test and still be broken.
type spyFS struct {
	*memFS
	asked []string
}

func (s *spyFS) record(p string) { s.asked = append(s.asked, p) }

func (s *spyFS) Stat(p string) (filesystem.Stat, error) { s.record(p); return s.memFS.Stat(p) }
func (s *spyFS) ReadFile(p string) ([]byte, error)      { s.record(p); return s.memFS.ReadFile(p) }
func (s *spyFS) ListDir(p string) ([]filesystem.DirEntry, error) {
	s.record(p)
	return s.memFS.ListDir(p)
}
func (s *spyFS) WriteFile(p string, d []byte, m os.FileMode) error {
	s.record(p)
	return s.memFS.WriteFile(p, d, m)
}
func (s *spyFS) MkDir(p string, m os.FileMode) error { s.record(p); return s.memFS.MkDir(p, m) }
func (s *spyFS) DeleteFile(p string) error           { s.record(p); return s.memFS.DeleteFile(p) }
func (s *spyFS) DeleteDir(p string) error            { s.record(p); return s.memFS.DeleteDir(p) }
func (s *spyFS) Rename(a, b string) error            { s.record(a); s.record(b); return s.memFS.Rename(a, b) }

func TestNoRequestPathEscapesTheExport(t *testing.T) {
	// Every spelling of the traversal a client can put in a URL. net/url has
	// already percent-decoded the path by the time a handler sees it, so
	// "%2e%2e" and ".." arrive as the same thing and are clamped together.
	paths := []string{
		"/../etc/passwd",
		"/../../../../../../etc/passwd",
		"/sub/../../etc/passwd",
		"/%2e%2e/etc/passwd",
		"/%2e%2e%2f%2e%2e%2fetc/passwd",
		"/sub/%2e%2e/%2e%2e/etc/passwd",
		"/./../.././etc/passwd",
		"//../etc/passwd",
		"/sub/..%2f..%2fetc/passwd",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			fs := &spyFS{memFS: newMemFS().dir("/sub").file("/sub/inside.txt", "x")}
			c, _ := serve(t, fs, webdav.ReadWrite())
			for _, m := range []string{"GET", "PROPFIND", "PUT", "DELETE", "MKCOL", "LOCK"} {
				c.do(m, p, "")
			}
			for _, asked := range fs.asked {
				if strings.Contains(asked, "..") {
					t.Fatalf("driver was asked for %q, which is outside the export", asked)
				}
				if !strings.HasPrefix(asked, "/") {
					t.Fatalf("driver was asked for %q, which is not an absolute path in the export", asked)
				}
			}
		})
	}
}

func TestClampedTraversalLandsBackInsideTheImage(t *testing.T) {
	// Clamping is not only refusal: "/../inside.txt" resolves to
	// "/inside.txt", which is a real resource of *this* image and of no
	// other. That is the containment guarantee stated positively.
	fs := newMemFS().file("/inside.txt", "the image's own bytes")
	c, _ := serve(t, fs)
	c.do("GET", "/../inside.txt", "").
		wantCode(t, http.StatusOK, "clamped traversal").
		wantContains(t, "the image's own bytes", "resolved inside the image")
	c.do("GET", "/../../../inside.txt", "").
		wantCode(t, http.StatusOK, "deeper clamped traversal")
}

func TestDestinationHeaderCannotEscape(t *testing.T) {
	// MOVE and COPY carry a second path, in a header rather than the URL,
	// and it goes through exactly the same normalisation.
	for _, m := range []string{"MOVE", "COPY"} {
		t.Run(m, func(t *testing.T) {
			fs := &spyFS{memFS: newMemFS().file("/a.txt", "x")}
			c, _ := serve(t, fs, webdav.ReadWrite())
			for _, dst := range []string{
				"/../outside.txt",
				"/%2e%2e/outside.txt",
				"http://evil.invalid/outside.txt",
				"/sub/../../outside.txt",
			} {
				c.do(m, "/a.txt", "", "Destination", dst)
			}
			for _, asked := range fs.asked {
				if strings.Contains(asked, "..") {
					t.Fatalf("driver was asked for %q via Destination", asked)
				}
			}
			if _, ok := fs.nodes["/outside.txt"]; ok {
				// It landed clamped at the root, which is inside the image —
				// the point is only that nothing above it was touched.
				t.Log("destination clamped to the export root, as designed")
			}
		})
	}
}

func TestDestinationOnAPrefixedHandlerCannotLeaveThePrefix(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "x"), webdav.ReadWrite(), webdav.WithPrefix("/dav"))
	// A Destination outside the mount point is another namespace, and 502 is
	// what RFC 4918 says about one this server cannot reach.
	c.do("MOVE", "/dav/a.txt", "", "Destination", "/elsewhere/b.txt").
		wantCode(t, http.StatusBadGateway, "destination outside the prefix")
	c.do("MOVE", "/dav/a.txt", "", "Destination", "/dav/b.txt").
		wantCode(t, http.StatusCreated, "destination inside the prefix")
}

func TestNulInAPathIsRefused(t *testing.T) {
	// net/url refuses a NUL, so no HTTP client can send one — but a caller
	// that mounts this behind middleware which rewrites URL.Path can put one
	// there, which is why the check is in the handler rather than assumed
	// away. The request is therefore built with the NUL written into Path
	// after parsing, which is exactly what such middleware does.
	h, err := webdav.New(newMemFS())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest("GET", "/placeholder", nil)
	req.URL.Path = "/a\x00b.txt"
	req.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
}

func TestAnAbsurdlyLongPathIsRefused(t *testing.T) {
	c, _ := serve(t, newMemFS())
	c.do("GET", "/"+strings.Repeat("a", 5000), "").
		wantCode(t, http.StatusBadRequest, "over-long path")
}

func TestSymlinkTargetIsNeverResolved(t *testing.T) {
	// The one place a name inside an image could point outside it is a
	// symbolic link. This server reads links as metadata and never follows
	// one: there is no code path from a link's target to a driver call.
	fs := &spyFS{memFS: newMemFS()}
	fs.symlink("/escape", "../../../../etc/passwd")
	c, _ := serve(t, fs)
	c.do("GET", "/escape", "")
	c.do("PROPFIND", "/escape", propfindAll, "Depth", "0")
	for _, asked := range fs.asked {
		if asked != "/escape" && asked != "/" {
			t.Fatalf("a symlink made the driver reach for %q", asked)
		}
	}
}

func TestTwoHandlersOverTwoImagesCannotSeeEachOther(t *testing.T) {
	// The deployment: one image per tenant, one Handler per image. Nothing
	// is shared — not the lock table, not the export, not a cache.
	a := newMemFS().file("/tenant.txt", "alpha")
	b := newMemFS().file("/tenant.txt", "bravo")
	ca, _ := serve(t, a, webdav.ReadWrite())
	cb, _ := serve(t, b, webdav.ReadWrite())

	ca.do("GET", "/tenant.txt", "").wantContains(t, "alpha", "first tenant")
	cb.do("GET", "/tenant.txt", "").wantContains(t, "bravo", "second tenant")

	ca.do("PUT", "/only-in-a.txt", "x").wantCode(t, http.StatusCreated, "write to the first")
	cb.do("GET", "/only-in-a.txt", "").wantCode(t, http.StatusNotFound, "not visible to the second")

	// A lock held on one is not held on the other, and one tenant's token is
	// not a credential on another's Handler.
	tok := tokenFrom(t, ca.do("LOCK", "/tenant.txt", lockBody))
	cb.do("PUT", "/tenant.txt", "y").wantCode(t, http.StatusNoContent, "the other tenant is unaffected")
	ca.do("PUT", "/tenant.txt", "y", "If", "(<"+tok+">)").
		wantCode(t, http.StatusNoContent, "the token works where it was issued")
}

func TestManyHandlersAreCheap(t *testing.T) {
	// One Handler per image, many per machine: construction must not start a
	// goroutine, open a listener, read a file or allocate a table. This
	// asserts the observable part — that a thousand of them construct
	// quickly and independently — which is what the deployment needs.
	start := time.Now()
	for i := 0; i < 1000; i++ {
		h, err := webdav.New(newMemFS())
		if err != nil {
			t.Fatalf("New #%d: %v", i, err)
		}
		if h == nil {
			t.Fatal("nil handler")
		}
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("1000 handlers took %v", d)
	}
}

func TestHandlerNeverClosesTheFilesystemItWasGiven(t *testing.T) {
	// This module did not open the image, and the caller may still want it —
	// the same rule go-filesystems/nfs states for its exports.
	fs := newMemFS().file("/a.txt", "x")
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("GET", "/a.txt", "")
	c.do("PUT", "/b.txt", "y")
	c.do("DELETE", "/b.txt", "")
	if fs.closed {
		t.Fatal("the handler closed a filesystem it did not open")
	}
}
