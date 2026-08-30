package webdav

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// Errors returned by [New].
var (
	// ErrNilFilesystem reports New called with no filesystem. It is caught
	// here because the alternative is a nil dereference on the first request
	// — long after the mistake, in a connection goroutine, with no recover.
	ErrNilFilesystem = errors.New("webdav: nil filesystem")
	// ErrPrefix reports a prefix that is not a clean absolute path without a
	// trailing slash, e.g. "/files".
	ErrPrefix = errors.New("webdav: prefix must be a clean absolute path with no trailing slash")
	// ErrNilVerifier reports an authentication option given no verifier. A
	// nil one would accept every credential, which is worse than no
	// authentication at all because it looks like some.
	ErrNilVerifier = errors.New("webdav: nil credential verifier")
)

// defaultMaxBody bounds a PUT body at 1 GiB.
//
// A bound is not optional here. [github.com/go-filesystems/interface.Filesystem.WriteFile]
// takes a []byte, so a PUT is buffered in memory in full before the driver
// sees it, and an unbounded read from a network peer is an unbounded
// allocation on this side. 1 GiB is far past any artefact the fleet's drivers
// hold and small enough that one hostile client cannot exhaust a machine;
// [WithMaxBody] moves it.
const defaultMaxBody int64 = 1 << 30

// TimeStat is the optional capability a driver's
// [github.com/go-filesystems/interface.Stat] may implement to report a real
// modification time.
//
// No driver in the fleet does today, so every resource this server exports
// currently reports the Handler's start time as its getlastmodified. The
// probe exists so that the day a driver starts reporting mtime, clients get
// real times without a change here — and so that the gap is visible in the
// API instead of buried in a comment. It is spelled exactly as
// [github.com/go-filesystems/nfs.TimeStat] so a driver satisfies both servers
// with one method.
type TimeStat interface {
	ModTime() int64 // seconds since the Unix epoch
}

// Handler serves one [github.com/go-filesystems/interface.Filesystem] over
// WebDAV. It is an ordinary [net/http.Handler]: it opens no listener, holds
// no TLS configuration and reads no file, so a caller composes it with
// [net/http.ServeMux], wraps it in whatever middleware it already has, and
// terminates TLS in its own [net/http.Server].
//
// That is also what makes the intended deployment cheap: one disk image per
// tenant, one Handler per image, either many on one mux or one per process.
// A Handler can reach exactly one Filesystem and nothing else.
//
// The zero value is not usable; call [New].
type Handler struct {
	fs     filesystem.Filesystem
	opener filesystem.Opener // nil when the driver has no random-access capability
	ro     bool
	prefix string

	basic  func(user, pass string) bool
	bearer func(token string) bool
	realm  string
	// insecureAuth allows credentials over a cleartext, non-loopback
	// connection. See [AllowInsecureAuth].
	insecureAuth bool

	maxBody int64
	locks   *lockSystem

	// start is the timestamp reported for every resource until a driver can
	// report a real one. See [TimeStat].
	start time.Time

	// total and avail feed the RFC 4331 quota properties. Zero means
	// "unknown"; see [WithCapacity].
	total, avail uint64

	// fsmu serialises *all* access to the exported filesystem.
	//
	// A go-filesystems driver wraps a single *os.File and is not documented
	// as safe for concurrent use; two overlapping reads would interleave
	// seeks and hand each caller the other's bytes. net/http runs every
	// request in its own goroutine, so this is not theoretical — it is the
	// first thing two browser tabs would hit.
	fsmu sync.Mutex
}

// Option configures a [Handler].
type Option func(*Handler) error

// ReadWrite makes the export writable.
//
// Exports are read-only by default, following
// [github.com/go-filesystems/nfs]: most of what this module is pointed at is
// a forensic or build artefact, and an accidental write to one is
// unrecoverable. A read-only Handler answers every mutating method 403
// Forbidden and does not advertise them in OPTIONS, so a client's own check
// agrees with the server's before it tries.
func ReadWrite() Option { return func(h *Handler) error { h.ro = false; return nil } }

