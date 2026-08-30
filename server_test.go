package webdav_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/go-filesystems/webdav"
)

func TestNewRejectsNilFilesystem(t *testing.T) {
	if _, err := webdav.New(nil); !errors.Is(err, webdav.ErrNilFilesystem) {
		t.Fatalf("New(nil) = %v, want ErrNilFilesystem", err)
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  webdav.Option
		want error
	}{
		{"prefix relative", webdav.WithPrefix("files"), webdav.ErrPrefix},
		{"prefix trailing slash", webdav.WithPrefix("/files/"), webdav.ErrPrefix},
		{"prefix unclean", webdav.WithPrefix("/a/../b"), webdav.ErrPrefix},
		{"nil basic verifier", webdav.WithBasicAuth("r", nil), webdav.ErrNilVerifier},
		{"nil bearer verifier", webdav.WithBearerAuth("r", nil), webdav.ErrNilVerifier},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := webdav.New(newMemFS(), tc.opt); !errors.Is(err, tc.want) {
				t.Fatalf("New = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestEmptyPrefixIsAccepted(t *testing.T) {
	if _, err := webdav.New(newMemFS(), webdav.WithPrefix("")); err != nil {
		t.Fatalf("WithPrefix(\"\"): %v", err)
	}
}

func TestOptionsAdvertisesClasses(t *testing.T) {
	t.Run("read-only", func(t *testing.T) {
		c, _ := serve(t, newMemFS())
		r := c.do("OPTIONS", "/", "").wantCode(t, http.StatusOK, "OPTIONS")
		if got := r.header.Get("DAV"); got != "1" {
			t.Fatalf("DAV = %q, want 1 on a read-only export", got)
		}
		if got := r.header.Get("Allow"); strings.Contains(got, "PUT") {
			t.Fatalf("Allow = %q, must not offer PUT on a read-only export", got)
		}
		if got := r.header.Get("Content-Length"); got != "0" {
			t.Fatalf("Content-Length = %q, want 0", got)
		}
	})
	t.Run("read-write advertises class 2", func(t *testing.T) {
		// This is the macOS requirement: without class 2 the Finder mounts
		// read-only whatever Allow says.
		c, _ := serve(t, newMemFS(), webdav.ReadWrite())
		r := c.do("OPTIONS", "/", "").wantCode(t, http.StatusOK, "OPTIONS")
		if got := r.header.Get("DAV"); got != "1, 2" {
			t.Fatalf("DAV = %q, want \"1, 2\"", got)
		}
		if got := r.header.Get("MS-Author-Via"); got != "DAV" {
			t.Fatalf("MS-Author-Via = %q", got)
		}
		for _, m := range []string{"PUT", "DELETE", "MKCOL", "COPY", "MOVE", "LOCK", "UNLOCK", "PROPPATCH"} {
			if !strings.Contains(r.header.Get("Allow"), m) {
				t.Fatalf("Allow = %q, missing %s", r.header.Get("Allow"), m)
			}
		}
	})
}

func TestDAVHeaderIsOnEveryResponse(t *testing.T) {
	// The macOS client reads the DAV header off the PROPFIND that follows a
	// mount, not off the OPTIONS that preceded it.
	c, _ := serve(t, newMemFS().file("/a.txt", "x"), webdav.ReadWrite())
	for _, m := range []string{"GET", "PROPFIND"} {
		r := c.do(m, "/a.txt", "")
		if got := r.header.Get("DAV"); got != "1, 2" {
			t.Fatalf("%s: DAV = %q", m, got)
		}
	}
}

func TestUnknownMethodIsRefusedWithAllow(t *testing.T) {
	c, _ := serve(t, newMemFS())
	r := c.do("BREW", "/", "").wantCode(t, http.StatusMethodNotAllowed, "BREW")
	if r.header.Get("Allow") == "" {
		t.Fatal("405 must carry an Allow header")
	}
}

func TestReadOnlyRefusesEveryMutatingMethod(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "x"))
	for _, m := range []string{"PUT", "DELETE", "MKCOL", "COPY", "MOVE", "PROPPATCH", "LOCK", "UNLOCK"} {
		r := c.do(m, "/a.txt", "")
		// 403, not 405: the method is implemented and would be allowed on a
		// writable export, which is what RFC 9110 distinguishes them by.
		if r.code != http.StatusForbidden {
			t.Fatalf("%s on a read-only export: %d, want 403", m, r.code)
		}
	}
}

func TestPrefixRoutesAndIsQuotedBackInHrefs(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "hello"), webdav.WithPrefix("/files"))
	c.do("GET", "/files/a.txt", "").wantCode(t, http.StatusOK, "prefixed GET").
		wantContains(t, "hello", "prefixed GET")
	c.do("GET", "/files", "").wantCode(t, http.StatusOK, "prefixed root")
	// An href in a multistatus must carry the prefix, or the client follows
	// it to a path this handler refuses.
	c.do("PROPFIND", "/files", propfindAll, "Depth", "1").
		wantCode(t, webdav.StatusMulti, "prefixed PROPFIND").
		wantContains(t, "<href xmlns=\"DAV:\">/files/a.txt</href>", "prefixed href")
	// A path outside the prefix belongs to whoever else is on the mux.
	c.do("GET", "/elsewhere/a.txt", "").wantCode(t, http.StatusBadRequest, "outside prefix")
}

