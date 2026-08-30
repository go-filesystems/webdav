package webdav_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-filesystems/webdav"
)

// TestBrowserGetReturnsTheBytes is the property the whole module has to keep
// true: a plain GET on a file is its bytes and nothing else. No XML, no
// wrapper, no redirect — a browser and curl are clients without mounting.
func TestBrowserGetReturnsTheBytes(t *testing.T) {
	body := "the quick brown fox\n"
	c, _ := serve(t, newMemFS().file("/a.txt", body))
	r := c.do("GET", "/a.txt", "").wantCode(t, http.StatusOK, "GET")
	if r.body != body {
		t.Fatalf("body = %q, want %q", r.body, body)
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if r.header.Get("ETag") == "" {
		t.Fatal("GET must carry an ETag")
	}
	if r.header.Get("Accept-Ranges") != "bytes" {
		t.Fatal("GET must advertise Accept-Ranges")
	}
}

func TestHeadHasNoBody(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "hello"))
	r := c.do("HEAD", "/a.txt", "").wantCode(t, http.StatusOK, "HEAD")
	if r.body != "" {
		t.Fatalf("HEAD body = %q, want empty", r.body)
	}
	if r.header.Get("Content-Length") != "5" {
		t.Fatalf("Content-Length = %q, want 5", r.header.Get("Content-Length"))
	}
}

func TestGetMissingFileIs404(t *testing.T) {
	c, _ := serve(t, newMemFS())
	c.do("GET", "/nope.txt", "").wantCode(t, http.StatusNotFound, "GET missing")
}

// TestRangeReturnsTheRightSlice checks the arithmetic against the source
// bytes, not against itself: a range server that is consistently off by one
// passes every self-comparison.
func TestRangeReturnsTheRightSlice(t *testing.T) {
	raw := make([]byte, 4096)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	body := hex.EncodeToString(raw) // 8192 printable bytes

	// Both driver shapes are exercised: one that can answer a byte range
	// natively, and one that cannot and is read through ReadFile. The client
	// must not be able to tell them apart.
	withOpener := &openFS{memFS: newMemFS()}
	withOpener.file("/big.bin", body)
	plain := newMemFS().file("/big.bin", body)

	for name, fsys := range map[string]filesystem.Filesystem{
		"with Opener":    withOpener,
		"without Opener": plain,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := serve(t, fsys)
			for _, tc := range []struct {
				hdr        string
				start, end int
			}{
				{"bytes=0-99", 0, 100},
				{"bytes=100-199", 100, 200},
				{"bytes=8000-", 8000, 8192},
				{"bytes=-100", 8092, 8192},
			} {
				r := c.do("GET", "/big.bin", "", "Range", tc.hdr).
					wantCode(t, http.StatusPartialContent, "range "+tc.hdr)
				want := body[tc.start:tc.end]
				if r.body != want {
					t.Fatalf("range %s: got %d bytes %q..., want %d bytes %q...",
						tc.hdr, len(r.body), first(r.body), len(want), first(want))
				}
				wantCR := fmt.Sprintf("bytes %d-%d/%d", tc.start, tc.end-1, len(body))
				if got := r.header.Get("Content-Range"); got != wantCR {
					t.Fatalf("range %s: Content-Range %q, want %q", tc.hdr, got, wantCR)
				}
			}
			// The whole file still comes back whole, checked by digest
			// against the source rather than against another response.
			full := c.do("GET", "/big.bin", "").wantCode(t, http.StatusOK, "full GET")
			if sha256.Sum256([]byte(full.body)) != sha256.Sum256([]byte(body)) {
				t.Fatal("full GET does not match the source bytes")
			}
			// A range past the end is 416 carrying the total size, which is
			// how a resuming client discovers the file shrank.
			r := c.do("GET", "/big.bin", "", "Range", "bytes=99999-").
				wantCode(t, http.StatusRequestedRangeNotSatisfiable, "range past EOF")
			if got := r.header.Get("Content-Range"); got != "bytes */8192" {
				t.Fatalf("Content-Range = %q, want bytes */8192", got)
			}
		})
	}
}

func first(s string) string {
	if len(s) > 24 {
		return s[:24]
	}
	return s
}

// TestRangeWithOpenerDoesNotReadTheWholeFile is the measurement behind the
// Opener capability: a 100-byte range must cost about 100 bytes of driver
// read, not the file.
func TestRangeWithOpenerDoesNotReadTheWholeFile(t *testing.T) {
	fs := &openFS{memFS: newMemFS()}
	fs.file("/big.bin", strings.Repeat("0123456789abcdef", 4096)) // 64 KiB
	c, _ := serve(t, fs)
	c.do("GET", "/big.bin", "", "Range", "bytes=1000-1099").
		wantCode(t, http.StatusPartialContent, "range")
	if fs.bytes > 4096 {
		t.Fatalf("a 100-byte range read %d bytes from the driver; Opener is not being used", fs.bytes)
	}
}

