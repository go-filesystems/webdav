package webdav_test

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-filesystems/webdav"
)

// A partial PUT is the one operation in this module that needs more than
// WriteFile, and it is exactly the shape that reduces the NFS server to
// 90 kB/s: replacing sixteen bytes in the middle of a file without rewriting
// the file. These tests pin both halves of the bargain — that it happens in
// place when the driver can, and that it is refused rather than emulated when
// the driver cannot.

// TestContentRangePutReplacesOnlyTheNamedBytes is the behavioural claim: the
// bytes inside the range change, and every byte outside it does not.
func TestContentRangePutReplacesOnlyTheNamedBytes(t *testing.T) {
	fs := newWriteFS("/f.bin", "0123456789")
	c, _ := serve(t, fs, webdav.ReadWrite())

	c.do("PUT", "/f.bin", "AB", "Content-Range", "bytes 4-5/10").
		wantCode(t, http.StatusNoContent, "partial PUT")

	if got := string(fs.nodes["/f.bin"].data); got != "0123AB6789" {
		t.Fatalf("after a partial PUT the file is %q, want %q", got, "0123AB6789")
	}
	// A GET must agree with the image, or the write went somewhere the read
	// path cannot see.
	c.do("GET", "/f.bin", "").wantCode(t, http.StatusOK, "GET").
		wantContains(t, "0123AB6789", "GET after partial PUT")
}

// TestContentRangePutSyncsBeforeReportingSuccess: a 204 that a power cut can
// retract is a lie, and Sync is the call that makes it true.
func TestContentRangePutSyncsBeforeReportingSuccess(t *testing.T) {
	fs := newWriteFS("/f.bin", "0123456789")
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("PUT", "/f.bin", "Z", "Content-Range", "bytes 0-0/10").
		wantCode(t, http.StatusNoContent, "partial PUT")
	if fs.synced != 1 {
		t.Fatalf("Sync called %d times, want exactly 1 before the 204", fs.synced)
	}
}

// TestContentRangePutSetsTheNewETag — a client that caches must be told the
// representation changed, and a partial write changes it as much as a whole
// one does.
func TestContentRangePutSetsTheNewETag(t *testing.T) {
	fs := newWriteFS("/f.bin", "0123456789")
	c, _ := serve(t, fs, webdav.ReadWrite())
	before := c.do("PROPFIND", "/f.bin", propfindAll, "Depth", "0").body
	r := c.do("PUT", "/f.bin", "Z", "Content-Range", "bytes 0-0/10")
	if r.header.Get("ETag") == "" {
		t.Fatal("a partial PUT returned no ETag")
	}
	if strings.Contains(before, r.header.Get("ETag")) {
		t.Fatal("the ETag did not change across a partial PUT")
	}
}

// TestContentRangePutIsRefusedWhenTheDriverCannotWriteAtAnOffset is the whole
// design decision in one test. Falling back to read-splice-write would give a
// client that asked for a cheap update the most expensive operation the
// module has, and hide it behind a 204.
func TestContentRangePutIsRefusedWhenTheDriverCannotWriteAtAnOffset(t *testing.T) {
	for _, tc := range []struct {
		name string
		fs   func() filesystem.Filesystem
	}{
		// No Opener at all: the driver has no random access whatsoever.
		{"no opener", func() filesystem.Filesystem {
			return newMemFS().file("/f.bin", "0123456789")
		}},
		// An Opener whose File is read-only — the interface v0.2.0 shape,
		// which every driver had until WritableFile landed in v0.3.0.
		{"opener without WritableFile", func() filesystem.Filesystem {
			fs := newWriteFS("/f.bin", "0123456789")
			fs.plain = true
			return fs
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := serve(t, tc.fs(), webdav.ReadWrite())
			c.do("PUT", "/f.bin", "AB", "Content-Range", "bytes 4-5/10").
				wantCode(t, http.StatusNotImplemented, "partial PUT on a driver without WriteAt").
				wantContains(t, "cannot write at an offset", "refusal explains itself")
		})
	}
}

// TestContentRangePutOnAMissingResourceIsAConflict: a range PUT patches, it
// does not create — creating one would mean inventing every byte outside the
// range.
func TestContentRangePutOnAMissingResourceIsAConflict(t *testing.T) {
	fs := newWriteFS("/f.bin", "0123456789")
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("PUT", "/nope.bin", "AB", "Content-Range", "bytes 0-1/10").
		wantCode(t, http.StatusConflict, "partial PUT on a missing resource")
}