func TestNilStatFromDriverIsAServerError(t *testing.T) {
	fs := newMemFS().file("/a.txt", "x")
	fs.failWith("NilStat:/a.txt", errors.New("inject"))
	c, _ := serve(t, fs)
	c.do("GET", "/a.txt", "").wantCode(t, http.StatusInternalServerError, "nil Stat")
}

// --- authentication --------------------------------------------------------

func TestNoAuthConfiguredAcceptsEveryone(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "x"))
	c.do("GET", "/a.txt", "").wantCode(t, http.StatusOK, "unauthenticated GET")
}

func TestBasicAuth(t *testing.T) {
	user, pass := secret(t), secret(t)
	opt := webdav.WithBasicAuth("images", func(u, p string) bool {
		return webdav.Verify(u, user) && webdav.Verify(p, pass)
	})
	c := serveTLS(t, newMemFS().file("/a.txt", "x"), opt)

	r := c.do("GET", "/a.txt", "").wantCode(t, http.StatusUnauthorized, "no credentials")
	if ch := r.header.Get("WWW-Authenticate"); !strings.HasPrefix(ch, `Basic realm="images"`) {
		t.Fatalf("challenge = %q", ch)
	}
	c.do("GET", "/a.txt", "", "Authorization", basic(user, secret(t))).
		wantCode(t, http.StatusUnauthorized, "wrong password")
	c.do("GET", "/a.txt", "", "Authorization", "Basic not-base64!").
		wantCode(t, http.StatusUnauthorized, "malformed credentials")
	c.do("GET", "/a.txt", "", "Authorization", basic(user, pass)).
		wantCode(t, http.StatusOK, "correct credentials")
}

func TestBearerAuth(t *testing.T) {
	token := secret(t)
	c := serveTLS(t, newMemFS().file("/a.txt", "x"),
		webdav.WithBearerAuth("images", func(got string) bool { return webdav.Verify(got, token) }))

	r := c.do("GET", "/a.txt", "").wantCode(t, http.StatusUnauthorized, "no token")
	if ch := r.header.Get("WWW-Authenticate"); ch != `Bearer realm="images"` {
		t.Fatalf("challenge = %q", ch)
	}
	c.do("GET", "/a.txt", "", "Authorization", "Bearer "+secret(t)).
		wantCode(t, http.StatusUnauthorized, "wrong token")
	c.do("GET", "/a.txt", "", "Authorization", "Basic "+token).
		wantCode(t, http.StatusUnauthorized, "wrong scheme")
	c.do("GET", "/a.txt", "", "Authorization", "Bearer").
		wantCode(t, http.StatusUnauthorized, "scheme with no token")
	c.do("GET", "/a.txt", "", "Authorization", "bearer "+token).
		wantCode(t, http.StatusOK, "scheme name is case-insensitive")
}

