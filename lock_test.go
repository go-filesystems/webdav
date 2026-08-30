package webdav_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/go-filesystems/webdav"
)

// lockBody is what a client sends to take an exclusive write lock. The owner
// carries a nested element on purpose: it is the case that breaks a server
// which echoes the client's raw inner XML back with its prefixes undeclared.
const lockBody = `<?xml version="1.0" encoding="utf-8"?>` +
	`<D:lockinfo xmlns:D="DAV:">` +
	`<D:lockscope><D:exclusive/></D:lockscope>` +
	`<D:locktype><D:write/></D:locktype>` +
	`<D:owner><D:href>mailto:someone@example.invalid</D:href></D:owner>` +
	`</D:lockinfo>`

// tokenFrom extracts the token a LOCK returned.
func tokenFrom(t *testing.T, r result) string {
	t.Helper()
	raw := r.header.Get("Lock-Token")
	if !strings.HasPrefix(raw, "<") || !strings.HasSuffix(raw, ">") {
		t.Fatalf("Lock-Token = %q, want <token>", raw)
	}
	tok := raw[1 : len(raw)-1]
	// The body must carry the same token: a client reads it from whichever
	// of the two it looks at first.
	if !strings.Contains(r.body, tok) {
		t.Fatalf("lockdiscovery body does not carry the token from the header")
	}
	return tok
}

func TestLockThenWriteThenUnlock(t *testing.T) {
	// This is the macOS write sequence: LOCK, PUT with the token in an If
	// header, UNLOCK.
	c, _ := serve(t, newMemFS().file("/a.txt", "old"), webdav.ReadWrite())
	r := c.do("LOCK", "/a.txt", lockBody, "Timeout", "Second-60").
		wantCode(t, http.StatusOK, "LOCK")
	tok := tokenFrom(t, r)
	r.wantContains(t, "<exclusive", "scope").
		wantContains(t, "Second-60", "granted timeout").
		wantContains(t, "someone@example.invalid", "the owner is echoed").
		wantContains(t, "<lockroot", "lockroot")

	// Without the token the write is refused, and refused as a lock problem
	// rather than a generic one.
	c.do("PUT", "/a.txt", "new").wantCode(t, webdav.StatusLocked, "PUT with no token").
		wantContains(t, "lock-token-submitted", "the precondition RFC 4918 defines")
	c.do("PUT", "/a.txt", "new", "If", "(<"+tok+">)").
		wantCode(t, http.StatusNoContent, "PUT with the token")
	c.do("UNLOCK", "/a.txt", "", "Lock-Token", "<"+tok+">").
		wantCode(t, http.StatusNoContent, "UNLOCK")
	c.do("PUT", "/a.txt", "newer").wantCode(t, http.StatusNoContent, "PUT after UNLOCK")
}

func TestLockIsExclusiveForReal(t *testing.T) {
	// A lock that always succeeds satisfies macOS in forty lines and is a
	// lie with a failure mode: two clients would both hold an "exclusive"
	// write lock and the second save would destroy the first.
	c, _ := serve(t, newMemFS().file("/a.txt", "x"), webdav.ReadWrite())
	r := c.do("LOCK", "/a.txt", lockBody).wantCode(t, http.StatusOK, "first LOCK")
	tokenFrom(t, r)
	c.do("LOCK", "/a.txt", lockBody).wantCode(t, webdav.StatusLocked, "second LOCK")
}

func TestLockOnAMissingResourceCreatesIt(t *testing.T) {
	// RFC 4918 section 7.3. This is how every client that locks before
	// writing gets a lock for a file it is about to create; refusing it is
	// another way to end up mounted read-only.
	fs := newMemFS()
	c, _ := serve(t, fs, webdav.ReadWrite())
	r := c.do("LOCK", "/new.txt", lockBody).wantCode(t, http.StatusCreated, "LOCK creating")
	tok := tokenFrom(t, r)
	if _, ok := fs.nodes["/new.txt"]; !ok {
		t.Fatal("LOCK on a missing resource must create an empty one")
	}
	c.do("PUT", "/new.txt", "body", "If", "(<"+tok+">)").
		wantCode(t, http.StatusNoContent, "PUT into the locked empty resource")
}

