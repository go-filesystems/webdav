package webdav

// The tests here are in the package rather than beside it because the
// branches they reach cannot be reached from the wire: a dead CSPRNG, a full
// lock table, a lock that expired between two requests. Every one of them is
// a path where the wrong behaviour would be *silent* — a predictable token,
// an evicted lock, a lock held for ever — which is exactly the kind that has
// to be executed by a test rather than reasoned about.

import (
	"encoding/xml"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// withBrokenRand makes the CSPRNG fail for the duration of one test.
func withBrokenRand(t *testing.T, after int) {
	t.Helper()
	prev := randRead
	n := 0
	randRead = func(b []byte) (int, error) {
		if n >= after {
			return 0, errors.New("no entropy")
		}
		n++
		return prev(b)
	}
	t.Cleanup(func() { randRead = prev })
}

func TestServerRefusesToStartWithoutEntropy(t *testing.T) {
	// A server that cannot mint unguessable lock tokens must refuse to start
	// rather than start insecurely: a guessable token is a credential
	// anybody can produce, and the failure would be invisible.
	withBrokenRand(t, 0)
	if _, err := New(stubFS{}); err == nil {
		t.Fatal("New must fail when the CSPRNG does")
	}
	if _, err := newLockSystem(); err == nil {
		t.Fatal("newLockSystem must fail when the CSPRNG does")
	}
}

func TestLockCreationFailsWhenEntropyRunsOut(t *testing.T) {
	// The probe at construction succeeded and the CSPRNG then failed, which
	// is the case a construction-time check alone would miss.
	ls, err := newLockSystem()
	if err != nil {
		t.Fatalf("newLockSystem: %v", err)
	}
	withBrokenRand(t, 0)
	if _, err := ls.create("/a", depthZero, nil, time.Minute); err == nil {
		t.Fatal("create must fail when a token cannot be minted")
	}
}

func TestLockTableIsBounded(t *testing.T) {
	// A client that locks a million distinct paths must hit a wall rather
	// than exhaust the machine.
	ls, err := newLockSystem()
	if err != nil {
		t.Fatalf("newLockSystem: %v", err)
	}
	// The bound is exercised against a table filled to it directly rather
	// than by taking 65536 real locks, which would test the machine's
	// patience instead of the check.
	for i := 0; i < maxLocks; i++ {
		ls.byToken[strconv.Itoa(i)] = &lockInfo{expiry: ls.now().Add(time.Hour)}
	}
	if _, err := ls.create("/a", depthZero, nil, time.Minute); !errors.Is(err, ErrLockTableFull) {
		t.Fatalf("create = %v, want ErrLockTableFull", err)
	}
}

func TestExpiredLocksAreSweptLazily(t *testing.T) {
	// Expiry is lazy rather than swept by a timer: a goroutine per Handler
	// would be a real cost in the one-Handler-per-tenant deployment. What
	// has to be true is that every path which could observe a stale lock
	// sweeps first.
	ls, err := newLockSystem()
	if err != nil {
		t.Fatalf("newLockSystem: %v", err)
	}
	now := time.Now()
	ls.now = func() time.Time { return now }
	l, err := ls.create("/a", depthZero, nil, time.Minute)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ls.confirm("/a", nil); !errors.Is(err, ErrLocked) {
		t.Fatalf("a live lock must block: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if err := ls.confirm("/a", nil); err != nil {
		t.Fatalf("an expired lock must not block: %v", err)
	}
	if ls.find("/a") != nil {
		t.Fatal("an expired lock must be gone from the table")
	}
	if err := ls.unlock("/a", l.token); !errors.Is(err, ErrNoSuchLock) {
		t.Fatalf("unlock of an expired lock = %v, want ErrNoSuchLock", err)
	}
	// A resource whose lock expired can be locked again.
	if _, err := ls.create("/a", depthZero, nil, time.Minute); err != nil {
		t.Fatalf("re-lock after expiry: %v", err)
	}
}

func TestWriteLockErrorMapsAnUnexpectedFailure(t *testing.T) {
	// ErrLockTableFull reaches the wire through this function, and a table
	// that filled up is the server's problem, not the client's.
	w := httptest.NewRecorder()
	writeLockError(w, ErrLockTableFull)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", w.Code)
	}
}

func TestStatusForKnowsTheInterfaceSentinels(t *testing.T) {
	// The substring table is a last resort; a driver that wraps a real
	// sentinel must never reach it.
	for _, tc := range []struct {
		err  error
		want int
	}{
		{fs.ErrNotExist, http.StatusNotFound},
		{fs.ErrExist, http.StatusMethodNotAllowed},
		{fs.ErrPermission, http.StatusForbidden},
		{fs.ErrInvalid, http.StatusBadRequest},
		{filesystem.ErrShrinkUnsupported, http.StatusNotImplemented},
		{errors.New("nothing recognisable"), http.StatusInternalServerError},
	} {
		if got := statusFor(tc.err, http.StatusInternalServerError); got != tc.want {
			t.Fatalf("statusFor(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
	// A wrapped sentinel must still be recognised: drivers wrap.
	if got := statusFor(errors.Join(errors.New("open image"), fs.ErrNotExist), 500); got != http.StatusNotFound {
		t.Fatalf("wrapped sentinel = %d, want 404", got)
	}
}

// TestEveryNamedPropertyHasAValue holds the invariant that lets allProps skip
// the lookup check: every name propNames lists is one propValue can render.
// If the two ever drift, this fails rather than an empty property appearing
// in somebody's mounted volume.
func TestEveryNamedPropertyHasAValue(t *testing.T) {
	h, err := New(stubFS{}, WithCapacity(100, 50))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, info := range []resourceInfo{
		{path: "/", name: "", isDir: true},
		{path: "/a.txt", name: "a.txt"},
		{path: "/link", name: "link", isLink: true},
	} {
		for _, n := range h.propNames(info) {
			if _, ok := h.propValue(info, xml.Name{Space: davNS, Local: n}); !ok {
				t.Fatalf("propNames lists %q for %+v but propValue cannot render it", n, info)
			}
		}
		if got := len(h.allProps(info)); got != len(h.propNames(info)) {
			t.Fatalf("allProps rendered %d of %d names", got, len(h.propNames(info)))
		}
	}
}

// stubFS is the smallest thing that satisfies the contract, for the tests
// above that never touch it.
type stubFS struct{}

func (stubFS) Close() error                                  { return nil }
func (stubFS) ReadFile(string) ([]byte, error)               { return nil, fs.ErrNotExist }
func (stubFS) ListDir(string) ([]filesystem.DirEntry, error) { return nil, fs.ErrNotExist }
func (stubFS) Stat(string) (filesystem.Stat, error)          { return nil, fs.ErrNotExist }
func (stubFS) WriteFile(string, []byte, fs.FileMode) error   { return fs.ErrPermission }
func (stubFS) ReadLink(string) (string, error)               { return "", fs.ErrNotExist }
func (stubFS) MkDir(string, fs.FileMode) error               { return fs.ErrPermission }
func (stubFS) DeleteFile(string) error                       { return fs.ErrPermission }
func (stubFS) DeleteDir(string) error                        { return fs.ErrPermission }
func (stubFS) Rename(string, string) error                   { return fs.ErrPermission }