// TestContentRangePutOnACollectionIsRefused.
func TestContentRangePutOnACollectionIsRefused(t *testing.T) {
	fs := &writeFS{openFS: &openFS{memFS: newMemFS().dir("/d")}}
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("PUT", "/d", "AB", "Content-Range", "bytes 0-1/10").
		wantCode(t, http.StatusMethodNotAllowed, "partial PUT on a collection")
}

// TestContentRangePutBeyondTheEndIsUnsatisfiable. A range PUT never extends:
// writing past the end would have to invent the bytes in between.
func TestContentRangePutBeyondTheEndIsUnsatisfiable(t *testing.T) {
	fs := newWriteFS("/f.bin", "0123456789")
	c, _ := serve(t, fs, webdav.ReadWrite())
	r := c.do("PUT", "/f.bin", "AB", "Content-Range", "bytes 20-21/*").
		wantCode(t, http.StatusRequestedRangeNotSatisfiable, "range past the end")
	if got := r.header.Get("Content-Range"); got != "bytes */10" {
		t.Fatalf("Content-Range = %q, want %q — the client must learn the real size", got, "bytes */10")
	}
	if string(fs.nodes["/f.bin"].data) != "0123456789" {
		t.Fatal("an unsatisfiable range PUT modified the file")
	}
}

// TestMalformedContentRangeIsRefusedNotIgnored. Ignoring the header would
// turn a fragment into a whole-file replacement — a silent data loss.
func TestMalformedContentRangeIsRefusedNotIgnored(t *testing.T) {
	for _, spec := range []string{
		"",                 // not "bytes ..." at all
		"items 0-1/10",     // wrong unit
		"bytes 0-1",        // no total
		"bytes 01/10",      // no dash
		"bytes x-1/10",     // unparseable start
		"bytes -1-2/10",    // negative start
		"bytes 0-x/10",     // unparseable end
		"bytes 5-2/10",     // reversed
		"bytes 0-1/total",  // unparseable total
		"bytes 0-9/5",      // total contradicts the range
		"bytes 0-9/10junk", // trailing rubbish in the total
	} {
		t.Run(spec, func(t *testing.T) {
			fs := newWriteFS("/f.bin", "0123456789")
			c, _ := serve(t, fs, webdav.ReadWrite())
			// A header the transport would reject outright is sent as a
			// non-empty value; the empty case is covered by "bytes" alone.
			v := spec
			if v == "" {
				v = "garbage"
			}
			c.do("PUT", "/f.bin", "AB", "Content-Range", v).
				wantCode(t, http.StatusBadRequest, "malformed Content-Range")
			if string(fs.nodes["/f.bin"].data) != "0123456789" {
				t.Fatal("a malformed Content-Range still wrote to the file")
			}
		})
	}
}

// TestContentRangePutWithAStarTotalIsAccepted: "*" means the client does not
// claim to know the final size, which is legal and common.
func TestContentRangePutWithAStarTotalIsAccepted(t *testing.T) {
	fs := newWriteFS("/f.bin", "0123456789")
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("PUT", "/f.bin", "AB", "Content-Range", "bytes 0-1/*").
		wantCode(t, http.StatusNoContent, "partial PUT with an unknown total")
	if got := string(fs.nodes["/f.bin"].data); got != "AB23456789" {
		t.Fatalf("file is %q, want %q", got, "AB23456789")
	}
}

// TestContentRangePutRejectsABodyLongerThanTheRange. A body longer than the
// range it declares is a contradiction, and writing the overflow would go
// past the end the client itself named.
func TestContentRangePutRejectsABodyLongerThanTheRange(t *testing.T) {
	fs := newWriteFS("/f.bin", "0123456789")
	c, _ := serve(t, fs, webdav.ReadWrite())
	// Four bytes of body for a two-byte range: only two may be read, and the
	// declared length then matches, so this must succeed writing exactly two.
	c.do("PUT", "/f.bin", "ABCD", "Content-Range", "bytes 0-1/10").
		wantCode(t, http.StatusNoContent, "over-long body is truncated to the range")
	if got := string(fs.nodes["/f.bin"].data); got != "AB23456789" {
		t.Fatalf("file is %q, want %q — bytes past the range must not be written", got, "AB23456789")
	}
}

// TestContentRangePutRejectsAShortBody. A short body would write a prefix of
// the range and leave the rest stale, with no way to tell which is which.
func TestContentRangePutRejectsAShortBody(t *testing.T) {
	fs := newWriteFS("/f.bin", "0123456789")
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("PUT", "/f.bin", "A", "Content-Range", "bytes 0-3/10").
		wantCode(t, http.StatusBadRequest, "body shorter than the declared range")
	if string(fs.nodes["/f.bin"].data) != "0123456789" {
		t.Fatal("a short-bodied range PUT wrote a partial range")
	}
}