func TestOpenerFailuresAreReported(t *testing.T) {
	t.Run("OpenFile error", func(t *testing.T) {
		fs := &openFS{memFS: newMemFS(), openErr: errors.New("device is not ready")}
		fs.file("/a.txt", "x")
		c, _ := serve(t, fs)
		c.do("GET", "/a.txt", "").wantCode(t, http.StatusInternalServerError, "OpenFile error")
	})
	t.Run("nil File with no error", func(t *testing.T) {
		fs := &openFS{memFS: newMemFS(), nilFile: true}
		fs.file("/a.txt", "x")
		c, _ := serve(t, fs)
		c.do("GET", "/a.txt", "").wantCode(t, http.StatusInternalServerError, "nil File")
	})
	t.Run("ReadFile error without Opener", func(t *testing.T) {
		fs := newMemFS().file("/a.txt", "x")
		fs.failWith("ReadFile:/a.txt", errors.New("media error"))
		c, _ := serve(t, fs)
		c.do("GET", "/a.txt", "").wantCode(t, http.StatusInternalServerError, "ReadFile error")
	})
}

func TestConditionalGet(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "hello"))
	etag := c.do("HEAD", "/a.txt", "").header.Get("ETag")
	c.do("GET", "/a.txt", "", "If-None-Match", etag).
		wantCode(t, http.StatusNotModified, "matching If-None-Match")
}

func TestGetOnACollectionListsIt(t *testing.T) {
	fs := newMemFS().dir("/sub").file("/sub/a.txt", "hello").file("/z.txt", "z")
	c, _ := serve(t, fs)
	r := c.do("GET", "/", "").wantCode(t, http.StatusOK, "index")
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q", ct)
	}
	r.wantContains(t, `href="/sub/"`, "collection link ends in a slash").
		wantContains(t, `href="/z.txt"`, "file link").
		wantLacks(t, `href="/."`, "the on-disk . entry must not be listed")
	// A subdirectory listing offers a way back up.
	c.do("GET", "/sub", "").wantCode(t, http.StatusOK, "sub index").
		wantContains(t, "../", "parent link")
	// HEAD on a collection is the same headers with no body.
	h := c.do("HEAD", "/", "").wantCode(t, http.StatusOK, "HEAD index")
	if h.body != "" {
		t.Fatalf("HEAD index body = %q", h.body)
	}
	if h.header.Get("Content-Length") == "0" {
		t.Fatal("HEAD on a collection must report the listing's length")
	}
}

// TestIndexEscapesNames proves a file name inside someone's disk image cannot
// become markup in the listing.
func TestIndexEscapesNames(t *testing.T) {
	fs := newMemFS().file(`/a<script>&"'.txt`, "x")
	c, _ := serve(t, fs)
	r := c.do("GET", "/", "").wantCode(t, http.StatusOK, "index")
	r.wantLacks(t, "<script>", "raw markup from a file name").
		wantContains(t, "&lt;script&gt;", "escaped name").
		wantContains(t, "&amp;", "escaped ampersand").
		wantContains(t, "&#34;", "escaped quote").
		wantContains(t, "&#39;", "escaped apostrophe")
}

func TestIndexSurvivesAnUnreadableMember(t *testing.T) {
	fs := newMemFS().file("/good.txt", "x").file("/bad.txt", "y")
	fs.failWith("Stat:/bad.txt", errors.New("media error"))
	c, _ := serve(t, fs)
	// A file that cannot be described is still listed: dropping it would
	// make the listing lie about what the image holds.
	c.do("GET", "/", "").wantCode(t, http.StatusOK, "index").
		wantContains(t, "bad.txt", "unreadable member is still listed").
		wantContains(t, "good.txt", "readable member")
}

func TestIndexReportsAFailedListing(t *testing.T) {
	fs := newMemFS()
	fs.failWith("ListDir:/", errors.New("media error"))
	c, _ := serve(t, fs)
	c.do("GET", "/", "").wantCode(t, http.StatusInternalServerError, "failed listing")
}

func TestBinaryFileRoundTripsByteForByte(t *testing.T) {
	raw := make([]byte, 65536)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	fs := newMemFS()
	fs.add("/blob.bin", 0o100644, raw)
	c, _ := serve(t, fs)
	r := c.do("GET", "/blob.bin", "").wantCode(t, http.StatusOK, "binary GET")
	if !bytes.Equal([]byte(r.body), raw) {
		t.Fatal("binary body did not survive the round trip")
	}
	if ct := r.header.Get("Content-Type"); ct == "" {
		t.Fatal("a body with no extension must still get a content type")
	}
	_ = webdav.StatusMulti
}