func TestBothSchemesAreOfferedAndEitherIsAccepted(t *testing.T) {
	user, pass, token := secret(t), secret(t), secret(t)
	c := serveTLS(t, newMemFS().file("/a.txt", "x"),
		webdav.WithBasicAuth("r", func(u, p string) bool { return u == user && p == pass }),
		webdav.WithBearerAuth("r", func(got string) bool { return got == token }))
	r := c.do("GET", "/a.txt", "").wantCode(t, http.StatusUnauthorized, "no credentials")
	if got := len(r.header.Values("WWW-Authenticate")); got != 2 {
		t.Fatalf("%d challenges, want 2", got)
	}
	c.do("GET", "/a.txt", "", "Authorization", basic(user, pass)).wantCode(t, http.StatusOK, "basic")
	c.do("GET", "/a.txt", "", "Authorization", "Bearer "+token).wantCode(t, http.StatusOK, "bearer")
}

func TestCredentialsAreRefusedOverCleartext(t *testing.T) {
	// httptest binds to loopback, which is exempt: a credential there has no
	// link to be intercepted on. The refusal is therefore exercised by
	// making the handler believe the peer is not loopback.
	user, pass := secret(t), secret(t)
	h, err := webdav.New(newMemFS().file("/a.txt", "x"),
		webdav.WithBasicAuth("r", func(u, p string) bool { return u == user && p == pass }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, tc := range []struct {
		name, remote string
		want         int
	}{
		{"loopback is exempt", "127.0.0.1:5000", http.StatusOK},
		{"IPv6 loopback is exempt", "[::1]:5000", http.StatusOK},
		{"public peer is refused", "198.51.100.7:5000", http.StatusUpgradeRequired},
		{"unparseable peer is refused", "not-an-address", http.StatusUpgradeRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request(h, "GET", "/a.txt", tc.remote, "Authorization", basic(user, pass))
			if r.Code != tc.want {
				t.Fatalf("status %d, want %d", r.Code, tc.want)
			}
			if tc.want == http.StatusUpgradeRequired && r.Header().Get("Upgrade") == "" {
				t.Fatal("426 must name the upgrade it wants")
			}
		})
	}
	t.Run("AllowInsecureAuth lifts it", func(t *testing.T) {
		h, err := webdav.New(newMemFS().file("/a.txt", "x"),
			webdav.WithBasicAuth("r", func(u, p string) bool { return u == user && p == pass }),
			webdav.AllowInsecureAuth())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if r := request(h, "GET", "/a.txt", "198.51.100.7:5000", "Authorization", basic(user, pass)); r.Code != http.StatusOK {
			t.Fatalf("status %d, want 200", r.Code)
		}
	})
}

func TestRealmIsNeverAllowedToSplitTheHeader(t *testing.T) {
	// The realm comes from the caller, not the network, but a value that can
	// break out of a quoted string is a header-injection primitive whoever
	// supplied it.
	c := serveTLS(t, newMemFS(),
		webdav.WithBasicAuth("a\"b\\c\r\nX-Injected: 1", func(string, string) bool { return false }))
	r := c.do("GET", "/", "").wantCode(t, http.StatusUnauthorized, "challenge")
	if r.header.Get("X-Injected") != "" {
		t.Fatal("realm split the header")
	}
	if got := r.header.Get("WWW-Authenticate"); !strings.Contains(got, `a\"b\\cX-Injected: 1`) {
		t.Fatalf("challenge = %q", got)
	}
}

func TestVerifyComparesEqualStrings(t *testing.T) {
	s := secret(t)
	if !webdav.Verify(s, s) {
		t.Fatal("Verify must accept equal strings")
	}
	if webdav.Verify(s, s+"x") {
		t.Fatal("Verify must reject different strings")
	}
}
