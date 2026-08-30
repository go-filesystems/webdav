package webdav_test

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-filesystems/webdav"
)

// Every test drives the handler through a real HTTP server rather than by
// calling ServeHTTP with a synthetic ResponseWriter. The difference is not
// cosmetic: a real client applies chunked encoding, closes connections, sends
// its own headers, and — for the range tests — is the thing that has to agree
// with the Content-Range the handler wrote. An in-process call cannot catch a
// header written after WriteHeader, and that is exactly the class of defect
// that makes a real mount fail.

// serve starts a test server for fsys and returns a client bound to it.
func serve(t *testing.T, fsys filesystem.Filesystem, opts ...webdav.Option) (*dav, *httptest.Server) {
	t.Helper()
	h, err := webdav.New(fsys, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &dav{t: t, base: srv.URL, c: srv.Client()}, srv
}

// serveTLS is the same over TLS, which is the only way to exercise the
// credential paths that refuse a cleartext connection.
func serveTLS(t *testing.T, fsys filesystem.Filesystem, opts ...webdav.Option) *dav {
	t.Helper()
	h, err := webdav.New(fsys, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return &dav{t: t, base: srv.URL, c: srv.Client()}
}

// dav is a minimal WebDAV client: enough to send any method with any headers
// and read the whole response back.
type dav struct {
	t    *testing.T
	base string
	c    *http.Client
}

type result struct {
	code   int
	body   string
	header http.Header
}

// do sends one request. headers are given as alternating name/value pairs,
// which keeps a call site to one line.
func (d *dav) do(method, path, body string, headers ...string) result {
	d.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, d.base+path, r)
	if err != nil {
		d.t.Fatalf("%s %s: %v", method, path, err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := d.c.Do(req)
	if err != nil {
		d.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		d.t.Fatalf("%s %s: reading body: %v", method, path, err)
	}
	return result{code: resp.StatusCode, body: string(out), header: resp.Header}
}

// wantCode fails unless the response carried the expected status.
func (r result) wantCode(t *testing.T, want int, what string) result {
	t.Helper()
	if r.code != want {
		t.Fatalf("%s: status %d, want %d; body %q", what, r.code, want, truncate(r.body))
	}
	return r
}

// wantContains fails unless the body carries a fragment.
func (r result) wantContains(t *testing.T, frag, what string) result {
	t.Helper()
	if !strings.Contains(r.body, frag) {
		t.Fatalf("%s: body does not contain %q; got %q", what, frag, truncate(r.body))
	}
	return r
}

func (r result) wantLacks(t *testing.T, frag, what string) result {
	t.Helper()
	if strings.Contains(r.body, frag) {
		t.Fatalf("%s: body unexpectedly contains %q", what, frag)
	}
	return r
}

func truncate(s string) string {
	if len(s) > 600 {
		return s[:600] + "..."
	}
	return s
}

// secret returns a fresh random credential.
//
// No test in this module contains a literal password, token or key. That is
// not decoration: a credential written into a repository is a credential
// forever, and a test fixture is the most common place one gets there. Each
// value here exists for the lifetime of one test process and is never
// written anywhere.
func secret(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// propfindAll is the body a client sends to ask for every property.
const propfindAll = `<?xml version="1.0" encoding="utf-8"?>` +
	`<D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`