func TestLockDepthInfinityCoversTheSubtree(t *testing.T) {
	fs := newMemFS().dir("/sub").file("/sub/a.txt", "x")
	c, _ := serve(t, fs, webdav.ReadWrite())
	r := c.do("LOCK", "/sub", lockBody, "Depth", "infinity").
		wantCode(t, http.StatusOK, "depth-infinity LOCK")
	r.wantContains(t, "<depth>infinity</depth>", "granted depth")
	tok := tokenFrom(t, r)
	c.do("PUT", "/sub/a.txt", "y").wantCode(t, webdav.StatusLocked, "member is covered")
	c.do("PUT", "/sub/a.txt", "y", "If", "(<"+tok+">)").
		wantCode(t, http.StatusNoContent, "member with the token")
	// A depth-infinity lock cannot be taken over a subtree that already
	// holds one.
	c.do("LOCK", "/", lockBody, "Depth", "infinity").
		wantCode(t, webdav.StatusLocked, "ancestor infinity LOCK")
	// A depth-0 lock on an unrelated path is fine.
	c.do("LOCK", "/other.txt", lockBody).wantCode(t, http.StatusCreated, "unrelated LOCK")
}

func TestLockRefresh(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "x"), webdav.ReadWrite())
	tok := tokenFrom(t, c.do("LOCK", "/a.txt", lockBody, "Timeout", "Second-60"))
	// An empty body is a refresh of the lock named in the If header.
	c.do("LOCK", "/a.txt", "", "If", "(<"+tok+">)", "Timeout", "Second-120").
		wantCode(t, http.StatusOK, "refresh").
		wantContains(t, "Second-120", "the new timeout")
	// A refresh naming no live lock is 412: the If header is a precondition
	// that did not hold.
	c.do("LOCK", "/a.txt", "", "If", "(<opaquelocktoken:deadbeef>)").
		wantCode(t, http.StatusPreconditionFailed, "refresh with an unknown token")
	c.do("LOCK", "/a.txt", "").
		wantCode(t, http.StatusPreconditionFailed, "refresh with no token at all")
	// A token is a credential for one resource: it must not refresh another.
	c.do("PUT", "/b.txt", "y", "If", "(<"+tok+">)")
	c.do("LOCK", "/b.txt", "", "If", "(<"+tok+">)").
		wantCode(t, http.StatusPreconditionFailed, "refresh across resources")
}

func TestUnlockErrors(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "x"), webdav.ReadWrite())
	tok := tokenFrom(t, c.do("LOCK", "/a.txt", lockBody))
	c.do("UNLOCK", "/a.txt", "").
		wantCode(t, http.StatusBadRequest, "UNLOCK with no Lock-Token")
	c.do("UNLOCK", "/a.txt", "", "Lock-Token", "not-bracketed").
		wantCode(t, http.StatusBadRequest, "UNLOCK with a malformed Lock-Token")
	c.do("UNLOCK", "/a.txt", "", "Lock-Token", "<opaquelocktoken:nope>").
		wantCode(t, http.StatusConflict, "UNLOCK with an unknown token")
	// A token released against another resource is a client error, not a
	// licence to unlock whatever it does name.
	c.do("UNLOCK", "/other.txt", "", "Lock-Token", "<"+tok+">").
		wantCode(t, http.StatusConflict, "UNLOCK against the wrong resource")
}

