package webdav_test

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/go-filesystems/webdav"
)

func TestPutCreatesThenReplaces(t *testing.T) {
	fs := newMemFS()
	c, _ := serve(t, fs, webdav.ReadWrite())
	r := c.do("PUT", "/new.txt", "first").wantCode(t, http.StatusCreated, "PUT creating")
	if r.header.Get("Location") != "/new.txt" {
		t.Fatalf("Location = %q", r.header.Get("Location"))
	}
	if r.header.Get("ETag") == "" {
		t.Fatal("PUT must return an ETag so a client can revalidate its upload")
	}
	c.do("GET", "/new.txt", "").wantContains(t, "first", "body after create")
	// A replacement is 204, not 201: the resource already existed.
	c.do("PUT", "/new.txt", "second").wantCode(t, http.StatusNoContent, "PUT replacing")
	c.do("GET", "/new.txt", "").wantContains(t, "second", "body after replace")
}

// TestPutIsOneWholeFileWrite is the point of the module. PUT carries the
// whole body, which is exactly WriteFile, so one PUT is one driver write —
// not the read-modify-write per offset that caps go-filesystems/nfs at 90
// kB/s.
func TestPutIsOneWholeFileWrite(t *testing.T) {
	fs := &countingFS{memFS: newMemFS()}
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("PUT", "/big.bin", strings.Repeat("x", 1<<20)).
		wantCode(t, http.StatusCreated, "1 MiB PUT")
	if fs.writes != 1 {
		t.Fatalf("a 1 MiB PUT cost %d WriteFile calls, want exactly 1", fs.writes)
	}
	if fs.reads != 0 {
		t.Fatalf("a PUT cost %d ReadFile calls; it must never read to write", fs.reads)
	}
}

func TestPutRefusesTheRoot(t *testing.T) {
	c, _ := serve(t, newMemFS(), webdav.ReadWrite())
	c.do("PUT", "/", "x").wantCode(t, http.StatusMethodNotAllowed, "PUT on the root")
}

func TestPutWithNoParentIsConflictNotNotFound(t *testing.T) {
	// 409, not 404: a 404 would tell the client the file it named is gone.
	c, _ := serve(t, newMemFS(), webdav.ReadWrite())
	c.do("PUT", "/missing/a.txt", "x").wantCode(t, http.StatusConflict, "PUT with no parent")
}

func TestPutIntoANonCollectionIsConflict(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "x"), webdav.ReadWrite())
	c.do("PUT", "/a.txt/b.txt", "y").wantCode(t, http.StatusConflict, "parent is a file")
}

func TestPutReportsADriverFailure(t *testing.T) {
	fs := newMemFS()
	fs.failWith("WriteFile:/a.txt", errors.New("no space left on device"))
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("PUT", "/a.txt", "x").wantCode(t, webdav.StatusInsufficientStorage, "full disk")
	fs2 := newMemFS()
	fs2.failWith("Stat:/", errors.New("media error"))
	c2, _ := serve(t, fs2, webdav.ReadWrite())
	c2.do("PUT", "/a.txt", "x").wantCode(t, http.StatusInternalServerError, "unreadable parent")
}

func TestPutBodyIsBounded(t *testing.T) {
	// WriteFile takes a []byte, so a PUT is buffered in full; an unbounded
	// read from a network peer would be an unbounded allocation here.
	c, _ := serve(t, newMemFS(), webdav.ReadWrite(), webdav.WithMaxBody(16))
	c.do("PUT", "/a.txt", strings.Repeat("x", 17)).
		wantCode(t, http.StatusRequestEntityTooLarge, "over the bound")
	c.do("PUT", "/b.txt", strings.Repeat("x", 16)).
		wantCode(t, http.StatusCreated, "at the bound")
}

