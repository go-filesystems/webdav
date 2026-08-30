package webdav

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Locking is the one part of WebDAV that exists because of a client rather
// than because of a filesystem, so the reasoning is written down.
//
// # Why lock at all
//
// The macOS WebDAV client will not write to a server that does not advertise
// compliance class 2. It takes an exclusive write lock before every PUT and
// releases it after, and a mount against a class-1 server is silently
// read-only however permissive the Allow header is. Windows' Mini-Redirector
// behaves the same way for some operations. So a WebDAV server for a
// filesystem that people actually mount has to lock, and "the client is the
// judge" is the standard this module is held to.
//
// # What is implemented, and what a fake would have cost
//
// A tempting shortcut is a lock that always succeeds, remembers nothing and
// enforces nothing: it satisfies macOS in about forty lines. It is rejected
// here because it is a lie with a failure mode. Two clients editing one file
// would both be granted "exclusive" write locks and the second save would
// silently destroy the first, which is precisely the outcome the client asked
// the server to prevent. A server that answers "you hold it exclusively" to
// two callers is worse than one that answers "I do not lock", because the
// first is trusted.
//
// So the locks here are real, and their limits are the honest ones:
//
//   - Exclusive write locks only. Shared locks are not offered, and
//     supportedlock says so, which is a legal answer: RFC 4918 requires a
//     server to describe what it supports, not to support everything.
//   - Depth 0 and depth infinity, both enforced. A depth-infinity lock on a
//     collection blocks writes anywhere beneath it.
//   - The lock table is in memory and dies with the process. A lock does not
//     survive a restart, which is the same honest choice
//     [github.com/go-filesystems/nfs] makes about file handles: after a
//     restart an old token is *rejected*, never silently reinterpreted onto
//     a resource somebody else now holds.
//   - There is no persistence and no cross-process coordination, so two
//     servers over the same image do not see each other's locks. The
//     deployment this is built for is one image per process, where that
//     cannot arise; anything else must not rely on these locks for safety.
//
// # Tokens
//
// A token is 128 bits from the system CSPRNG, formatted as an
// opaquelocktoken URI. It has to be unguessable: the token is the *only*
// credential for the lock — anyone who submits it in an If header may write —
// so a counter or a timestamp would let a second client take over the first's
// lock by guessing. The CSPRNG failing is therefore a refusal to start, not a
// fallback, exactly as it is for NFS file handles.

// Lock defaults and bounds.
const (
	// defaultLockTimeout is what a LOCK with no Timeout header gets.
	defaultLockTimeout = 5 * time.Minute
	// maxLockTimeout caps what a client may ask for. A client asking for a
	// year would otherwise pin a resource for a year after it crashed, and
	// nothing in the protocol lets the server say "no, but here is less"
	// other than granting less, which is what this does.
	maxLockTimeout = time.Hour
	// maxLocks bounds the table so that a client cannot exhaust memory by
	// locking a million distinct paths.
	maxLocks = 1 << 16
)

// Lock errors, mapped to wire statuses by [writeLockError].
var (
	// ErrLocked reports a resource locked by a token the request did not
	// submit. It becomes 423 Locked.
	ErrLocked = errors.New("webdav: resource is locked")
	// ErrNoSuchLock reports a token that names no live lock. It becomes 409
	// Conflict for UNLOCK and 412 for a refresh, per RFC 4918.
	ErrNoSuchLock = errors.New("webdav: no such lock")
	// ErrLockTableFull reports the lock table hitting maxLocks.
	ErrLockTableFull = errors.New("webdav: lock table full")
)

// supportedLockXML is the constant value of the supportedlock property: one
// entry, exclusive write. It is a constant because the answer never varies
// per resource, and because a generated one would be a chance to disagree
// with what [lockSystem.create] actually grants.
const supportedLockXML = `<lockentry xmlns="DAV:">` +
	`<lockscope><exclusive/></lockscope>` +
	`<locktype><write/></locktype>` +
	`</lockentry>`

// randRead is crypto/rand.Read, indirected so a test can prove the server
// refuses to start rather than mint guessable lock tokens when the CSPRNG is
// unavailable. There is no other way to reach that branch, and it is the one
// branch where the wrong behaviour is silent.
var randRead = rand.Read

