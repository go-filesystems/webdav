package webdav_test

// Regression tests for the 2026-11-22 audit of go-filesystems/webdav:
// the lock table retained client-chosen memory, and a lock on a member did
// not protect it from an operation on its parent (RFC 4918 section 9.6.1).

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-filesystems/webdav"
)

func lockDo(h http.Handler, method, target string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const boundsLockBody = `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype><D:owner>alice</D:owner></D:lockinfo>`

func ownerBody(n int) string {
	return `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"><D:owner>` + strings.Repeat("A", n) + `</D:owner></D:lockinfo>`
}

func newLockHandler(t *testing.T, fs *memFS) http.Handler {
	t.Helper()
	h, err := webdav.New(fs, webdav.ReadWrite())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// An <owner> is kept for the life of the lock, so it is capped: 256 requests
// of 1 MiB used to retain 256 MiB, and the count cap allowed 64 GiB.
func TestLockOwnerTooLargeIsRefused(t *testing.T) {
	h := newLockHandler(t, newMemFS())
	w := lockDo(h, "LOCK", "/f", map[string]string{"Depth": "0"}, ownerBody(1<<20-512))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("LOCK with a 1 MiB owner: %d, want 413", w.Code)
	}
	if w := lockDo(h, "LOCK", "/f", map[string]string{"Depth": "0"}, ownerBody(4<<10)); w.Code != http.StatusCreated {
		t.Fatalf("LOCK with an owner at the cap: %d, want 201", w.Code)
	}
}

// The table also bounds the bytes it retains, whatever the count: owners at
// the per-lock cap run out of budget long before 65536 locks.
func TestLockTableBytesAreBounded(t *testing.T) {
	h := newLockHandler(t, newMemFS())
	refused := false
	for i := 0; i < 5000; i++ {
		w := lockDo(h, "LOCK", "/f"+strconv.Itoa(i), map[string]string{"Depth": "0"}, ownerBody(4<<10))
		if w.Code == http.StatusCreated {
			continue
		}
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("LOCK %d: %d, want 500 once the table is full", i, w.Code)
		}
		refused = true
		break
	}
	if !refused {
		t.Fatal("5000 locks with 4 KiB owners were all accepted: the table is not bounded by bytes")
	}
}

// A lock on a member protects it from DELETE, MOVE and an overwriting COPY or
// MOVE of an ancestor by a client that does not hold the token.
func TestLockOnMemberProtectsParentOperations(t *testing.T) {
	fs := newMemFS().dir("/dir").file("/dir/locked.txt", "precious").file("/src.txt", "s").dir("/dst").file("/dst/locked.txt", "d")
	h := newLockHandler(t, fs)
	w := lockDo(h, "LOCK", "/dir/locked.txt", map[string]string{"Depth": "0"}, boundsLockBody)
	if w.Code != http.StatusOK {
		t.Fatalf("LOCK: %d", w.Code)
	}
	token := strings.Trim(w.Header().Get("Lock-Token"), "<>")
	if w := lockDo(h, "LOCK", "/dst/locked.txt", map[string]string{"Depth": "0"}, boundsLockBody); w.Code != http.StatusOK {
		t.Fatalf("LOCK dst: %d", w.Code)
	}

	for _, c := range []struct {
		name, method, target string
		hdr                  map[string]string
	}{
		{"DELETE parent", "DELETE", "/dir", nil},
		{"MOVE parent away", "MOVE", "/dir", map[string]string{"Destination": "/elsewhere"}},
		{"MOVE onto parent", "MOVE", "/src.txt", map[string]string{"Destination": "/dir", "Overwrite": "T"}},
		{"COPY over parent", "COPY", "/src.txt", map[string]string{"Destination": "/dst", "Overwrite": "T"}},
	} {
		if w := lockDo(h, c.method, c.target, c.hdr, ""); w.Code != webdav.StatusLocked {
			t.Fatalf("%s without a token: %d, want 423", c.name, w.Code)
		}
		if _, err := fs.Stat("/dir/locked.txt"); err != nil {
			t.Fatalf("%s removed the locked member: %v", c.name, err)
		}
	}

	// With the token, the same DELETE goes through.
	w = lockDo(h, "DELETE", "/dir", map[string]string{"If": "(<" + token + ">)"}, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE with the token: %d, want 204", w.Code)
	}
	if _, err := fs.Stat("/dir"); err == nil {
		t.Fatal("DELETE with the token left /dir behind")
	}
}

// LOCK on an unmapped URL used to create the empty resource before the lock
// table was consulted, leaving a file inside a tree another client had locked.
func TestLockRefusedCreatesNoFile(t *testing.T) {
	fs := newMemFS().dir("/dir")
	h := newLockHandler(t, fs)
	if w := lockDo(h, "LOCK", "/dir", map[string]string{"Depth": "infinity"}, boundsLockBody); w.Code != http.StatusOK {
		t.Fatalf("LOCK /dir: %d", w.Code)
	}
	w := lockDo(h, "LOCK", "/dir/intruder.txt", map[string]string{"Depth": "0"}, boundsLockBody)
	if w.Code != webdav.StatusLocked {
		t.Fatalf("LOCK inside a locked tree: %d, want 423", w.Code)
	}
	if _, err := fs.Stat("/dir/intruder.txt"); err == nil {
		t.Fatal("a refused LOCK created /dir/intruder.txt")
	}
}
