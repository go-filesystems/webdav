// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package webdav_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// hostFS is a memFS whose files open as filesystem.HostFile over a real file
// of the same bytes, counting what is read through Read and ReadAt -- what a
// copy through this process costs. What sendfile(2) sends passes through
// neither.
type hostFS struct {
	*memFS
	path    string
	readVia atomic.Int64
}

func (h *hostFS) OpenFile(string) (filesystem.File, error) {
	f, err := os.Open(h.path)
	if err != nil {
		return nil, err
	}
	return &hostFile{File: f, read: &h.readVia}, nil
}

type hostFile struct {
	*os.File
	read *atomic.Int64
}

func (f *hostFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	f.read.Add(int64(n))
	return n, err
}

func (f *hostFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.File.ReadAt(p, off)
	f.read.Add(int64(n))
	return n, err
}

func (f *hostFile) Size() int64 {
	st, _ := f.Stat()
	return st.Size()
}

func (*hostFile) HostFile() {}

func newHostFS(t *testing.T, data []byte) *hostFS {
	t.Helper()
	path := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	m := newMemFS()
	if err := m.WriteFile("/big.bin", data, 0o644); err != nil {
		t.Fatal(err)
	}
	return &hostFS{memFS: m, path: path}
}

// A GET of a HostFile over plain HTTP goes out through the connection's
// ReadFrom: net/http copies the first 512 bytes itself, to sniff a type,
// and the kernel sends the rest. Over TLS there is no such ReadFrom, and
// every byte is copied, as before.
func TestAHostFileIsSentWithoutACopy(t *testing.T) {
	data := make([]byte, 4<<20+3)
	for i := range data {
		data[i] = byte(i * 13)
	}
	sendfile := runtime.GOOS == "linux" || runtime.GOOS == "darwin" ||
		runtime.GOOS == "freebsd" || runtime.GOOS == "windows"

	fs := newHostFS(t, data)
	d, _ := serve(t, fs)
	r := d.do("GET", "/big.bin", "").wantCode(t, 200, "GET")
	if !bytes.Equal([]byte(r.body), data) {
		t.Fatalf("GET returned %d bytes, not the file", len(r.body))
	}
	if via := fs.readVia.Load(); sendfile && via > 512 {
		t.Errorf("%d of %d bytes were copied through Read or ReadAt; want at most the 512 sniffed", via, len(data))
	}

	fs.readVia.Store(0)
	r = d.do("GET", "/big.bin", "", "Range", "bytes=1000-1999999").wantCode(t, 206, "GET range")
	if !bytes.Equal([]byte(r.body), data[1000:2000000]) {
		t.Fatalf("GET range returned %d bytes, not the range", len(r.body))
	}
	if via := fs.readVia.Load(); sendfile && via > 512 {
		t.Errorf("range: %d bytes were copied through Read or ReadAt; want at most 512", via)
	}

	tfs := newHostFS(t, data)
	dt := serveTLS(t, tfs)
	r = dt.do("GET", "/big.bin", "").wantCode(t, 200, "GET over TLS")
	if !bytes.Equal([]byte(r.body), data) {
		t.Fatalf("GET over TLS returned %d bytes, not the file", len(r.body))
	}
	if via := tfs.readVia.Load(); via != int64(len(data)) {
		t.Errorf("over TLS %d bytes went through Read or ReadAt; want all %d", via, len(data))
	}
}

var _ io.ReadSeeker = (*hostFile)(nil)