// lockInfo is one held lock.
type lockInfo struct {
	token string
	path  string
	// depth is depthZero or depthInfinity.
	depth int
	// owner is the client's <owner> element, re-encoded (never echoed
	// verbatim) so that lockdiscovery is well-formed whatever arrived.
	owner   []byte
	expiry  time.Time
	timeout time.Duration
}

// lockSystem is the in-memory lock table.
type lockSystem struct {
	mu      sync.Mutex
	byToken map[string]*lockInfo
	byPath  map[string]*lockInfo
	// now is time.Now, indirected so expiry can be tested without sleeping.
	now func() time.Time
}

func newLockSystem() (*lockSystem, error) {
	// The CSPRNG is proved available at construction rather than at the
	// first LOCK, so a server that could only mint guessable tokens fails to
	// start instead of failing under a client.
	var probe [1]byte
	if _, err := randRead(probe[:]); err != nil {
		return nil, err
	}
	return &lockSystem{
		byToken: map[string]*lockInfo{},
		byPath:  map[string]*lockInfo{},
		now:     time.Now,
	}, nil
}

// newToken mints an unguessable lock token.
func newToken() (string, error) {
	var b [16]byte
	if _, err := randRead(b[:]); err != nil {
		return "", err
	}
	return "opaquelocktoken:" + hex.EncodeToString(b[:]), nil
}

// sweep drops expired locks. The caller must hold [lockSystem.mu].
//
// Expiry is lazy rather than swept by a timer: a background goroutine per
// Handler would be a real cost in the one-Handler-per-tenant deployment this
// is built for, and a lock that has expired but not yet been noticed is
// indistinguishable from one that has, because every path that could observe
// it calls this first.
func (ls *lockSystem) sweep() {
	now := ls.now()
	for token, l := range ls.byToken {
		if now.After(l.expiry) {
			delete(ls.byToken, token)
			delete(ls.byPath, l.path)
		}
	}
}

// conflict reports the live lock that blocks an operation on path, if any.
// The caller must hold [lockSystem.mu].
//
// Three ways a lock can block a path, and all three have to be checked:
// the path itself is locked; an ancestor holds a depth-infinity lock; or the
// caller is trying to take a depth-infinity lock over a subtree that already
// contains one.
func (ls *lockSystem) conflict(path string, depth int) *lockInfo {
	if l, ok := ls.byPath[path]; ok {
		return l
	}
	for p := parentOf(path); ; p = parentOf(p) {
		if l, ok := ls.byPath[p]; ok && l.depth == depthInfinity {
			return l
		}
		if p == "/" {
			break
		}
	}
	if depth == depthInfinity {
		for p, l := range ls.byPath {
			if strictlyUnder(path, p) {
				return l
			}
		}
	}
	return nil
}

// create takes an exclusive write lock.
func (ls *lockSystem) create(path string, depth int, owner []byte, timeout time.Duration) (*lockInfo, error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.sweep()
	if l := ls.conflict(path, depth); l != nil {
		return nil, ErrLocked
	}
	if len(ls.byToken) >= maxLocks {
		return nil, ErrLockTableFull
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	l := &lockInfo{
		token:   token,
		path:    path,
		depth:   depth,
		owner:   owner,
		timeout: timeout,
		expiry:  ls.now().Add(timeout),
	}
	ls.byToken[token] = l
	ls.byPath[path] = l
	return l, nil
}

// refresh extends a lock a client re-submitted with no body.
func (ls *lockSystem) refresh(path string, tokens []string, timeout time.Duration) (*lockInfo, error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.sweep()
	for _, t := range tokens {
		l, ok := ls.byToken[t]
		if !ok {
			continue
		}
		if l.path != path {
			// A token is a credential for one resource. Refreshing a lock on
			// a different path with it would let a client keep any lock alive
			// from anywhere it happened to hold one.
			continue
		}
		l.timeout = timeout
		l.expiry = ls.now().Add(timeout)
		return l, nil
	}
	return nil, ErrNoSuchLock
}

// unlock releases a lock by token. The path must match: a token released
// against another resource is a client error, not a licence to unlock
// whatever it does name.
func (ls *lockSystem) unlock(path, token string) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.sweep()
	l, ok := ls.byToken[token]
	if !ok || l.path != path {
		return ErrNoSuchLock
	}
	delete(ls.byToken, token)
	delete(ls.byPath, path)
	return nil
}

