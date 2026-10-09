// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package webdav_test

import (
	"bytes"
	"errors"
	"net/http"
	"testing"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-filesystems/webdav"
)

// copyFS is a writeFS whose OpenFile can fail, or return nil, for one path,
// whose Files can be altered, and which counts whole-file reads: a COPY
// between files the driver can open must not read a whole file.
type copyFS struct {
	*writeFS
	failOpen  map[string]error
	nilOpen   map[string]bool
	alter     func(path string, f filesystem.File) filesystem.File
	wholeRead int
}

func (c *copyFS) ReadFile(path string) ([]byte, error) {
	c.wholeRead++
	return c.writeFS.ReadFile(path)
}

func (c *copyFS) OpenFile(path string) (filesystem.File, error) {
	if err := c.failOpen[path]; err != nil {
		return nil, err
	}
	if c.nilOpen[path] {
		return nil, nil
	}
	f, err := c.writeFS.OpenFile(path)
	if err == nil && c.alter != nil {
		f = c.alter(path, f)
	}
	return f, err
}

// liarFile says it is longer than it is.
type liarFile struct{ filesystem.File }

func (l liarFile) Size() int64 { return l.File.Size() + 10 }

// stuckFile cannot be resized.
type stuckFile struct{ filesystem.WritableFile }

func (stuckFile) Truncate(int64) error { return errors.New("cannot resize") }

func newCopyFS(data []byte) *copyFS {
	return &copyFS{writeFS: newWriteFS("/a.bin", string(data))}
}

// A COPY goes from the opened source into the opened destination, without
// reading the whole file, and the destination is synced.
func TestCopyGoesFileToFile(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 3<<20/16+7)
	fs := newCopyFS(data)
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("COPY", "/a.bin", "", "Destination", "/b.bin").wantCode(t, http.StatusCreated, "COPY")
	if fs.wholeRead != 0 {
		t.Fatalf("COPY read %d whole file(s)", fs.wholeRead)
	}
	got := c.do("GET", "/b.bin", "")
	if !bytes.Equal([]byte(got.body), data) {
		t.Fatalf("the copy has %d bytes, not the %d of the source", len(got.body), len(data))
	}
	if fs.synced == 0 {
		t.Fatal("the copy was not synced")
	}
	// Over a destination that already exists, and longer: replaced, not merged.
	fs.writeFS.memFS.file("/long.bin", string(bytes.Repeat([]byte("z"), len(data)+100)))
	c.do("COPY", "/a.bin", "", "Destination", "/long.bin").wantCode(t, http.StatusNoContent, "COPY over")
	if got := c.do("GET", "/long.bin", ""); !bytes.Equal([]byte(got.body), data) {
		t.Fatalf("overwritten copy has %d bytes, want %d", len(got.body), len(data))
	}
}

// A driver that opens files but cannot write them in place is copied the
// whole-file way, as before.
func TestCopyWithoutInPlaceWrites(t *testing.T) {
	fs := newCopyFS([]byte("hello"))
	fs.plain = true
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("COPY", "/a.bin", "", "Destination", "/b.bin").wantCode(t, http.StatusCreated, "COPY")
	c.do("GET", "/b.bin", "").wantContains(t, "hello", "copied body")
	if fs.wholeRead == 0 {
		t.Fatal("expected the whole-file fallback")
	}
	fs.failWith("ReadFile:/a.bin", errors.New("media error"))
	c.do("COPY", "/a.bin", "", "Destination", "/c.bin").wantCode(t, webdav.StatusMulti, "unreadable, whole-file")
}

// Every way the file-to-file copy can fail is reported for the destination.
func TestCopyFileFailures(t *testing.T) {
	boom := errors.New("media error")
	for _, tc := range []struct {
		name string
		set  func(*copyFS)
	}{
		{"the source does not open", func(f *copyFS) { f.failOpen = map[string]error{"/a.bin": boom} }},
		{"the source opens as nil", func(f *copyFS) { f.nilOpen = map[string]bool{"/a.bin": true} }},
		{"the destination cannot be created", func(f *copyFS) { f.failWith("WriteFile:/b.bin", boom) }},
		{"the destination does not open", func(f *copyFS) { f.failOpen = map[string]error{"/b.bin": boom} }},
		{"the destination opens as nil", func(f *copyFS) { f.nilOpen = map[string]bool{"/b.bin": true} }},
		{"the destination cannot be resized", func(f *copyFS) {
			f.alter = func(p string, file filesystem.File) filesystem.File {
				if p == "/b.bin" {
					return stuckFile{file.(filesystem.WritableFile)}
				}
				return file
			}
		}},
		{"the source is shorter than it says", func(f *copyFS) {
			f.alter = func(p string, file filesystem.File) filesystem.File {
				if p == "/a.bin" {
					return liarFile{file}
				}
				return file
			}
		}},
		{"a write fails", func(f *copyFS) { f.writeErr = boom }},
		{"the sync fails", func(f *copyFS) { f.syncErr = boom }},
		{"the close fails", func(f *copyFS) { f.closeErr = boom }},
	} {
		fs := newCopyFS([]byte("some bytes"))
		tc.set(fs)
		c, _ := serve(t, fs, webdav.ReadWrite())
		c.do("COPY", "/a.bin", "", "Destination", "/b.bin").wantCode(t, webdav.StatusMulti, tc.name)
	}
}
