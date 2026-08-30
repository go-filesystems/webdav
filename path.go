package webdav

import (
	"net/url"
	"strings"
)

// maxPath bounds a resolved path. It is not a protocol limit — HTTP has none
// — but an unbounded name is an unbounded allocation in every driver
// downstream, and no filesystem in the fleet accepts anything near this.
const maxPath = 4096

// cleanPath normalises an absolute path: exactly one leading slash, no
// duplicate slashes, no "." or ".." components, no trailing slash except at
// the root.
//
// It is written out rather than delegated to [path.Clean] for one reason that
// matters more here than anywhere else: ".." must be *clamped* at the root
// rather than escaping it, and path.Clean leaves a leading ".." in place. A
// WebDAV URL is precisely where a client tries to escape, and it can spell
// the attempt three ways — "..", "%2e%2e", and a doubled slash hiding an
// empty component. All three arrive here as the same thing, because
// [net/url] has already percent-decoded the path by the time it reaches a
// handler, and this function then drops the empty components and consumes
// the "..". The result can never name anything above the root.
//
// This is the containment guarantee the whole module rests on: a Handler
// reaches exactly one Filesystem, and within it only paths this function
// produced.
func cleanPath(p string) string {
	var out []string
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, part)
		}
	}
	return "/" + strings.Join(out, "/")
}

// parentOf returns the containing directory, clamped at the root.
func parentOf(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

// join appends one component to a directory path. Both sides have already
// been through [cleanPath], so this is concatenation, not resolution.
func join(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// resource turns a request URL path into a filesystem path inside the export.
//
// The prefix the Handler is mounted under is stripped first, and a request
// that does not carry it is rejected: serving it from the root instead would
// make the same file reachable under two names and would let a
// [net/http.ServeMux] misconfiguration silently widen the export.
//
// A NUL is refused outright. It cannot appear in a URL that a conforming
// client wrote, and letting one through would mean the name this server logs
// and the name a C consumer downstream sees are different strings.
func (h *Handler) resource(p string) (string, bool) {
	if strings.ContainsRune(p, 0) {
		return "", false
	}
	if h.prefix != "" {
		switch {
		case p == h.prefix:
			p = "/"
		case strings.HasPrefix(p, h.prefix+"/"):
			p = p[len(h.prefix):]
		default:
			return "", false
		}
	}
	c := cleanPath(p)
	if len(c) > maxPath {
		return "", false
	}
	return c, true
}

// href renders a filesystem path as the URL a client should use for it,
// percent-encoded and carrying the Handler's prefix back.
//
// Encoding is done per component with [net/url.URL.EscapedPath] rather than
// with [net/url.PathEscape], because PathEscape escapes "/" as well and would
// turn a path into a single opaque segment. A collection's href ends in "/":
// clients — the macOS one especially — use the trailing slash to decide
// whether a resource can be descended into, before they have read
// resourcetype.
func (h *Handler) href(p string, collection bool) string {
	u := &url.URL{Path: h.prefix + p}
	s := u.EscapedPath()
	if collection && !strings.HasSuffix(s, "/") {
		s += "/"
	}
	return s
}

// under reports whether p is parent, or lies beneath it.
//
// It exists because the obvious spelling, strings.HasPrefix(p, parent+"/"),
// is wrong at exactly one path and wrong in the dangerous direction: for the
// root it tests against "//", which nothing matches, so the root appears to
// contain nothing. A COPY of "/" into a new child then passed the
// "destination is inside the source" check, created the child, listed the
// root again — now containing it — and recursed until the process died. One
// helper, used by every containment question in the module, is the fix.
func under(parent, p string) bool {
	if parent == "/" || p == parent {
		return true
	}
	return strings.HasPrefix(p, parent+"/")
}

// strictlyUnder is [under] excluding the parent itself.
func strictlyUnder(parent, p string) bool { return p != parent && under(parent, p) }