// WithPrefix mounts the export under a URL prefix, e.g. "/files". The prefix
// must be a clean absolute path with no trailing slash.
//
// It exists rather than deferring to [net/http.StripPrefix] because a
// multistatus body quotes absolute hrefs back to the client: a stripped
// prefix would be missing from every href in it, and the client would follow
// them to the wrong place. The Handler therefore has to know its own mount
// point rather than have it hidden from it.
func WithPrefix(prefix string) Option {
	return func(h *Handler) error {
		if prefix == "" {
			return nil
		}
		if prefix[0] != '/' || strings.HasSuffix(prefix, "/") || prefix != cleanPath(prefix) {
			return ErrPrefix
		}
		h.prefix = prefix
		return nil
	}
}

// WithCapacity sets the total and available byte counts reported by the RFC
// 4331 quota properties, which is what a mounted volume shows as its size and
// free space.
//
// It exists because [github.com/go-filesystems/interface.Filesystem] has no
// statfs operation, so this module genuinely cannot know. Rather than invent
// a plausible number — which would make a volume's free-space display
// confidently wrong, and would make a client refuse a write it could actually
// have done — a Handler with no capacity set omits both properties, which
// clients read as "unknown", and the caller who does know (it opened the
// image, so it knows its size) can say so.
func WithCapacity(total, avail uint64) Option {
	return func(h *Handler) error { h.total, h.avail = total, avail; return nil }
}

// WithMaxBody bounds the number of bytes a PUT may carry. See
// [defaultMaxBody] for why there is a bound at all. A value <= 0 restores the
// default rather than removing the bound, because "no limit" is not something
// this server can offer: the body is buffered in full.
func WithMaxBody(n int64) Option {
	return func(h *Handler) error {
		if n <= 0 {
			n = defaultMaxBody
		}
		h.maxBody = n
		return nil
	}
}

// WithBasicAuth requires HTTP Basic credentials that verify accepts.
//
// The verifier is a function rather than a user/password pair on purpose:
// this package must never be the place a credential is written down. Where
// the caller's credentials come from — a keyring, a database, an
// environment the caller controls, a token service — is the caller's
// business, and a callback is the only shape that does not force it into
// this API. Nothing here is read from a fixed location.
//
// The verifier is called with attacker-controlled strings and must compare
// in constant time; [Verify] does that for the common case of a fixed
// credential the caller holds in memory.
//
// Basic sends the password on every request with nothing but base64 over it,
// so a Handler with any authentication configured refuses to read credentials
// from a cleartext, non-loopback connection at all: see [AllowInsecureAuth].
//
// Basic and Bearer may both be configured; a request satisfying either is
// accepted, and the challenge offers both.
func WithBasicAuth(realm string, verify func(user, pass string) bool) Option {
	return func(h *Handler) error {
		if verify == nil {
			return ErrNilVerifier
		}
		h.basic, h.realm = verify, realm
		return nil
	}
}

// WithBearerAuth requires an RFC 6750 bearer token that verify accepts. The
// same rules as [WithBasicAuth] apply: the verifier is supplied by the
// caller, must compare in constant time, and is not consulted over a
// cleartext non-loopback connection.
func WithBearerAuth(realm string, verify func(token string) bool) Option {
	return func(h *Handler) error {
		if verify == nil {
			return ErrNilVerifier
		}
		h.bearer, h.realm = verify, realm
		return nil
	}
}

// AllowInsecureAuth permits credentials over a cleartext, non-loopback
// connection.
//
// Without it, a Handler that has authentication configured answers 426
// Upgrade Required on such a connection instead of reading the Authorization
// header, because a Basic password crosses that link in the clear on every
// single request and a server that accepts it teaches its users to send it.
// Loopback is already exempt without this option — there is no link to
// intercept — and so is any request that arrived over TLS.
//
// It exists for the one honest case: a Handler already behind a TLS
// terminator the caller trusts, reached over a private link. Passing it
// because a certificate was inconvenient is how the password leaks.
func AllowInsecureAuth() Option { return func(h *Handler) error { h.insecureAuth = true; return nil } }

