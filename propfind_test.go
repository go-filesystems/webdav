package webdav_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/go-filesystems/webdav"
)

func TestPropfindDepthZero(t *testing.T) {
	c, _ := serve(t, newMemFS().dir("/sub").file("/a.txt", "hello"))
	r := c.do("PROPFIND", "/", propfindAll, "Depth", "0").
		wantCode(t, webdav.StatusMulti, "PROPFIND depth 0")
	r.wantContains(t, "<href xmlns=\"DAV:\">/</href>", "self href").
		wantContains(t, "<collection", "root is a collection").
		wantLacks(t, "a.txt", "depth 0 must not list members")
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !strings.HasPrefix(r.body, "<?xml") {
		t.Fatalf("multistatus must open with an XML declaration; got %q", first(r.body))
	}
}

func TestPropfindDepthOne(t *testing.T) {
	c, _ := serve(t, newMemFS().dir("/sub").file("/a.txt", "hello").file("/sub/deep.txt", "x"))
	r := c.do("PROPFIND", "/", propfindAll, "Depth", "1").
		wantCode(t, webdav.StatusMulti, "PROPFIND depth 1")
	r.wantContains(t, "<href xmlns=\"DAV:\">/a.txt</href>", "member file").
		wantContains(t, "<href xmlns=\"DAV:\">/sub/</href>", "member collection href ends in a slash").
		wantLacks(t, "deep.txt", "depth 1 must not recurse").
		wantContains(t, "<getcontentlength xmlns=\"DAV:\">5</getcontentlength>", "file length").
		wantContains(t, "HTTP/1.1 200 OK", "status line")
	// "." and ".." are on disk in a FAT or ISO directory; emitting them would
	// make a client that walks hrefs recurse for ever.
	if strings.Count(r.body, "<response") != 3 {
		t.Fatalf("want 3 responses (self + 2 members), got %d: %s", strings.Count(r.body, "<response"), truncate(r.body))
	}
}

func TestPropfindDepthInfinityIsRefusedTheWayTheRFCDefines(t *testing.T) {
	c, _ := serve(t, newMemFS())
	// A missing Depth header means infinity (RFC 4918 section 10.2), so both
	// spellings must land on the same refusal.
	for _, hdr := range []string{"infinity", ""} {
		var args []string
		if hdr != "" {
			args = []string{"Depth", hdr}
		}
		c.do("PROPFIND", "/", propfindAll, args...).
			wantCode(t, http.StatusForbidden, "depth infinity").
			wantContains(t, "propfind-finite-depth", "the precondition the RFC defines")
	}
}

func TestPropfindBadDepth(t *testing.T) {
	c, _ := serve(t, newMemFS())
	c.do("PROPFIND", "/", propfindAll, "Depth", "2").
		wantCode(t, http.StatusBadRequest, "bad Depth")
}

func TestPropfindEmptyBodyIsAllprop(t *testing.T) {
	// The macOS client sends no body on the first request of a mount. A
	// server that requires one does not mount there.
	c, _ := serve(t, newMemFS().file("/a.txt", "hello"))
	c.do("PROPFIND", "/a.txt", "", "Depth", "0").
		wantCode(t, webdav.StatusMulti, "empty body").
		wantContains(t, "getcontentlength", "allprop was assumed")
}

func TestPropfindNamedProperties(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "hello"))
	body := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:prop>` +
		`<D:getcontentlength/><D:resourcetype/><D:nosuchproperty/>` +
		`<Z:custom xmlns:Z="http://example.invalid/"/>` +
		`</D:prop></D:propfind>`
	r := c.do("PROPFIND", "/a.txt", body, "Depth", "0").
		wantCode(t, webdav.StatusMulti, "named props")
	// The properties the server has come back 200; the ones it does not come
	// back 404 by name. Answering the whole request 404 is the common bug —
	// it tells the client the resource is missing.
	r.wantContains(t, "HTTP/1.1 200 OK", "found propstat").
		wantContains(t, "HTTP/1.1 404 Not Found", "missing propstat").
		wantContains(t, "nosuchproperty", "the missing name is echoed").
		wantContains(t, "custom", "a dead property in another namespace is reported missing")
}

func TestPropfindPropname(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "hello"))
	body := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:propname/></D:propfind>`
	c.do("PROPFIND", "/a.txt", body, "Depth", "0").
		wantCode(t, webdav.StatusMulti, "propname").
		wantContains(t, "<getcontentlength xmlns=\"DAV:\"></getcontentlength>", "name with no value").
		wantContains(t, "supportedlock", "lock properties are named too")
}

