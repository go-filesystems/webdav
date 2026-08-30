package demo

// In-package tests for the two paths Main cannot reach from outside: the
// serving loop, which only ends when its listener dies, and the handler
// construction that fails only when the system CSPRNG does.

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	fat32 "github.com/go-filesystems/fat32"
	filesystem "github.com/go-filesystems/interface"
	"github.com/go-filesystems/webdav"
)

// image formats a small FAT32 image with one known file and returns its path.
func image(t *testing.T) (string, []byte) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "d.img")
	fs, err := fat32.Format(p, 32<<20, fat32.FormatConfig{Label: "INJ"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	body := []byte("served by Main itself\n")
	if err := fs.WriteFile("/M.TXT", body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return p, body
}

func TestSetupReleasesTheDriverWhenTheHandlerCannotBeBuilt(t *testing.T) {
	// An error path that has never been executed is an error path that has
	// never been shown to clean up after itself, and this one must close the
	// driver it already opened.
	orig := newHandler
	t.Cleanup(func() { newHandler = orig })
	boom := errors.New("no handler")
	var closed bool
	newHandler = func(fsys filesystem.Filesystem, _ ...webdav.Option) (*webdav.Handler, error) {
		_ = fsys.Close()
		closed = true
		return nil, boom
	}
	p, _ := image(t)
	var out bytes.Buffer
	if _, _, _, err := Setup(p, "127.0.0.1:0", false, &out); !errors.Is(err, boom) {
		t.Fatalf("Setup = %v, want %v", err, boom)
	}
	if !closed {
		t.Fatal("the driver was not released")
	}
}

func TestMainServesAndReturnsWhenItsServerEnds(t *testing.T) {
	p, want := image(t)
	orig := serveHTTP
	t.Cleanup(func() { serveHTTP = orig })

	ready := make(chan string, 1)
	release := make(chan struct{})
	serveHTTP = func(srv *http.Server, ln net.Listener) error {
		go func() { _ = srv.Serve(ln) }()
		ready <- ln.Addr().String()
		<-release
		return srv.Close()
	}

	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- Main([]string{"-image", p, "-addr", "127.0.0.1:0"}, &out, &errOut) }()

	var addr string
	select {
	case addr = <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("Main never started serving")
	}
	// A real request, through the whole program, on the server Main built.
	resp, err := http.Get("http://" + addr + "/M.TXT")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("Main served %q, want %q", body, want)
	}
	close(release)
	select {
	case got := <-done:
		if got != 1 {
			t.Fatalf("Main exit %d, want 1", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Main did not return after its server ended")
	}
}