func TestWithMaxBodyRestoresTheDefaultRatherThanRemovingTheBound(t *testing.T) {
	h, err := webdav.New(newMemFS(), webdav.ReadWrite(), webdav.WithMaxBody(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if h == nil {
		t.Fatal("nil handler")
	}
}

func TestMkcol(t *testing.T) {
	c, _ := serve(t, newMemFS(), webdav.ReadWrite())
	r := c.do("MKCOL", "/sub", "").wantCode(t, http.StatusCreated, "MKCOL")
	if r.header.Get("Location") != "/sub/" {
		t.Fatalf("Location = %q, want a trailing slash on a collection", r.header.Get("Location"))
	}
	c.do("MKCOL", "/sub", "").wantCode(t, http.StatusMethodNotAllowed, "MKCOL twice")
	c.do("MKCOL", "/missing/sub", "").wantCode(t, http.StatusConflict, "MKCOL with no parent")
	c.do("PUT", "/a.txt", "x")
	c.do("MKCOL", "/a.txt/sub", "").wantCode(t, http.StatusConflict, "parent is a file")
	// RFC 4918 section 9.3: a body MKCOL does not understand is 415, not a
	// silently ignored body. Extended MKCOL is not implemented, so every
	// body is one it does not understand.
	c.do("MKCOL", "/withbody", "<x/>").wantCode(t, http.StatusUnsupportedMediaType, "MKCOL with a body")
}

func TestMkcolReportsADriverFailure(t *testing.T) {
	fs := newMemFS()
	fs.failWith("MkDir:/sub", errors.New("media error"))
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("MKCOL", "/sub", "").wantCode(t, http.StatusInternalServerError, "MkDir failure")
	fs2 := newMemFS()
	fs2.failWith("Stat:/", errors.New("media error"))
	c2, _ := serve(t, fs2, webdav.ReadWrite())
	c2.do("MKCOL", "/sub", "").wantCode(t, http.StatusInternalServerError, "unreadable parent")
}

func TestDelete(t *testing.T) {
	fs := newMemFS().file("/a.txt", "x").dir("/sub").file("/sub/b.txt", "y").dir("/sub/deep").file("/sub/deep/c.txt", "z")
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("DELETE", "/a.txt", "").wantCode(t, http.StatusNoContent, "DELETE a file")
	c.do("GET", "/a.txt", "").wantCode(t, http.StatusNotFound, "gone")
	// DELETE on a collection is depth-infinity by definition: there is no
	// way to ask for a shallow one.
	c.do("DELETE", "/sub", "").wantCode(t, http.StatusNoContent, "DELETE a tree")
	for _, p := range []string{"/sub", "/sub/b.txt", "/sub/deep", "/sub/deep/c.txt"} {
		if _, ok := fs.nodes[p]; ok {
			t.Fatalf("%s survived a depth-infinity DELETE", p)
		}
	}
	c.do("DELETE", "/nope", "").wantCode(t, http.StatusNotFound, "DELETE missing")
	c.do("DELETE", "/", "").wantCode(t, http.StatusForbidden, "DELETE the root")
}

func TestDeletePartialFailureIsA207NamingTheSurvivors(t *testing.T) {
	fs := newMemFS().dir("/sub").file("/sub/ok.txt", "x").file("/sub/stuck.txt", "y")
	fs.failWith("DeleteFile:/sub/stuck.txt", errors.New("media error"))
	c, _ := serve(t, fs, webdav.ReadWrite())
	// RFC 4918 section 9.6.1: the client learns what is left, not what is
	// gone.
	c.do("DELETE", "/sub", "").
		wantCode(t, webdav.StatusMulti, "partial DELETE").
		wantContains(t, "/sub/stuck.txt", "the member that survived").
		wantLacks(t, "ok.txt", "removed members are not listed")
	if _, ok := fs.nodes["/sub"]; !ok {
		t.Fatal("a collection with a surviving member must not itself be removed")
	}
}

func TestDeleteReportsAFailedListingAndAFailedRmdir(t *testing.T) {
	fs := newMemFS().dir("/sub")
	fs.failWith("ListDir:/sub", errors.New("media error"))
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("DELETE", "/sub", "").wantCode(t, webdav.StatusMulti, "unlistable collection").
		wantContains(t, "/sub/", "the collection's own row")

	fs2 := newMemFS().dir("/sub")
	fs2.failWith("DeleteDir:/sub", errors.New("media error"))
	c2, _ := serve(t, fs2, webdav.ReadWrite())
	c2.do("DELETE", "/sub", "").wantCode(t, webdav.StatusMulti, "undeletable collection")

	fs3 := newMemFS().dir("/sub").file("/sub/bad.txt", "x")
	fs3.failWith("Stat:/sub/bad.txt", errors.New("media error"))
	c3, _ := serve(t, fs3, webdav.ReadWrite())
	c3.do("DELETE", "/sub", "").wantCode(t, webdav.StatusMulti, "unstatable member")
}

func TestMove(t *testing.T) {
	fs := newMemFS().file("/a.txt", "hello").dir("/sub")
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("MOVE", "/a.txt", "", "Destination", "/sub/b.txt").
		wantCode(t, http.StatusCreated, "MOVE")
	c.do("GET", "/sub/b.txt", "").wantContains(t, "hello", "moved body")
	c.do("GET", "/a.txt", "").wantCode(t, http.StatusNotFound, "source is gone")
}

func TestMoveOverwrite(t *testing.T) {
	fs := newMemFS().file("/a.txt", "new").file("/b.txt", "old")
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("MOVE", "/a.txt", "", "Destination", "/b.txt", "Overwrite", "F").
		wantCode(t, http.StatusPreconditionFailed, "Overwrite F onto an existing target")
	// The default is T, so a MOVE with no Overwrite header replaces.
	c.do("MOVE", "/a.txt", "", "Destination", "/b.txt").
		wantCode(t, http.StatusNoContent, "overwriting MOVE")
	c.do("GET", "/b.txt", "").wantContains(t, "new", "target was replaced")
}

func TestMoveAndCopyDestinationChecks(t *testing.T) {
	// Each method gets its own server: MOVE consumes its source, so sharing
	// one would make the second subtest depend on the first's leftovers.
	for _, m := range []string{"MOVE", "COPY"} {
		t.Run(m, func(t *testing.T) {
			fs := newMemFS().file("/a.txt", "x").dir("/sub").file("/f.txt", "y")
			c, srv := serve(t, fs, webdav.ReadWrite())
			c.do(m, "/a.txt", "").wantCode(t, http.StatusBadRequest, "no Destination")
			c.do(m, "/a.txt", "", "Destination", "://bad").
				wantCode(t, http.StatusBadRequest, "unparseable Destination")
			// Another host is 502, not 400: the request is well formed, the
			// server simply cannot reach across namespaces.
			c.do(m, "/a.txt", "", "Destination", "http://elsewhere.invalid/b.txt").
				wantCode(t, http.StatusBadGateway, "another host")
			c.do(m, "/a.txt", "", "Destination", "/a.txt").
				wantCode(t, http.StatusForbidden, "destination equals source")
			c.do(m, "/sub", "", "Destination", "/sub/inner").
				wantCode(t, http.StatusForbidden, "destination inside the source")
			c.do(m, "/a.txt", "", "Destination", "/", "Overwrite", "T").
				wantCode(t, http.StatusForbidden, "overwriting the root")
			c.do(m, "/a.txt", "", "Destination", "/b.txt", "Overwrite", "maybe").
				wantCode(t, http.StatusBadRequest, "bad Overwrite")
			c.do(m, "/a.txt", "", "Destination", "/missing/b.txt").
				wantCode(t, http.StatusConflict, "destination parent missing")
			c.do(m, "/a.txt", "", "Destination", "/f.txt/b.txt").
				wantCode(t, http.StatusConflict, "destination parent is a file")
			c.do(m, "/nope", "", "Destination", "/b.txt").
				wantCode(t, http.StatusNotFound, "missing source")
			// The same host, spelled in full, is this server.
			c.do(m, "/a.txt", "", "Destination", srv.URL+"/done.txt").
				wantCode(t, http.StatusCreated, "absolute Destination on this host")
			c.do(m, "/", "", "Destination", "/x").
				wantCode(t, http.StatusForbidden, "the root is not a source")
		})
	}
}

func TestMoveReportsDriverFailures(t *testing.T) {
	fs := newMemFS().file("/a.txt", "x")
	fs.failWith("Rename:/a.txt", errors.New("media error"))
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("MOVE", "/a.txt", "", "Destination", "/b.txt").
		wantCode(t, http.StatusInternalServerError, "Rename failure")

	fs2 := newMemFS().file("/a.txt", "x").file("/b.txt", "y")
	fs2.failWith("Stat:/b.txt", errors.New("media error"))
	c2, _ := serve(t, fs2, webdav.ReadWrite())
	c2.do("MOVE", "/a.txt", "", "Destination", "/b.txt").
		wantCode(t, http.StatusInternalServerError, "unstatable destination")

	fs3 := newMemFS().file("/a.txt", "x").file("/b.txt", "y")
	fs3.failWith("DeleteFile:/b.txt", errors.New("media error"))
	c3, _ := serve(t, fs3, webdav.ReadWrite())
	c3.do("MOVE", "/a.txt", "", "Destination", "/b.txt").
		wantCode(t, webdav.StatusMulti, "undeletable destination")
}

func TestCopy(t *testing.T) {
	fs := newMemFS().file("/a.txt", "hello").dir("/sub").file("/sub/b.txt", "world").dir("/sub/deep").file("/sub/deep/c.txt", "!")
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("COPY", "/a.txt", "", "Destination", "/copy.txt").
		wantCode(t, http.StatusCreated, "COPY a file")
	c.do("GET", "/copy.txt", "").wantContains(t, "hello", "copied body")
	c.do("GET", "/a.txt", "").wantContains(t, "hello", "source survives")

	// Depth infinity is the default for COPY.
	c.do("COPY", "/sub", "", "Destination", "/deepcopy").
		wantCode(t, http.StatusCreated, "COPY a tree")
	c.do("GET", "/deepcopy/deep/c.txt", "").wantContains(t, "!", "recursive copy")

	// Depth 0 on a collection copies the collection with no members, which
	// is what RFC 4918 section 9.8.3 says it means.
	c.do("COPY", "/sub", "", "Destination", "/shallow", "Depth", "0").
		wantCode(t, http.StatusCreated, "COPY depth 0")
	c.do("GET", "/shallow/b.txt", "").wantCode(t, http.StatusNotFound, "depth 0 copies no members")

	c.do("COPY", "/a.txt", "", "Destination", "/x.txt", "Depth", "1").
		wantCode(t, http.StatusBadRequest, "COPY depth 1 has no meaning")
	c.do("COPY", "/a.txt", "", "Destination", "/x.txt", "Depth", "2").
		wantCode(t, http.StatusBadRequest, "bad Depth")
}

func TestCopyReportsDriverFailures(t *testing.T) {
	fs := newMemFS().file("/a.txt", "x")
	fs.failWith("ReadFile:/a.txt", errors.New("media error"))
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("COPY", "/a.txt", "", "Destination", "/b.txt").
		wantCode(t, webdav.StatusMulti, "unreadable source")

	fs2 := newMemFS().dir("/sub")
	fs2.failWith("MkDir:/copy", errors.New("media error"))
	c2, _ := serve(t, fs2, webdav.ReadWrite())
	c2.do("COPY", "/sub", "", "Destination", "/copy").
		wantCode(t, webdav.StatusMulti, "undirectorable destination")

	fs3 := newMemFS().dir("/sub")
	fs3.failWith("ListDir:/sub", errors.New("media error"))
	c3, _ := serve(t, fs3, webdav.ReadWrite())
	c3.do("COPY", "/sub", "", "Destination", "/copy").
		wantCode(t, webdav.StatusMulti, "unlistable source")

	fs4 := newMemFS().dir("/sub").file("/sub/bad.txt", "x")
	fs4.failWith("Stat:/sub/bad.txt", errors.New("media error"))
	c4, _ := serve(t, fs4, webdav.ReadWrite())
	c4.do("COPY", "/sub", "", "Destination", "/copy").
		wantCode(t, webdav.StatusMulti, "unstatable member")
}

// TestPutWithATruncatedBodyWritesNothing sends a Content-Length the body does
// not fulfil and then hangs up.
//
// Half a file at the client's chosen name is worse than no file, because the
// client believes the upload succeeded the moment it sees any 2xx — and this
// is the one path where a server that reads with io.ReadAll and ignores the
// error writes exactly that. It needs a raw connection: no HTTP client will
// send a body shorter than the length it announced.
func TestPutWithATruncatedBodyWritesNothing(t *testing.T) {
	fs := newMemFS()
	c, srv := serve(t, fs, webdav.ReadWrite())
	_ = c
	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_, err = io.WriteString(conn, "PUT /truncated.txt HTTP/1.1\r\nHost: "+addr+
		"\r\nContent-Length: 1000\r\n\r\nonly-a-few-bytes")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	// A half-close leaves the server reading a body that will never arrive.
	if tc, ok := conn.(*net.TCPConn); ok {
		if err := tc.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
	}
	_, _ = io.ReadAll(conn)
	conn.Close()
	// Whatever the server told the (now departed) client, the image must not
	// hold a partial file.
	if _, ok := fs.nodes["/truncated.txt"]; ok {
		t.Fatal("a truncated PUT was written to the image")
	}
}
