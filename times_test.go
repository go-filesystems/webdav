package webdav_test

import (
	"errors"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// timesFS is the memory filesystem plus filesystem.TimesReader.
type timesFS struct {
	*memFS
	ft  filesystem.FileTimes
	err error
}

func (f *timesFS) Times(string) (filesystem.FileTimes, error) { return f.ft, f.err }

const propfindNamed = `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:prop>` +
	`<D:getlastmodified/><D:creationdate/></D:prop></D:propfind>`

func TestTimesReaderFeedsMtimeAndBirthTime(t *testing.T) {
	m := filesystem.NewTimestamp(time.Date(2001, 9, 9, 1, 46, 40, 0, time.UTC), 2*time.Second)
	b := filesystem.NewTimestamp(time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC), 10*time.Millisecond)
	c, _ := serve(t, &timesFS{memFS: newMemFS().file("/a.txt", "hello"), ft: filesystem.FileTimes{Mtime: m, Btime: b}})
	c.do("PROPFIND", "/a.txt", propfindAll, "Depth", "0").
		wantContains(t, "Sun, 09 Sep 2001 01:46:40 GMT", "getlastmodified is the mtime").
		wantContains(t, "1999-12-31T23:59:59Z", "creationdate is the birth time, not the mtime").
		wantLacks(t, "2001-09-09T01:46:40Z", "creationdate must not be copied from the mtime")
	r := c.do("GET", "/a.txt", "")
	if got := r.header.Get("Last-Modified"); got != "Sun, 09 Sep 2001 01:46:40 GMT" {
		t.Errorf("Last-Modified = %q", got)
	}
}

// A time the filesystem does not record is not listed, and a PROPFIND that asks
// for it gets 404 for that property: RFC 4918 section 9.1. A GET sends no
// Last-Modified rather than one from 1970 or from now.
func TestUnrecordedTimesAreAbsentNotInvented(t *testing.T) {
	c, _ := serve(t, &timesFS{memFS: newMemFS().file("/a.txt", "hello")})
	c.do("PROPFIND", "/a.txt", propfindAll, "Depth", "0").
		wantCode(t, 207, "allprop").
		wantLacks(t, "getlastmodified", "no mtime recorded").
		wantLacks(t, "creationdate", "no birth time recorded").
		wantContains(t, "getetag", "the rest is still there")
	c.do("PROPFIND", "/a.txt", propfindNamed, "Depth", "0").
		wantContains(t, "HTTP/1.1 404 Not Found", "the missing properties are reported by name").
		wantContains(t, "getlastmodified", "named in the 404 propstat")
	if got := c.do("GET", "/a.txt", "").header.Get("Last-Modified"); got != "" {
		t.Errorf("Last-Modified = %q, want none", got)
	}
}

func TestMtimeWithoutBirthTime(t *testing.T) {
	m := filesystem.NewTimestamp(time.Date(2001, 9, 9, 1, 46, 40, 0, time.UTC), 2*time.Second)
	c, _ := serve(t, &timesFS{memFS: newMemFS().file("/a.txt", "x"), ft: filesystem.FileTimes{Mtime: m}})
	c.do("PROPFIND", "/a.txt", propfindAll, "Depth", "0").
		wantContains(t, "Sun, 09 Sep 2001 01:46:40 GMT", "getlastmodified").
		wantLacks(t, "creationdate", "FAT-like: no birth time, so no creationdate")
}

// A Times call that fails is the old behaviour: the Handler's start time.
func TestTimesErrorFallsBackToTheStartTime(t *testing.T) {
	c, _ := serve(t, &timesFS{memFS: newMemFS().file("/a.txt", "x"), err: errors.New("boom")})
	c.do("PROPFIND", "/a.txt", propfindAll, "Depth", "0").
		wantContains(t, "getlastmodified", "the fallback still answers").
		wantContains(t, "creationdate", "and so does creationdate")
}