// New returns a Handler exporting fsys, read-only unless [ReadWrite] is
// passed.
//
// It returns an error if an option is malformed, or if the system CSPRNG is
// unavailable — which would make lock tokens guessable, so the server refuses
// to start rather than start insecurely (see lock.go).
func New(fsys filesystem.Filesystem, opts ...Option) (*Handler, error) {
	if fsys == nil {
		return nil, ErrNilFilesystem
	}
	locks, err := newLockSystem()
	if err != nil {
		return nil, err
	}
	h := &Handler{
		fs:      fsys,
		ro:      true,
		maxBody: defaultMaxBody,
		locks:   locks,
		start:   time.Now(),
	}
	// The random-access capability is probed once, here, rather than per
	// request. interface v0.2.0 declares Opener, so this is a plain type
	// assertion: the older reflection probe in go-filesystems/nfs existed
	// only because the capability was not tagged yet, and re-declaring the
	// type locally would have made a driver's real filesystem.File fail to
	// match. It is tagged now, so the honest spelling is available.
	if o, ok := fsys.(filesystem.Opener); ok {
		h.opener = o
	}
	for _, opt := range opts {
		if err := opt(h); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// methods lists what the Handler answers, which is both what OPTIONS
// advertises and what a 405 names in Allow.
func (h *Handler) methods() string {
	read := "OPTIONS, GET, HEAD, PROPFIND"
	if h.ro {
		return read
	}
	return read + ", PUT, DELETE, MKCOL, COPY, MOVE, PROPPATCH, LOCK, UNLOCK"
}

// ServeHTTP implements [net/http.Handler].
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorise(w, r) {
		return
	}
	// Every response advertises the compliance classes, not just OPTIONS.
	// The macOS client reads the DAV header off whichever response it
	// happens to have in hand — commonly the PROPFIND that follows a mount,
	// not the OPTIONS that preceded it — and mounts read-only, or refuses to
	// write, when it does not find class 2 there.
	h.setDAV(w)

	name, ok := h.resource(r.URL.Path)
	if !ok {
		http.Error(w, "webdav: bad request path", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodOptions:
		h.serveOptions(w)
	case http.MethodGet, http.MethodHead:
		h.serveGet(w, r, name)
	case "PROPFIND":
		h.servePropfind(w, r, name)
	case http.MethodPut:
		h.mutate(w, r, name, h.servePut)
	case http.MethodDelete:
		h.mutate(w, r, name, h.serveDelete)
	case "MKCOL":
		h.mutate(w, r, name, h.serveMkcol)
	case "COPY":
		h.mutate(w, r, name, h.serveCopy)
	case "MOVE":
		h.mutate(w, r, name, h.serveMove)
	case "PROPPATCH":
		h.mutate(w, r, name, h.serveProppatch)
	case "LOCK":
		h.mutate(w, r, name, h.serveLock)
	case "UNLOCK":
		h.mutate(w, r, name, h.serveUnlock)
	default:
		w.Header().Set("Allow", h.methods())
		http.Error(w, "webdav: method not allowed", http.StatusMethodNotAllowed)
	}
}

// setDAV writes the compliance-class headers.
//
// Class 1 is the base protocol and class 2 is locking. Advertising 2 is not a
// formality: the macOS client mounts a class-1 server read-only whatever the
// Allow header says, because it will not write without taking a lock first.
// MS-Author-Via is the header the Windows Mini-Redirector looks for to
// decide that this is a WebDAV endpoint rather than a plain web server.
func (h *Handler) setDAV(w http.ResponseWriter) {
	if h.ro {
		w.Header().Set("DAV", "1")
	} else {
		w.Header().Set("DAV", "1, 2")
	}
	w.Header().Set("MS-Author-Via", "DAV")
}

// serveOptions answers OPTIONS.
func (h *Handler) serveOptions(w http.ResponseWriter) {
	w.Header().Set("Allow", h.methods())
	// A zero Content-Length is explicit because some clients treat a missing
	// one on OPTIONS as "read until close" and hang for the idle timeout.
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

// mutate runs a mutating method, refusing it outright on a read-only export.
//
// Doing the check in one place is what makes "read-only" a property of the
// Handler rather than a rule each method has to remember, and 403 rather than
// 405 is deliberate: the method is implemented and would be allowed on a
// writable export, which is exactly what RFC 9110 distinguishes the two by.
func (h *Handler) mutate(w http.ResponseWriter, r *http.Request, name string, fn func(http.ResponseWriter, *http.Request, string)) {
	if h.ro {
		w.Header().Set("Allow", h.methods())
		http.Error(w, "webdav: read-only export", http.StatusForbidden)
		return
	}
	fn(w, r, name)
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

// authorise reports whether the request may proceed, writing the refusal
// itself when it may not.
func (h *Handler) authorise(w http.ResponseWriter, r *http.Request) bool {
	if h.basic == nil && h.bearer == nil {
		return true
	}
	if !h.confidential(r) {
		// The Authorization header is deliberately not even read here: a
		// server that validates a cleartext password and *then* complains
		// has already accepted it once.
		w.Header().Set("Upgrade", "TLS/1.2, HTTP/1.1")
		w.Header().Set("Connection", "Upgrade")
		http.Error(w, "webdav: credentials are not accepted over a cleartext connection; use HTTPS",
			http.StatusUpgradeRequired)
		return false
	}
	if h.credentialsOK(r) {
		return true
	}
	h.challenge(w)
	http.Error(w, "webdav: unauthorized", http.StatusUnauthorized)
	return false
}

// confidential reports whether credentials may be read off this request: over
// TLS, over loopback, or because the caller passed [AllowInsecureAuth].
func (h *Handler) confidential(r *http.Request) bool {
	if h.insecureAuth || r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// A RemoteAddr with no port is not something net/http produces, and
		// an address this code cannot parse is one it must not vouch for.
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// credentialsOK runs the configured verifiers. Both are tried, so a Handler
// offering Basic and Bearer accepts either.
func (h *Handler) credentialsOK(r *http.Request) bool {
	if h.basic != nil {
		if user, pass, ok := r.BasicAuth(); ok && h.basic(user, pass) {
			return true
		}
	}
	if h.bearer != nil {
		const prefix = "Bearer "
		if v := r.Header.Get("Authorization"); len(v) > len(prefix) &&
			strings.EqualFold(v[:len(prefix)], prefix) && h.bearer(v[len(prefix):]) {
			return true
		}
	}
	return false
}

// challenge writes the WWW-Authenticate headers for whichever schemes are
// configured. charset="UTF-8" is RFC 7617's way of saying a non-ASCII
// password is UTF-8 rather than whatever the client guesses.
func (h *Handler) challenge(w http.ResponseWriter) {
	realm := quoteHeader(h.realm)
	if h.basic != nil {
		w.Header().Add("WWW-Authenticate", `Basic realm="`+realm+`", charset="UTF-8"`)
	}
	if h.bearer != nil {
		w.Header().Add("WWW-Authenticate", `Bearer realm="`+realm+`"`)
	}
}

// quoteHeader makes a caller-supplied realm safe to place inside a quoted
// header string. A realm arrives from the caller, not the network, but a
// stray quote or newline in one would split the header, and a header that a
// value can split is a header-injection primitive whoever supplied the value.
func quoteHeader(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			// Dropped rather than escaped: there is no escape for a control
			// character in a quoted-string, and a realm containing one is a
			// mistake, not an intent.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Verify reports whether got equals want without leaking, through timing,
// where the two first differ.
//
// It exists so that the obvious implementation of a [WithBasicAuth] verifier
// is also the correct one. A verifier written with == compares byte by byte
// and stops at the first mismatch, which lets a client recover a secret one
// character at a time; that is a real attack on a network service and an easy
// one to write by accident. It does not hide the length, which no comparison
// can.
func Verify(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