func TestLockBadRequests(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "x"), webdav.ReadWrite())
	c.do("LOCK", "/a.txt", lockBody, "Depth", "1").
		wantCode(t, http.StatusBadRequest, "LOCK depth 1")
	c.do("LOCK", "/a.txt", lockBody, "Depth", "2").
		wantCode(t, http.StatusBadRequest, "bad Depth")
	c.do("LOCK", "/a.txt", "<<<").
		wantCode(t, webdav.StatusUnprocessable, "malformed body")
	c.do("LOCK", "/a.txt", `<D:propfind xmlns:D="DAV:"/>`).
		wantCode(t, webdav.StatusUnprocessable, "wrong root")
	c.do("LOCK", "/a.txt", `<D:lockinfo xmlns:D="DAV:"><D:owner><D:href>x`).
		wantCode(t, webdav.StatusUnprocessable, "truncated owner")
	c.do("LOCK", "/a.txt", `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive>`).
		wantCode(t, webdav.StatusUnprocessable, "truncated lockscope")
}

func TestLockReportsDriverFailures(t *testing.T) {
	fs := newMemFS()
	fs.failWith("WriteFile:/new.txt", errors.New("no space left on device"))
	c, _ := serve(t, fs, webdav.ReadWrite())
	c.do("LOCK", "/new.txt", lockBody).
		wantCode(t, webdav.StatusInsufficientStorage, "LOCK cannot create the resource")

	fs2 := newMemFS().file("/a.txt", "x")
	fs2.failWith("Stat:/a.txt", errors.New("media error"))
	c2, _ := serve(t, fs2, webdav.ReadWrite())
	c2.do("LOCK", "/a.txt", lockBody).
		wantCode(t, http.StatusInternalServerError, "LOCK on an unstatable resource")
}

func TestLockTimeoutIsClampedAndParsed(t *testing.T) {
	c, _ := serve(t, newMemFS(), webdav.ReadWrite())
	for _, tc := range []struct{ hdr, want string }{
		{"", "Second-300"},                         // default
		{"Second-60", "Second-60"},                 // honoured
		{"Infinite", "Second-3600"},                // clamped: nobody can release an infinite lock
		{"Second-99999", "Second-3600"},            // clamped
		{"Second-0", "Second-300"},                 // nonsense falls back
		{"Second-abc", "Second-300"},               // unparseable falls back
		{"Weeks-2", "Second-300"},                  // unknown unit falls back
		{"Second-99999, Second-30", "Second-3600"}, // the first usable value wins
	} {
		var args []string
		if tc.hdr != "" {
			args = []string{"Timeout", tc.hdr}
		}
		path := "/t" + strings.ReplaceAll(strings.ReplaceAll(tc.hdr, " ", ""), ",", "") + ".txt"
		c.do("LOCK", path, lockBody, args...).
			wantContains(t, tc.want, "Timeout "+tc.hdr)
	}
}

func TestIfHeaderParsing(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "x"), webdav.ReadWrite())
	tok := tokenFrom(t, c.do("LOCK", "/a.txt", lockBody))
	for _, tc := range []struct {
		name, hdr string
		want      int
	}{
		{"no-tag-list", "(<" + tok + ">)", http.StatusNoContent},
		{"tagged-list", "</a.txt> (<" + tok + ">)", http.StatusNoContent},
		{"two conditions", "(<opaquelocktoken:other>) (<" + tok + ">)", http.StatusNoContent},
		{"with an entity tag", `([W/"etag"] <` + tok + `>)`, http.StatusNoContent},
		// "Not <token>" asserts the resource is NOT locked with it. Treating
		// it as a submission would grant exactly the access the client said
		// it did not have.
		{"Not inverts", "(Not <" + tok + ">)", webdav.StatusLocked},
		{"unterminated token", "(<" + tok, webdav.StatusLocked},
		{"unterminated entity tag", `([W/"etag" <` + tok + `>)`, webdav.StatusLocked},
		{"empty", "", webdav.StatusLocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var args []string
			if tc.hdr != "" {
				args = []string{"If", tc.hdr}
			}
			c.do("PUT", "/a.txt", "y", args...).wantCode(t, tc.want, tc.name)
		})
	}
}