func TestPropfindAllpropWithInclude(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "hello"))
	body := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:allprop/>` +
		`<D:include><D:supportedlock/></D:include></D:propfind>`
	c.do("PROPFIND", "/a.txt", body, "Depth", "0").
		wantCode(t, webdav.StatusMulti, "allprop+include").
		wantContains(t, "supportedlock", "included property")
}

func TestPropfindQuotaPropertiesOnlyWhenCapacityIsKnown(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		c, _ := serve(t, newMemFS())
		// Inventing a plausible number would make a mounted volume's free
		// space display confidently wrong.
		c.do("PROPFIND", "/", propfindAll, "Depth", "0").
			wantLacks(t, "quota-available-bytes", "quota must be absent when unknown")
	})
	t.Run("known", func(t *testing.T) {
		c, _ := serve(t, newMemFS(), webdav.WithCapacity(1000, 400))
		c.do("PROPFIND", "/", propfindAll, "Depth", "0").
			wantContains(t, "<quota-available-bytes xmlns=\"DAV:\">400</quota-available-bytes>", "available").
			wantContains(t, "<quota-used-bytes xmlns=\"DAV:\">600</quota-used-bytes>", "used")
	})
	t.Run("not on a file", func(t *testing.T) {
		c, _ := serve(t, newMemFS().file("/a.txt", "x"), webdav.WithCapacity(1000, 400))
		c.do("PROPFIND", "/a.txt", propfindAll, "Depth", "0").
			wantLacks(t, "quota-", "quota is a property of a collection")
	})
}

func TestPropfindMalformedBody(t *testing.T) {
	c, _ := serve(t, newMemFS())
	for _, tc := range []struct{ name, body string }{
		{"not xml", "<<<"},
		{"wrong root", `<D:propertyupdate xmlns:D="DAV:"/>`},
		{"empty propfind", `<D:propfind xmlns:D="DAV:"></D:propfind>`},
		{"truncated inside prop", `<D:propfind xmlns:D="DAV:"><D:prop><D:foo>`},
		{"truncated inside allprop", `<D:propfind xmlns:D="DAV:"><D:allprop><x>`},
		{"truncated inside include", `<D:propfind xmlns:D="DAV:"><D:include><x>`},
		{"truncated inside unknown", `<D:propfind xmlns:D="DAV:"><D:unknown><x>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.do("PROPFIND", "/", tc.body, "Depth", "0").
				wantCode(t, webdav.StatusUnprocessable, tc.name)
		})
	}
}

func TestPropfindOnAMissingResource(t *testing.T) {
	c, _ := serve(t, newMemFS())
	c.do("PROPFIND", "/nope", propfindAll, "Depth", "0").
		wantCode(t, http.StatusNotFound, "missing resource")
}

func TestPropfindReportsAFailedListing(t *testing.T) {
	fs := newMemFS()
	fs.failWith("ListDir:/", errors.New("media error"))
	c, _ := serve(t, fs)
	c.do("PROPFIND", "/", propfindAll, "Depth", "1").
		wantCode(t, http.StatusInternalServerError, "failed listing")
}

func TestPropfindReportsAnUnreadableMemberAsItsOwnRow(t *testing.T) {
	// One unreadable file must not make a folder unopenable.
	fs := newMemFS().file("/good.txt", "x").file("/bad.txt", "y")
	fs.failWith("Stat:/bad.txt", errors.New("media error"))
	c, _ := serve(t, fs)
	c.do("PROPFIND", "/", propfindAll, "Depth", "1").
		wantCode(t, webdav.StatusMulti, "mixed listing").
		wantContains(t, "good.txt", "readable member").
		wantContains(t, "HTTP/1.1 500 Internal Server Error", "the failing member's own status")
}

func TestSymlinkIsDescribedNotFollowed(t *testing.T) {
	// A link's target is never resolved by this server: resolving one would
	// be the one place a name could point outside the image.
	c, _ := serve(t, newMemFS().symlink("/link", "/etc/passwd"))
	r := c.do("PROPFIND", "/link", propfindAll, "Depth", "0").
		wantCode(t, webdav.StatusMulti, "symlink")
	r.wantLacks(t, "collection", "a symlink is not a collection").
		wantLacks(t, "passwd", "the target must never leave the server")
}