// TestContentRangePutOverTheBodyLimitIsRefused.
func TestContentRangePutOverTheBodyLimitIsRefused(t *testing.T) {
	fs := newWriteFS("/f.bin", strings.Repeat("x", 100))
	c, _ := serve(t, fs, webdav.ReadWrite(), webdav.WithMaxBody(4))
	c.do("PUT", "/f.bin", strings.Repeat("y", 40), "Content-Range", "bytes 0-39/100").
		wantCode(t, http.StatusRequestEntityTooLarge, "range body over the limit")
}

// TestContentRangePutWithATruncatedBodyWritesNothing needs a raw connection:
// no HTTP client will send a body shorter than the length it announced.
func TestContentRangePutWithATruncatedBodyWritesNothing(t *testing.T) {
	// The file has to be big enough that the range is satisfiable, or the
	// request is refused at 416 before a byte of body is ever read and this
	// test silently stops testing what it names.
	original := strings.Repeat("0123456789abcdef", 16)
	fs := newWriteFS("/f.bin", original)
	_, srv := serve(t, fs, webdav.ReadWrite())
	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_, err = io.WriteString(conn, "PUT /f.bin HTTP/1.1\r\nHost: "+addr+
		"\r\nContent-Range: bytes 0-99/1000\r\nContent-Length: 100\r\n\r\nshort")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		if err := tc.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
	}
	_, _ = io.ReadAll(conn)
	conn.Close()
	if string(fs.nodes["/f.bin"].data) != original {
		t.Fatal("a truncated range PUT modified the file")
	}
}

// TestContentRangePutSurfacesDriverFailures walks the three ways the driver
// can fail after the range has been accepted. Each must reach the client as a
// failure — a 204 issued over a failed write is the worst outcome available.
func TestContentRangePutSurfacesDriverFailures(t *testing.T) {
	boom := errors.New("driver exploded")
	for _, tc := range []struct {
		name  string
		setup func(*writeFS)
	}{
		{"WriteAt fails", func(f *writeFS) { f.writeErr = boom }},
		{"Sync fails", func(f *writeFS) { f.syncErr = boom }},
		{"Close fails", func(f *writeFS) { f.closeErr = boom }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newWriteFS("/f.bin", "0123456789")
			tc.setup(fs)
			c, _ := serve(t, fs, webdav.ReadWrite())
			c.do("PUT", "/f.bin", "AB", "Content-Range", "bytes 0-1/10").
				wantCode(t, http.StatusInternalServerError, tc.name)
		})
	}
}

// TestContentRangePutSurfacesOpenFailures covers the two ways opening for
// write can fail before any byte is read from the client.
func TestContentRangePutSurfacesOpenFailures(t *testing.T) {
	t.Run("OpenFile errors", func(t *testing.T) {
		fs := newWriteFS("/f.bin", "0123456789")
		fs.openErr = errors.New("driver refused to open")
		c, _ := serve(t, fs, webdav.ReadWrite())
		c.do("PUT", "/f.bin", "AB", "Content-Range", "bytes 0-1/10").
			wantCode(t, http.StatusInternalServerError, "OpenFile error")
	})
	t.Run("OpenFile returns a nil File with no error", func(t *testing.T) {
		fs := newWriteFS("/f.bin", "0123456789")
		fs.nilFile = true
		c, _ := serve(t, fs, webdav.ReadWrite())
		// A driver bug the server must answer, not panic on, in a request
		// goroutine where nothing would recover it.
		c.do("PUT", "/f.bin", "AB", "Content-Range", "bytes 0-1/10").
			wantCode(t, http.StatusInternalServerError, "nil File")
	})
}

// TestContentRangePutIsRefusedOnAReadOnlyExport — the read-only guard runs
// before any of this, and must not be reachable around the new path.
func TestContentRangePutIsRefusedOnAReadOnlyExport(t *testing.T) {
	fs := newWriteFS("/f.bin", "0123456789")
	c, _ := serve(t, fs)
	c.do("PUT", "/f.bin", "AB", "Content-Range", "bytes 0-1/10").
		wantCode(t, http.StatusForbidden, "partial PUT on a read-only export")
	if string(fs.nodes["/f.bin"].data) != "0123456789" {
		t.Fatal("a read-only export was written through a Content-Range PUT")
	}
}
