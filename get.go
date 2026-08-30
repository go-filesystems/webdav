package webdav

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"

	filesystem "github.com/go-filesystems/interface"
)

// serveGet answers GET and HEAD.
//
// A plain GET on a file returns its bytes and nothing else — no wrapper, no
// XML, no redirect. That is the property that makes a browser, `curl` and
// every HTTP library a client of this server without mounting anything, and
// it is worth protecting deliberately rather than assuming.
//
// GET on a *collection* returns an HTML index. RFC 4918 leaves the behaviour
// undefined; a listing is what makes an export browsable, and it is generated
// rather than served from the image so it cannot collide with a real file.
func (h *Handler) serveGet(w http.ResponseWriter, r *http.Request, name string) {
	h.fsmu.Lock()
	info, err := h.info(name)
	if err != nil {
		h.fsmu.Unlock()
		http.Error(w, "webdav: "+err.Error(), statusFor(err, http.StatusInternalServerError))
		return
	}
	if info.isDir {
		defer h.fsmu.Unlock()
		h.serveIndex(w, r, name)
		return
	}

	// The content is opened under the lock, then served without it: the
	// reader handed to ServeContent takes the lock per read (see
	// lockedReaderAt), so a slow client cannot hold the driver hostage while
	// it trickles a body down a 3G link.
	content, closer, err := h.open(info)
	h.fsmu.Unlock()
	if err != nil {
		http.Error(w, "webdav: "+err.Error(), statusFor(err, http.StatusInternalServerError))
		return
	}
	defer closer()

	w.Header().Set("ETag", info.etag)
	// Advertising range support explicitly matters for a client deciding
	// whether it may resume: net/http sets Accept-Ranges only once it has
	// decided to honour a range, which is too late to be discovered by a
	// HEAD.
	w.Header().Set("Accept-Ranges", "bytes")

	// ServeContent is what answers Range, If-Range, If-None-Match,
	// If-Modified-Since and HEAD, and emits 206 with a correct Content-Range
	// or 416 with "bytes */size". Reimplementing byte-range arithmetic that
	// the standard library already gets right — including multipart ranges
	// and the off-by-one at the end of the file — would be a way to be
	// subtly wrong for no gain.
	http.ServeContent(w, r, info.name, info.modTime, content)
}

// open returns a seekable view of a file's bytes, plus the release function
// for whatever it holds. The caller must hold [Handler.fsmu].
//
// Two paths, and which one a driver gets is the difference between serving a
// 4 GiB image and refusing to:
//
//   - A driver implementing [github.com/go-filesystems/interface.Opener] is
//     read at the offset the client asked for. A Range request for 4 KiB
//     costs 4 KiB.
//   - A driver without it is read through ReadFile, which materialises the
//     *entire* file. Correct, and the reason a Range request against a
//     squashfs or oci export costs the whole file: the capability is the
//     fix, not a workaround here.
func (h *Handler) open(info resourceInfo) (io.ReadSeeker, func(), error) {
	if h.opener != nil {
		f, err := h.opener.OpenFile(info.path)
		if err != nil {
			return nil, nil, err
		}
		if f == nil {
			// A driver returning (nil, nil) is a bug this process must not
			// turn into a nil dereference in a request goroutine.
			return nil, nil, errNilFile
		}
		return io.NewSectionReader(&lockedReaderAt{h: h, f: f}, 0, f.Size()), func() { _ = f.Close() }, nil
	}
	data, err := h.fs.ReadFile(info.path)
	if err != nil {
		return nil, nil, err
	}
	return bytes.NewReader(data), func() {}, nil
}

// lockedReaderAt takes the driver lock around each read.
//
// It is what lets the body be written outside the lock. [io.SectionReader]
// calls ReadAt once per buffer refill, so the lock is held for one driver
// read at a time rather than for the whole transfer — which is the
// difference between one slow client and every client waiting for it.
type lockedReaderAt struct {
	h *Handler
	f filesystem.File
}

func (l *lockedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	l.h.fsmu.Lock()
	defer l.h.fsmu.Unlock()
	return l.f.ReadAt(p, off)
}

// serveIndex writes an HTML listing of a collection. The caller must hold
// [Handler.fsmu].
func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request, name string) {
	entries, err := h.fs.ListDir(name)
	if err != nil {
		http.Error(w, "webdav: "+err.Error(), statusFor(err, http.StatusInternalServerError))
		return
	}
	var b bytes.Buffer
	b.WriteString("<!doctype html>\n<meta charset=\"utf-8\">\n<title>")
	writeHTML(&b, name)
	b.WriteString("</title>\n<h1>")
	writeHTML(&b, name)
	b.WriteString("</h1>\n<ul>\n")
	if name != "/" {
		b.WriteString(`<li><a href="`)
		writeHTML(&b, h.href(parentOf(name), true))
		b.WriteString("\">../</a></li>\n")
	}
	for _, e := range entries {
		n := e.Name()
		if n == "." || n == ".." {
			continue
		}
		child := join(name, n)
		info, err := h.info(child)
		// A member whose Stat fails is listed as a plain link rather than
		// omitted: a file that cannot be described can still be fetched, and
		// silently dropping it would make the listing lie.
		isDir := err == nil && info.isDir
		b.WriteString(`<li><a href="`)
		writeHTML(&b, h.href(child, isDir))
		b.WriteString(`">`)
		writeHTML(&b, n)
		if isDir {
			b.WriteString("/")
		}
		b.WriteString("</a>")
		if err == nil && !isDir {
			b.WriteString(" <small>" + strconv.FormatUint(info.size, 10) + " bytes</small>")
		}
		b.WriteString("</li>\n")
	}
	b.WriteString("</ul>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(b.Bytes())
}

// writeHTML escapes text for an HTML document.
//
// It is spelled out rather than taken from html/template because the listing
// is a handful of elements, and because the one thing that must be true —
// that a file named `"><script>` in someone's disk image cannot become
// markup — is easier to see in six cases than through a template engine.
func writeHTML(b *bytes.Buffer, s string) {
	const escaped = `<>&"'`
	for {
		i := strings.IndexAny(s, escaped)
		if i < 0 {
			b.WriteString(s)
			return
		}
		b.WriteString(s[:i])
		switch s[i] {
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		case '"':
			b.WriteString("&#34;")
		default:
			b.WriteString("&#39;")
		}
		s = s[i+1:]
	}
}
