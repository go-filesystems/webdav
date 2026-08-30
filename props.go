package webdav

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"mime"
	"net/http"
	"path"
	"strconv"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// POSIX file-type bits, as they appear in a driver's
// [github.com/go-filesystems/interface.Stat.Mode].
const (
	sIFMT  = 0o170000
	sIFDIR = 0o040000
	sIFLNK = 0o120000
)

// resourceInfo is everything one PROPFIND response row needs, gathered in a
// single Stat so that rendering cannot go back to the driver and cannot fail.
type resourceInfo struct {
	path    string
	name    string
	isDir   bool
	isLink  bool
	size    uint64
	modTime time.Time
	etag    string
}

// info builds a resourceInfo for one path. The caller must hold [Handler.fsmu].
func (h *Handler) info(p string) (resourceInfo, error) {
	st, err := h.fs.Stat(p)
	if err != nil {
		return resourceInfo{}, err
	}
	if st == nil {
		// A driver returning (nil, nil) is a bug, but it is *this* process
		// that would take the nil dereference, in a per-request goroutine
		// whose panic net/http converts into a dropped connection for every
		// other request on it. It is answered as a server error instead.
		return resourceInfo{}, errNilStat
	}
	return h.infoFromStat(p, st), nil
}

// infoFromStat converts a driver Stat into a resourceInfo.
func (h *Handler) infoFromStat(p string, st filesystem.Stat) resourceInfo {
	mode := st.Mode()
	r := resourceInfo{
		path:    p,
		name:    path.Base(p),
		isDir:   mode&sIFMT == sIFDIR,
		isLink:  mode&sIFMT == sIFLNK,
		size:    st.Size(),
		modTime: h.start,
	}
	if p == "/" {
		// path.Base("/") is "/", which is not a display name.
		r.name = ""
		r.isDir = true
	}
	// Timestamps. No driver in the fleet reports one yet, so every resource
	// carries the Handler's start time. That is visibly wrong in a mounted
	// volume's Date Modified column and it is reported here rather than
	// hidden: the fix is a timestamp accessor on interface.Stat, not a guess
	// in this module. The probe picks one up the day it exists.
	if t, ok := st.(TimeStat); ok {
		r.modTime = time.Unix(t.ModTime(), 0).UTC()
	}
	r.etag = etagFor(r)
	return r
}

// etagFor derives a strong entity tag from the metadata the driver reports.
//
// It has to be derived rather than stored, because no driver records one. The
// inputs are the ones that change when the body changes — path, size, mtime —
// and they are hashed rather than concatenated so that the tag discloses
// neither the path nor the size to a client that only asked for a validator.
//
// Its limit is worth stating: with no real mtime from the driver, two
// different bodies of the same length at the same path hash the same, so a
// cached copy can go stale. That is a consequence of the missing timestamp,
// not of the hash, and it resolves itself the day [TimeStat] is satisfied.
func etagFor(r resourceInfo) string {
	sum := sha256.Sum256([]byte(r.path + "\x00" +
		strconv.FormatUint(r.size, 10) + "\x00" +
		strconv.FormatInt(r.modTime.UnixNano(), 10)))
	return `"` + hex.EncodeToString(sum[:12]) + `"`
}

// collectionType is what a collection reports as its getcontenttype. It is
// not a real media type — there is no body to have one — but it is what
// WebDAV servers have sent since mod_dav and what clients key their folder
// icon off.
const collectionType = "httpd/unix-directory"