func TestModTimeIsUsedWhenTheDriverHasOne(t *testing.T) {
	base := newMemFS().file("/a.txt", "hello")
	base.nodes["/a.txt"].mtime = 1000000000 // 2001-09-09T01:46:40Z
	c, _ := serve(t, &timeFS{memFS: base})
	c.do("PROPFIND", "/a.txt", propfindAll, "Depth", "0").
		wantContains(t, "Sun, 09 Sep 2001 01:46:40 GMT", "getlastmodified from the driver").
		wantContains(t, "2001-09-09T01:46:40Z", "creationdate in ISO 8601")
}

func TestProppatchRefusesEveryPropertyConformantly(t *testing.T) {
	// There is nowhere on a FAT32 or ISO 9660 image for a dead property to
	// live, so every change is refused — but in a 207 that names each one,
	// not with a blanket error.
	c, _ := serve(t, newMemFS().file("/a.txt", "x"), webdav.ReadWrite())
	body := `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:">` +
		`<D:set><D:prop><Z:mine xmlns:Z="http://example.invalid/">v</Z:mine></D:prop></D:set>` +
		`<D:remove><D:prop><D:displayname/></D:prop></D:remove>` +
		`<D:unknown/></D:propertyupdate>`
	c.do("PROPPATCH", "/a.txt", body).
		wantCode(t, webdav.StatusMulti, "PROPPATCH").
		wantContains(t, "HTTP/1.1 403 Forbidden", "every property refused").
		wantContains(t, "mine", "the refused name is echoed").
		wantContains(t, "displayname", "the removed name is echoed")
}

func TestProppatchErrors(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "x"), webdav.ReadWrite())
	c.do("PROPPATCH", "/a.txt", "<<<").
		wantCode(t, webdav.StatusUnprocessable, "malformed")
	c.do("PROPPATCH", "/a.txt", `<D:propfind xmlns:D="DAV:"/>`).
		wantCode(t, webdav.StatusUnprocessable, "wrong root")
	c.do("PROPPATCH", "/a.txt", `<D:propertyupdate xmlns:D="DAV:"><D:set><D:prop><D:x>`).
		wantCode(t, webdav.StatusUnprocessable, "truncated inside prop")
	c.do("PROPPATCH", "/a.txt", `<D:propertyupdate xmlns:D="DAV:"><D:junk><x>`).
		wantCode(t, webdav.StatusUnprocessable, "truncated inside unknown")
	c.do("PROPPATCH", "/nope", `<D:propertyupdate xmlns:D="DAV:"/>`).
		wantCode(t, http.StatusNotFound, "missing resource")
}

func TestPropfindNamedPropertiesOnACollection(t *testing.T) {
	// The guards that make a property present or absent by resource type are
	// only reachable when a client names the property explicitly: allprop
	// never asks for one that does not apply.
	c, _ := serve(t, newMemFS().dir("/sub").file("/a.txt", "hello"))
	ask := func(path string, names ...string) result {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:prop>`)
		for _, n := range names {
			b.WriteString("<D:" + n + "/>")
		}
		b.WriteString(`</D:prop></D:propfind>`)
		return c.do("PROPFIND", path, b.String(), "Depth", "0")
	}
	// getcontentlength is the one property RFC 4918 leaves undefined for a
	// collection: there is no body to have a length.
	ask("/sub", "getcontentlength").
		wantCode(t, webdav.StatusMulti, "length of a collection").
		wantContains(t, "HTTP/1.1 404 Not Found", "no length on a collection")
	// getcontenttype does apply, and a collection's is the conventional one.
	ask("/sub", "getcontenttype").
		wantContains(t, "httpd/unix-directory", "collection content type")
	// Quota is a property of a collection, and only when capacity is known.
	ask("/a.txt", "quota-available-bytes", "quota-used-bytes").
		wantContains(t, "HTTP/1.1 404 Not Found", "no quota on a file")
	ask("/sub", "quota-available-bytes", "quota-used-bytes").
		wantContains(t, "HTTP/1.1 404 Not Found", "no quota when capacity is unknown")
}

func TestProppatchWithAnEmptyBody(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "x"), webdav.ReadWrite())
	c.do("PROPPATCH", "/a.txt", "").
		wantCode(t, webdav.StatusUnprocessable, "empty PROPPATCH body")
}