func TestEveryMutatingMethodHonoursALock(t *testing.T) {
	fs := newMemFS().file("/a.txt", "x").file("/b.txt", "y").dir("/sub")
	c, _ := serve(t, fs, webdav.ReadWrite())
	tokenFrom(t, c.do("LOCK", "/a.txt", lockBody))
	for _, tc := range []struct {
		method, path string
		headers      []string
	}{
		{"PUT", "/a.txt", nil},
		{"DELETE", "/a.txt", nil},
		{"MKCOL", "/a.txt", nil},
		{"PROPPATCH", "/a.txt", nil},
		{"MOVE", "/a.txt", []string{"Destination", "/c.txt"}},
		{"MOVE", "/b.txt", []string{"Destination", "/a.txt"}},
		{"COPY", "/b.txt", []string{"Destination", "/a.txt"}},
	} {
		r := c.do(tc.method, tc.path, "", tc.headers...)
		if r.code != webdav.StatusLocked {
			t.Fatalf("%s %s: status %d, want 423", tc.method, tc.path, r.code)
		}
	}
}

func TestDeleteAndOverwriteReleaseTheLock(t *testing.T) {
	// A lock on a name whose resource is gone would block whoever creates it
	// next for the rest of the timeout.
	fs := newMemFS().dir("/sub").file("/sub/a.txt", "x").file("/src.txt", "y")
	c, _ := serve(t, fs, webdav.ReadWrite())
	tok := tokenFrom(t, c.do("LOCK", "/sub/a.txt", lockBody))
	c.do("DELETE", "/sub", "", "If", "(<"+tok+">)").
		wantCode(t, http.StatusNoContent, "DELETE the tree")
	// The member's lock went with it, so a fresh LOCK on the same name works.
	c.do("LOCK", "/sub/a.txt", lockBody).wantCode(t, http.StatusCreated, "re-lock after DELETE")

	tok2 := tokenFrom(t, c.do("LOCK", "/dest.txt", lockBody))
	c.do("MOVE", "/src.txt", "", "Destination", "/dest.txt", "If", "(<"+tok2+">)").
		wantCode(t, http.StatusNoContent, "overwriting MOVE")
	c.do("LOCK", "/dest.txt", lockBody).wantCode(t, http.StatusOK, "re-lock after overwrite")
}

func TestLockdiscoveryReportsALiveLock(t *testing.T) {
	c, _ := serve(t, newMemFS().file("/a.txt", "x"), webdav.ReadWrite(), webdav.WithPrefix("/dav"))
	tok := tokenFrom(t, c.do("LOCK", "/dav/a.txt", lockBody))
	r := c.do("PROPFIND", "/dav/a.txt", propfindAll, "Depth", "0").
		wantCode(t, webdav.StatusMulti, "PROPFIND on a locked resource")
	r.wantContains(t, tok, "the token in lockdiscovery").
		wantContains(t, "<lockroot><href>/dav/a.txt</href>", "the lockroot carries the prefix")
	// An unlocked resource simply has no activelock.
	c.do("PROPFIND", "/dav", propfindAll, "Depth", "0").
		wantLacks(t, "activelock", "no lock, no activelock")
}

func TestLockDepthZeroDoesNotCoverMembers(t *testing.T) {
	// RFC 4918 section 9.10.3: a LOCK with no Depth header acts as though
	// "infinity" had been sent, so depth 0 has to be asked for explicitly.
	fs := newMemFS().dir("/sub").file("/sub/a.txt", "x")
	c, _ := serve(t, fs, webdav.ReadWrite())
	r := c.do("LOCK", "/sub", lockBody, "Depth", "0").
		wantCode(t, http.StatusOK, "depth-0 LOCK")
	r.wantContains(t, "<depth>0</depth>", "granted depth")
	tokenFrom(t, r)
	c.do("PUT", "/sub/a.txt", "y").
		wantCode(t, http.StatusNoContent, "a depth-0 lock does not reach a member")
}