// releaseTree drops the locks on a path and everything under it, which is
// what a successful DELETE or an overwriting MOVE leaves behind: the resource
// is gone, so holding a lock on its name would block whoever creates it next
// for the rest of the timeout.
func (ls *lockSystem) releaseTree(path string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	for p, l := range ls.byPath {
		if under(path, p) {
			delete(ls.byPath, p)
			delete(ls.byToken, l.token)
		}
	}
}

// confirm reports whether an operation on path may proceed given the tokens
// the request submitted in its If header.
func (ls *lockSystem) confirm(path string, tokens []string) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.sweep()
	l := ls.conflict(path, depthZero)
	if l == nil {
		return nil
	}
	for _, t := range tokens {
		if t == l.token {
			return nil
		}
	}
	return ErrLocked
}

// find returns the live lock on exactly this path, for lockdiscovery.
func (ls *lockSystem) find(path string) *lockInfo {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.sweep()
	return ls.byPath[path]
}

// discoveryXML renders the lockdiscovery property for a path: the activelock
// element when one is held, and nothing when none is.
func (h *Handler) discoveryXML(path string) []byte {
	l := h.locks.find(path)
	if l == nil {
		return nil
	}
	return l.activeLockXML(h.href(l.path, false))
}

// activeLockXML renders one activelock element. lockroot is the href the
// client should use for the locked resource, which only the Handler knows
// because only it knows the prefix it is mounted under.
func (l *lockInfo) activeLockXML(lockroot string) []byte {
	var b bytes.Buffer
	b.WriteString(`<activelock xmlns="DAV:">`)
	b.WriteString(`<locktype><write/></locktype>`)
	b.WriteString(`<lockscope><exclusive/></lockscope>`)
	b.WriteString(`<depth>` + depthHeader(l.depth) + `</depth>`)
	if len(l.owner) > 0 {
		b.WriteString(`<owner>`)
		b.Write(l.owner)
		b.WriteString(`</owner>`)
	}
	b.WriteString(`<timeout>Second-` + strconv.Itoa(int(l.timeout/time.Second)) + `</timeout>`)
	b.WriteString(`<locktoken><href>`)
	b.Write(textValue(l.token))
	b.WriteString(`</href></locktoken>`)
	b.WriteString(`<lockroot><href>`)
	b.Write(textValue(lockroot))
	b.WriteString(`</href></lockroot>`)
	b.WriteString(`</activelock>`)
	return b.Bytes()
}

// depthHeader renders a depth the way RFC 4918 spells it on the wire.
func depthHeader(d int) string {
	if d == depthInfinity {
		return "infinity"
	}
	return "0"
}

// ---------------------------------------------------------------------------
// The If header
// ---------------------------------------------------------------------------

// submittedTokens extracts the lock tokens an If header submits.
//
// The full If grammar (RFC 4918 §10.4) is a list of conditions over resources
// and entity tags. This server evaluates the part that matters for locking —
// the state tokens — and does not use If to make a request conditional on an
// entity tag; a client wanting that has If-Match, which net/http already
// honours on GET. That limit is stated rather than hidden, because silently
// ignoring an [etag] condition would make a conditional write unconditional.
//
// A token that appears after "Not" is deliberately *not* collected: "Not
// <token>" asserts the resource is not locked with it, so treating it as a
// submission would grant exactly the access the client said it did not have.
func submittedTokens(header string) []string {
	var out []string
	not := false
	for i := 0; i < len(header); {
		switch {
		case header[i] == '<':
			end := strings.IndexByte(header[i:], '>')
			if end < 0 {
				// Unterminated: nothing after it can be trusted to be a
				// token either, so parsing stops.
				return out
			}
			tok := header[i+1 : i+end]
			if !not {
				out = append(out, tok)
			}
			not = false
			i += end + 1
		case strings.HasPrefix(header[i:], "Not"):
			not = true
			i += 3
		case header[i] == '[':
			// An entity-tag condition. Skipped whole so its contents cannot
			// be mistaken for a token.
			end := strings.IndexByte(header[i:], ']')
			if end < 0 {
				return out
			}
			i += end + 1
		default:
			i++
		}
	}
	return out
}

