package webdav

import (
	"errors"
	"io/fs"
	"net/http"
	"strings"

	filesystem "github.com/go-filesystems/interface"
)

// WebDAV extends HTTP's status codes; the ones RFC 4918 adds and this module
// uses are spelled out because net/http has no constants for them.
const (
	// StatusMulti is 207 Multi-Status (RFC 4918 §11.1): the body carries one
	// status per resource.
	StatusMulti = 207
	// StatusUnprocessable is 422 Unprocessable Entity (RFC 4918 §11.2):
	// well-formed XML the server cannot act on.
	StatusUnprocessable = 422
	// StatusLocked is 423 Locked (RFC 4918 §11.3).
	StatusLocked = 423
	// StatusFailedDependency is 424 Failed Dependency (RFC 4918 §11.4): this
	// resource was not touched because another one in the same request
	// failed.
	StatusFailedDependency = 424
	// StatusInsufficientStorage is 507 Insufficient Storage (RFC 4918 §11.5).
	StatusInsufficientStorage = 507
)

// substringStatus maps a fragment of a driver's error text to an HTTP status.
//
// This is a wart, and it is worth being explicit about whose wart it is:
// [github.com/go-filesystems/interface] defines no error taxonomy, so drivers
// report "not found" however they like — iso9660 has typed sentinels that do
// not wrap [io/fs.ErrNotExist], fat32 uses bare fmt.Errorf. A protocol server
// must turn those into distinct wire codes, because a client behaves very
// differently on 404 than on 500.
//
// The table is lifted from [github.com/go-filesystems/nfs] deliberately: the
// two servers face the same drivers, and one shared list of driver phrasings
// is easier to keep true than two that drift. The mitigation is the same
// there and here — this is a *last* resort. Sentinels are tried first, and
// every method that can afford to establishes existence and type with an
// explicit Stat rather than inferring them from an error string. The real fix
// belongs upstream: sentinel errors in the interface module that every driver
// wraps.
var substringStatus = []struct {
	frag   string
	status int
}{
	{"not found", http.StatusNotFound},
	{"no such", http.StatusNotFound},
	{"does not exist", http.StatusNotFound},
	{"not a directory", http.StatusConflict},
	{"is a directory", http.StatusMethodNotAllowed},
	{"not a regular file", http.StatusMethodNotAllowed},
	{"not a symbolic link", http.StatusMethodNotAllowed},
	{"not empty", http.StatusConflict},
	{"read-only", http.StatusForbidden},
	{"already exists", http.StatusMethodNotAllowed},
	{"no space", StatusInsufficientStorage},
	{"too many", StatusInsufficientStorage},
}

// statusFor maps a driver error to an HTTP status, using fallback when
// nothing matches. err must not be nil: every caller reaches here holding a
// failure, and a nil guard would be a branch no request could execute.
func statusFor(err error, fallback int) int {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return http.StatusNotFound
	case errors.Is(err, fs.ErrExist):
		return http.StatusMethodNotAllowed
	case errors.Is(err, fs.ErrPermission):
		return http.StatusForbidden
	case errors.Is(err, fs.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, filesystem.ErrShrinkUnsupported):
		return http.StatusNotImplemented
	}
	low := strings.ToLower(err.Error())
	for _, m := range substringStatus {
		if strings.Contains(low, m.frag) {
			return m.status
		}
	}
	return fallback
}

// notFound reports whether a driver error means "no such path". It is the one
// distinction the write methods need before they act — MKCOL, PUT and MOVE
// all behave differently on a missing parent than on a broken one — and
// asking it as a question keeps the substring table in exactly one place.
func notFound(err error) bool {
	return statusFor(err, http.StatusInternalServerError) == http.StatusNotFound
}
