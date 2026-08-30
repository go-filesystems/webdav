package demo_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fat32 "github.com/go-filesystems/fat32"
	"github.com/go-filesystems/webdav/fat32demo/demo"
)

// makeImage formats a genuine FAT32 image and puts known content in it.
func makeImage(t *testing.T) (path string, content []byte) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "disk.img")
	fs, err := fat32.Format(path, 64<<20, fat32.FormatConfig{Label: "DAVDEMO"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	content = bytes.Repeat([]byte("go-filesystems/webdav 0123456789abcdef\n"), 512)
	if err := fs.WriteFile("/HELLO.TXT", content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.MkDir("/SUB", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path, content
}

// start serves an image and returns its base URL. Nothing is left behind: the
// listener is closed and the driver released when the test ends.
func start(t *testing.T, path string, rw bool) (string, io.Writer) {
	t.Helper()
	var out bytes.Buffer
	h, fsys, ln, err := demo.Setup(path, "127.0.0.1:0", rw, &out)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = fsys.Close()
	})
	return "http://" + ln.Addr().String(), &out
}

// TestRealImageBytesMatchTheDriver is the end-to-end claim: what an HTTP
// client gets is byte for byte what the driver returns directly, out of a
// genuine on-disk FAT32 image.
//
// The digest is taken twice by two paths that share no code past the driver
// — once over HTTP, once through ReadFile — because two numbers produced by
// the same path always agree.
func TestRealImageBytesMatchTheDriver(t *testing.T) {
	path, want := makeImage(t)
	base, out := start(t, path, false)
	if !strings.Contains(out.(*bytes.Buffer).String(), "curl") {
		t.Fatalf("Setup printed no client instructions:\n%s", out)
	}

	body := get(t, base+"/HELLO.TXT", nil)
	if !bytes.Equal(body, want) {
		t.Fatalf("HTTP body is %d bytes, want %d", len(body), len(want))
	}
	viaHTTP := digest(body)

	fsys, err := fat32.Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	viaDriver, err := demo.Digest(fsys, "/HELLO.TXT")
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if viaHTTP != viaDriver {
		t.Fatalf("over HTTP %s, through the driver %s", viaHTTP, viaDriver)
	}
	if viaHTTP != digest(want) {
		t.Fatalf("neither matches the bytes that were written")
	}
}

// TestRangeOnARealImageReturnsTheRightSlice checks the arithmetic against the
// source, not against another response.
func TestRangeOnARealImageReturnsTheRightSlice(t *testing.T) {
	path, want := makeImage(t)
	base, _ := start(t, path, false)
	for _, tc := range []struct {
		hdr    string
		lo, hi int
	}{
		{"bytes=0-63", 0, 64},
		{"bytes=1000-1099", 1000, 1100},
		{"bytes=-128", len(want) - 128, len(want)},
	} {
		got := get(t, base+"/HELLO.TXT", map[string]string{"Range": tc.hdr})
		if !bytes.Equal(got, want[tc.lo:tc.hi]) {
			t.Fatalf("range %s: got %q, want %q", tc.hdr, got, want[tc.lo:tc.hi])
		}
	}
}

// TestPutOnARealImageIsOneWholeFileWrite exercises the write path that
// WebDAV gets right where NFS cannot: PUT carries the whole body, so it is
// exactly WriteFile.
func TestPutOnARealImageIsOneWholeFileWrite(t *testing.T) {
	path, _ := makeImage(t)
	base, _ := start(t, path, true)
	payload := bytes.Repeat([]byte("written over webdav\n"), 20000) // ~390 KiB

	req, err := http.NewRequest(http.MethodPut, base+"/SUB/UP.BIN", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT status %d, want 201", resp.StatusCode)
	}
	// The bytes must be in the image, judged by the driver rather than by
	// reading them back through the same server that wrote them.
	fsys, err := fat32.Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := demo.Digest(fsys, "/SUB/UP.BIN")
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got != digest(payload) {
		t.Fatalf("the image holds %s, the client sent %s", got, digest(payload))
	}
}

func TestReadOnlyByDefault(t *testing.T) {
	path, _ := makeImage(t)
	base, _ := start(t, path, false)
	req, err := http.NewRequest(http.MethodPut, base+"/NOPE.TXT", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT on a read-only export: %d, want 403", resp.StatusCode)
	}
}

func TestSetupRejectsABadImage(t *testing.T) {
	var out bytes.Buffer
	if _, _, _, err := demo.Setup(filepath.Join(t.TempDir(), "absent.img"), "127.0.0.1:0", false, &out); err == nil {
		t.Fatal("Setup must fail on a missing image")
	}
	junk := filepath.Join(t.TempDir(), "junk.img")
	if err := os.WriteFile(junk, make([]byte, 4096), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, _, err := demo.Setup(junk, "127.0.0.1:0", false, &out); err == nil {
		t.Fatal("Setup must fail on something that is not FAT32")
	}
}

func TestSetupRejectsAnUnavailableAddress(t *testing.T) {
	path, _ := makeImage(t)
	var out bytes.Buffer
	// Port 1 needs root, so the listen fails and Setup must release the
	// driver it already opened rather than leak it.
	if _, _, _, err := demo.Setup(path, "127.0.0.1:1", false, &out); err == nil {
		t.Fatal("Setup must fail on an address it cannot bind")
	}
}

func TestDigestReportsAMissingPath(t *testing.T) {
	path, _ := makeImage(t)
	fsys, err := fat32.Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()
	if _, err := demo.Digest(fsys, "/NOPE.TXT"); err == nil {
		t.Fatal("Digest must fail on a missing path")
	}
}

func TestMain_(t *testing.T) {
	path, want := makeImage(t)
	var out, errOut bytes.Buffer

	if got := demo.Main([]string{"-nosuchflag"}, &out, &errOut); got != 2 {
		t.Fatalf("bad flag: exit %d, want 2", got)
	}
	if got := demo.Main([]string{"-image", "/nonexistent"}, &out, &errOut); got != 1 {
		t.Fatalf("missing image: exit %d, want 1", got)
	}

	out.Reset()
	if got := demo.Main([]string{"-image", path, "-addr", "127.0.0.1:0", "-digest", "/HELLO.TXT"}, &out, &errOut); got != 0 {
		t.Fatalf("digest: exit %d, want 0", got)
	}
	// Digest mode prints the digest and nothing else, so a shell can compare
	// it without parsing anything out of the way.
	if strings.TrimSpace(out.String()) != digest(want) {
		t.Fatalf("digest printed %q, want %q", strings.TrimSpace(out.String()), digest(want))
	}
	if got := demo.Main([]string{"-image", path, "-addr", "127.0.0.1:0", "-digest", "/NOPE"}, &out, &errOut); got != 1 {
		t.Fatalf("digest of a missing path: exit %d, want 1", got)
	}

}

func waitFor(t *testing.T, addr string) {
	t.Helper()
	for range 100 {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s", addr)
}

func get(t *testing.T, url string, headers map[string]string) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	return body
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