// checkLocks reports whether a mutating request may touch name.
func (h *Handler) checkLocks(r *http.Request, name string) error {
	return h.locks.confirm(name, submittedTokens(r.Header.Get("If")))
}

// writeLockError maps a lock failure to its wire status.
func writeLockError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrLocked):
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(StatusLocked)
		// RFC 4918 §16 defines the precondition name, which is what tells a
		// client the 423 is about a lock it does not hold rather than a
		// generic refusal.
		_, _ = io.WriteString(w, xml.Header+
			`<error xmlns="DAV:"><lock-token-submitted/></error>`+"\n")
	case errors.Is(err, ErrNoSuchLock):
		http.Error(w, "webdav: "+err.Error(), http.StatusConflict)
	default:
		http.Error(w, "webdav: "+err.Error(), http.StatusInternalServerError)
	}
}

// ---------------------------------------------------------------------------
// LOCK and UNLOCK
// ---------------------------------------------------------------------------

// parseTimeout reads the Timeout header, clamped to [maxLockTimeout].
//
// "Infinite" is accepted and granted as maxLockTimeout rather than refused:
// RFC 4918 §10.7 lets a server grant less than was asked for, and an
// infinite lock in a table that no client is obliged to clean up is a
// resource nobody can ever release.
func parseTimeout(v string) time.Duration {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if strings.EqualFold(part, "Infinite") {
			return maxLockTimeout
		}
		if !strings.HasPrefix(strings.ToLower(part), "second-") {
			continue
		}
		n, err := strconv.Atoi(part[len("Second-"):])
		if err != nil || n <= 0 {
			continue
		}
		if d := time.Duration(n) * time.Second; d < maxLockTimeout {
			return d
		}
		return maxLockTimeout
	}
	return defaultLockTimeout
}

// lockRequest is the parsed body of a LOCK.
type lockRequest struct {
	// refresh is true when the body was empty, which RFC 4918 §9.10.2 makes
	// a refresh of the lock named in the If header rather than a new one.
	refresh bool
	owner   []byte
}

// parseLock decodes a LOCK body.
//
// The owner element is re-encoded through an XML encoder rather than copied
// as raw inner XML. Copying is what most servers do and it is a latent bug:
// the client's owner may use namespace prefixes declared on an ancestor that
// is not being copied with it, so the bytes that were well-formed in the
// request are not well-formed in the response, and the client that reads
// lockdiscovery back gets a parse error from its own data. Re-encoding
// resolves every prefix into the element it is written into.
func parseLock(r io.Reader) (lockRequest, error) {
	dec := xml.NewDecoder(io.LimitReader(r, maxPropfindBody))
	var req lockRequest
	var seenRoot bool
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return lockRequest{}, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if !seenRoot {
			if start.Name.Space != davNS || start.Name.Local != "lockinfo" {
				return lockRequest{}, errBadBody
			}
			seenRoot = true
			continue
		}
		if start.Name.Space == davNS && start.Name.Local == "owner" {
			owner, err := reencode(dec, start)
			if err != nil {
				return lockRequest{}, err
			}
			req.owner = owner
			continue
		}
		// lockscope and locktype are read for well-formedness and then
		// dropped: this server grants exclusive write locks and nothing
		// else, and supportedlock already told the client so.
		if err := dec.Skip(); err != nil {
			return lockRequest{}, err
		}
	}
	if !seenRoot {
		req.refresh = true
	}
	return req, nil
}

