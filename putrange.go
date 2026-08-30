package webdav

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	filesystem "github.com/go-filesystems/interface"
)

// errNotWritableFile reports a driver that cannot write at an offset. It is
// answered 501, never worked around; see [Handler.servePutRange].
var errNotWritableFile = errors.New("webdav: driver does not support writing at an offset")

// contentRange is a parsed Content-Range request header: the half-open byte
// interval [start, end] inclusive, as the header spells it.
type contentRange struct {
	start, end int64
}

// length is the number of bytes the range covers. The header is inclusive at
// both ends, so a single byte is "0-0" and has length 1.
func (c contentRange) length() int64 { return c.end - c.start + 1 }

// parseContentRange parses a Content-Range request header of the form
// "bytes start-end/total", where total may be "*".
//
// It is deliberately strict. Content-Range on a PUT is not part of RFC 4918 —
// it comes from RFC 7231 §4.3.4, which permits a server to reject it — so the
// only reason to accept one is that the client meant something precise by it.
// A header this server half-understood would corrupt a file at an offset
// nobody could reconstruct, which is the one failure mode worse than
// refusing. Anything malformed, reversed, negative, or disagreeing with the
// stated total is rejected rather than repaired.
func parseContentRange(v string) (contentRange, bool) {
	spec, ok := strings.CutPrefix(v, "bytes ")
	if !ok {
		return contentRange{}, false
	}
	rng, total, ok := strings.Cut(spec, "/")
	if !ok {
		return contentRange{}, false
	}
	lo, hi, ok := strings.Cut(rng, "-")
	if !ok {
		return contentRange{}, false
	}
	start, err := strconv.ParseInt(lo, 10, 64)
	if err != nil || start < 0 {
		return contentRange{}, false
	}
	end, err := strconv.ParseInt(hi, 10, 64)
	if err != nil || end < start {
		return contentRange{}, false
	}
	// "*" means the client does not claim to know the final size. Any other
	// value is a claim, and a claim that contradicts the range it decorates
	// is a client bug this server must not act on.
	if total != "*" {
		n, err := strconv.ParseInt(total, 10, 64)
		if err != nil || n <= end {
			return contentRange{}, false
		}
	}
	return contentRange{start: start, end: end}, true
}

// writableAt opens name for positional writing, or reports why it cannot.
// The caller must hold [Handler.fsmu].
//
// The capability is probed on the *File*, not on the Filesystem: interface
// v0.3.0 puts WriteAt/Truncate/Sync on
// [github.com/go-filesystems/interface.WritableFile], which is what an
// [github.com/go-filesystems/interface.Opener]'s File may additionally
// implement. fat32 v0.3.0 does; a driver that does not gets a 501 and not a
// silent fallback.
func (h *Handler) writableAt(name string) (filesystem.WritableFile, error) {
	if h.opener == nil {
		return nil, errNotWritableFile
	}
	f, err := h.opener.OpenFile(name)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, errNilFile
	}
	wf, ok := f.(filesystem.WritableFile)
	if !ok {
		_ = f.Close()
		return nil, errNotWritableFile
	}
	return wf, nil
}

// servePutRange answers a PUT carrying a Content-Range: it replaces a byte
// interval of an existing resource in place, leaving the rest untouched.
//
// # Why this exists at all
//
// This is the one place where the WebDAV write model needs more than
// [github.com/go-filesystems/interface.Filesystem.WriteFile], and it is
// exactly the shape that cripples the NFS server. Without a positional write,
// updating sixteen bytes in the middle of a 4 GiB file means ReadFile, splice,
// WriteFile — 8 GiB of traffic through memory to move 16 bytes, which is why
// go-filesystems/nfs measures 90 kB/s. With
// [github.com/go-filesystems/interface.WritableFile] it is one WriteAt.
//
// So the rule here is: do it properly or not at all. A driver without the
// capability is answered 501 Not Implemented. Quietly falling back to
// read-splice-write would give a client that asked for a cheap partial update
// the most expensive operation the module can perform, and hide the cost
// behind a 204 — the client would have no way to know it had just rewritten
// the file.
//
// # Why it never extends the file
//
// A range PUT patches; it does not append. Writing past the end would have to
// invent the bytes in between, and a driver's zero-fill is not something a
// client asked for. A range whose end lies beyond the current size is
// answered 416, the same status a GET Range gets for an unsatisfiable ask.
func (h *Handler) servePutRange(w http.ResponseWriter, r *http.Request, name string, cr contentRange) {
	h.fsmu.Lock()
	// The resource must already exist: there is nothing to patch a hole in
	// otherwise, and creating one would mean inventing every byte outside the
	// range. RFC 7231 §4.3.4 allows 409 for exactly this.
	info, err := h.info(name)
	if err != nil {
		h.fsmu.Unlock()
		http.Error(w, "webdav: partial PUT on a resource that does not exist: "+err.Error(),
			conflictOr(err))
		return
	}
	if info.isDir {
		h.fsmu.Unlock()
		http.Error(w, "webdav: cannot PUT a range into a collection", http.StatusMethodNotAllowed)
		return
	}
	f, err := h.writableAt(name)
	if err != nil {
		h.fsmu.Unlock()
		if errors.Is(err, errNotWritableFile) {
			http.Error(w, "webdav: this filesystem cannot write at an offset; "+
				"send the whole resource with a plain PUT", http.StatusNotImplemented)
			return
		}
		http.Error(w, "webdav: "+err.Error(), statusFor(err, http.StatusInternalServerError))
		return
	}
	size := f.Size()
	h.fsmu.Unlock()

	if cr.end >= size {
		// Mirrors the GET Range refusal, including the Content-Range that
		// tells the client what the size actually is.
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
		h.closeFile(f)
		http.Error(w, "webdav: range beyond the end of the resource",
			http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// Exactly the range's worth of body is read, and no more: a body longer
	// than the range it declares is a contradiction, not an invitation to
	// write past the end.
	body, err := io.ReadAll(io.LimitReader(http.MaxBytesReader(w, r.Body, h.maxBody), cr.length()))
	if err != nil {
		h.closeFile(f)
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "webdav: request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "webdav: reading request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if int64(len(body)) != cr.length() {
		// A short body would write a prefix of the range and leave the rest
		// stale, with no way for the client to tell which bytes are which.
		h.closeFile(f)
		http.Error(w, "webdav: body shorter than the declared Content-Range",
			http.StatusBadRequest)
		return
	}

	h.fsmu.Lock()
	_, err = f.WriteAt(body, cr.start)
	if err == nil {
		// Sync before reporting success: a 204 that a power cut can retract
		// is a lie, and this is the one call that makes it true.
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	var after resourceInfo
	if err == nil {
		after, _ = h.info(name)
	}
	h.fsmu.Unlock()
	if err != nil {
		http.Error(w, "webdav: "+err.Error(), statusFor(err, http.StatusInternalServerError))
		return
	}
	if after.etag != "" {
		w.Header().Set("ETag", after.etag)
	}
	w.WriteHeader(http.StatusNoContent)
}

// closeFile closes a driver File under the driver lock, discarding the error.
//
// It exists so the refusal paths above read as refusals rather than as
// cleanup. The error is genuinely uninteresting there: the request has
// already failed, nothing was written, and reporting a close failure instead
// of the reason for the refusal would tell the client the wrong thing.
func (h *Handler) closeFile(f filesystem.WritableFile) {
	h.fsmu.Lock()
	_ = f.Close()
	h.fsmu.Unlock()
}