// contentTypeOf names a file's media type from its extension only.
//
// Sniffing the body would mean reading it, and PROPFIND on a directory of a
// thousand files would then read a thousand files to answer a question about
// their names. A GET is different — [net/http.ServeContent] sniffs there,
// where the bytes are in hand anyway — so the two can disagree, and the GET
// is the one to believe.
func contentTypeOf(r resourceInfo) string {
	if r.isDir {
		return collectionType
	}
	if ct := mime.TypeByExtension(path.Ext(r.name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// ---------------------------------------------------------------------------
// The multistatus document
// ---------------------------------------------------------------------------

// davNS is the one XML namespace RFC 4918 defines. Every element this module
// emits is in it.
const davNS = "DAV:"

// property is one property in a propstat. The value is carried as raw inner
// XML because a property's content is arbitrary XML, not a string: every
// value written into one here is produced by [textValue] (which escapes) or
// by this package's own generators (which emit elements), so the field never
// carries anything a client wrote verbatim.
type property struct {
	XMLName  xml.Name
	InnerXML []byte `xml:",innerxml"`
}

// propBag is the <prop> element wrapping a propstat's properties.
//
// It is a named type with its own XMLName rather than a `prop>...` path on
// the field, because each property element carries its own name in its
// XMLName and [encoding/xml] resolves a field's element name from the value
// when the value has one — so a path tag would name the children, not the
// wrapper, and the <prop> element would vanish.
type propBag struct {
	XMLName xml.Name `xml:"DAV: prop"`
	Props   []property
}

// propstat groups the properties that share one status. RFC 4918 requires the
// grouping: a PROPFIND naming five properties of which two are missing
// answers one 200 propstat and one 404 propstat, not five responses.
type propstat struct {
	XMLName xml.Name `xml:"DAV: propstat"`
	Prop    propBag
	Status  string `xml:"DAV: status"`
}

// newPropstat groups props under one status.
func newPropstat(code int, props ...property) propstat {
	return propstat{Prop: propBag{Props: props}, Status: statusText(code)}
}

// response is one <response> row: either a set of propstats (PROPFIND,
// PROPPATCH) or a bare status (DELETE, COPY, MOVE reporting a member that
// failed).
type response struct {
	XMLName  xml.Name `xml:"DAV: response"`
	Href     string   `xml:"DAV: href"`
	Propstat []propstat
	Status   string `xml:"DAV: status,omitempty"`
}

// multistatus is the 207 body.
type multistatus struct {
	XMLName   xml.Name   `xml:"DAV: multistatus"`
	Responses []response `xml:"DAV: response"`
}

// statusText renders a status line the way RFC 4918 §14.28 requires, which is
// an HTTP status-line and not just a number.
func statusText(code int) string {
	return "HTTP/1.1 " + strconv.Itoa(code) + " " + http.StatusText(code)
}

// textValue escapes a string for use as a property's content.
func textValue(s string) []byte {
	var b bytes.Buffer
	// xml.EscapeText's only error comes from the writer, and a bytes.Buffer
	// has none; ignoring it here is what lets callers stay expression-shaped.
	_ = xml.EscapeText(&b, []byte(s))
	return b.Bytes()
}

// prop builds a property in the DAV: namespace.
func prop(name string, inner []byte) property {
	return property{XMLName: xml.Name{Space: davNS, Local: name}, InnerXML: inner}
}

// writeMultistatus sends a 207 body.
//
// The XML declaration is written by hand because [encoding/xml] does not emit
// one, and a client that reads the response with a strict parser configured
// for a specific encoding needs it.
func writeMultistatus(w http.ResponseWriter, ms multistatus) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(StatusMulti)
	var b bytes.Buffer
	b.WriteString(xml.Header)
	enc := xml.NewEncoder(&b)
	// The encoder's errors are dropped rather than checked, and that is a
	// decision rather than an oversight: [encoding/xml.Encoder.Encode] fails
	// only on a writer error or on a value it cannot marshal, and here the
	// writer is a [bytes.Buffer] — which has no error — and the value is
	// built entirely out of this package's own types, every field of which
	// is a string or a slice of them. Checking would add two branches no
	// request could ever execute, which is worse than saying so here.
	_ = enc.Encode(ms)
	_ = enc.Flush()
	// A short write means the client went away mid-body, which is not this
	// server's problem to report to anyone: the status line is already gone.
	_, _ = w.Write(b.Bytes())
}