// reencode reads the children of start and writes them back out through an
// XML encoder, producing bytes that are well-formed on their own.
func reencode(dec *xml.Decoder, start xml.StartElement) ([]byte, error) {
	var b bytes.Buffer
	enc := xml.NewEncoder(&b)
	depth := 0
	// The encoder's errors are dropped deliberately. EncodeToken fails on a
	// writer error, on an end element that does not match the open one, and
	// on a few tokens that cannot appear here (a nested XML declaration, a
	// comment carrying a "-->"). The writer is a [bytes.Buffer], which has
	// no error; the tokens come from a decoder that has already balanced
	// them; and comments, processing instructions and directives are dropped
	// below rather than forwarded. Checking would add branches no request
	// could execute.
	for {
		tok, err := dec.Token()
		if err != nil {
			// Including io.EOF: an owner element that never ends is a
			// truncated body, not an empty owner.
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			_ = enc.EncodeToken(t)
		case xml.EndElement:
			if depth == 0 && t.Name == start.Name {
				_ = enc.Flush()
				return b.Bytes(), nil
			}
			depth--
			_ = enc.EncodeToken(t)
		case xml.CharData:
			_ = enc.EncodeToken(t)
		}
	}
}

// serveLock answers LOCK.
func (h *Handler) serveLock(w http.ResponseWriter, r *http.Request, name string) {
	depth, ok := parseDepth(r.Header.Get("Depth"))
	if !ok || depth == depthOne {
		// RFC 4918 §9.10.3: LOCK takes 0 or infinity.
		http.Error(w, "webdav: LOCK accepts Depth 0 or infinity", http.StatusBadRequest)
		return
	}
	req, err := parseLock(r.Body)
	if err != nil {
		http.Error(w, "webdav: malformed LOCK body", StatusUnprocessable)
		return
	}
	timeout := parseTimeout(r.Header.Get("Timeout"))

	if req.refresh {
		l, err := h.locks.refresh(name, submittedTokens(r.Header.Get("If")), timeout)
		if err != nil {
			// RFC 4918 §9.10.6: a refresh naming no live lock is 412, since
			// the If header is a precondition that did not hold.
			http.Error(w, "webdav: no lock to refresh", http.StatusPreconditionFailed)
			return
		}
		h.writeLockResponse(w, l, http.StatusOK)
		return
	}

	// A LOCK on a resource that does not exist creates an empty one and
	// answers 201 (RFC 4918 §7.3). This is not a curiosity: it is how every
	// client that locks before writing gets a lock for a file it is about to
	// create, and refusing it is another way to end up mounted read-only.
	created := false
	h.fsmu.Lock()
	if _, statErr := h.fs.Stat(name); statErr != nil {
		if !notFound(statErr) {
			h.fsmu.Unlock()
			http.Error(w, "webdav: "+statErr.Error(), statusFor(statErr, http.StatusInternalServerError))
			return
		}
		if writeErr := h.fs.WriteFile(name, nil, defaultFilePerm); writeErr != nil {
			h.fsmu.Unlock()
			http.Error(w, "webdav: "+writeErr.Error(), statusFor(writeErr, http.StatusInternalServerError))
			return
		}
		created = true
	}
	h.fsmu.Unlock()

	l, err := h.locks.create(name, depth, req.owner, timeout)
	if err != nil {
		writeLockError(w, err)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	h.writeLockResponse(w, l, code)
}

// writeLockResponse writes the prop/lockdiscovery body a LOCK answers with.
func (h *Handler) writeLockResponse(w http.ResponseWriter, l *lockInfo, code int) {
	// The Lock-Token header is what the client actually reads the token
	// from; the body carries it too, and a client that reads only one of
	// them must find the same value in either.
	w.Header().Set("Lock-Token", "<"+l.token+">")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, xml.Header+`<prop xmlns="DAV:"><lockdiscovery>`+
		string(l.activeLockXML(h.href(l.path, false)))+`</lockdiscovery></prop>`+"\n")
}

// serveUnlock answers UNLOCK.
func (h *Handler) serveUnlock(w http.ResponseWriter, r *http.Request, name string) {
	raw := strings.TrimSpace(r.Header.Get("Lock-Token"))
	if !strings.HasPrefix(raw, "<") || !strings.HasSuffix(raw, ">") {
		// RFC 4918 §9.11: UNLOCK without a Lock-Token is a bad request, not
		// a silent success.
		http.Error(w, "webdav: UNLOCK requires a Lock-Token header", http.StatusBadRequest)
		return
	}
	if err := h.locks.unlock(name, raw[1:len(raw)-1]); err != nil {
		writeLockError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
