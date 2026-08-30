package webdav

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// errNilFile reports a driver whose OpenFile returned (nil, nil).
var errNilFile = errors.New("webdav: driver OpenFile returned a nil File with no error")

// defaultPerm is the mode a newly created file or collection is given.
//
// WebDAV has no way to express one — there is no mode in the protocol — so
// the choice is the server's. 0644/0755 is what a POSIX umask of 022 would
// have produced, which is what a client that later reads the mode back
// expects to see.
const (
	defaultFilePerm os.FileMode = 0o644
	defaultDirPerm  os.FileMode = 0o755
)

// servePut answers PUT.
//
// This is the method that WebDAV gets right where NFS cannot. PUT carries the
// whole body, which is exactly
// [github.com/go-filesystems/interface.Filesystem.WriteFile]: one call, one
// write of one file, at whatever speed the driver manages. NFS WRITE names an
// offset, and with no positional write in the contract its server has to
// read-modify-write the whole file per request — O(filesize) each — which is
// why go-filesystems/nfs measures 90 kB/s where this measures the driver's
// own throughput.
//
// The body is buffered in full before the driver sees it, because WriteFile
// takes a []byte. That is bounded by [WithMaxBody] rather than left to the
// client, since an unbounded read from a network peer is an unbounded
// allocation here. Buffering also buys a property worth keeping: a body that
// dies mid-upload writes nothing at all, where a streamed write would leave
// half a file under the name the client believes it just uploaded.
//
// A PUT carrying a Content-Range is a partial update instead, handled by
// [Handler.servePutRange] against the positional-write capability.
func (h *Handler) servePut(w http.ResponseWriter, r *http.Request, name string) {
	if name == "/" {
		http.Error(w, "webdav: cannot PUT the root collection", http.StatusMethodNotAllowed)
		return
	}
	if err := h.checkLocks(r, name); err != nil {
		writeLockError(w, err)
		return
	}
	// A Content-Range turns PUT into a partial update — a different operation
	// with different preconditions, patching an existing resource rather than
	// replacing one — so it is dispatched before any of the whole-body
	// machinery below runs. An unparseable one is refused rather than
	// ignored: treating it as a plain PUT would overwrite the entire resource
	// with what the client meant as a fragment of it.
	if v := r.Header.Get("Content-Range"); v != "" {
		cr, ok := parseContentRange(v)
		if !ok {
			http.Error(w, "webdav: malformed Content-Range", http.StatusBadRequest)
			return
		}
		h.servePutRange(w, r, name, cr)
		return
	}
	// RFC 4918 §9.7.1: PUT does not create intermediate collections, and a
	// PUT whose parent is missing is 409 Conflict — not 404, which a client
	// would read as "the file you asked about is gone".
	h.fsmu.Lock()
	parent, err := h.info(parentOf(name))
	if err != nil {
		h.fsmu.Unlock()
		http.Error(w, "webdav: parent collection: "+err.Error(), conflictOr(err))
		return
	}
	if !parent.isDir {
		h.fsmu.Unlock()
		http.Error(w, "webdav: parent is not a collection", http.StatusConflict)
		return
	}
	// Whether the resource already existed decides 201 vs 204, and it has to
	// be asked before the write rather than inferred after it.
	_, statErr := h.fs.Stat(name)
	existed := statErr == nil
	h.fsmu.Unlock()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "webdav: request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		// A truncated body must not be written: half a file at the client's
		// chosen name is worse than no file, because the client believes the
		// upload succeeded once it sees any 2xx.
		http.Error(w, "webdav: reading request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	h.fsmu.Lock()
	err = h.fs.WriteFile(name, body, defaultFilePerm)
	var info resourceInfo
	if err == nil {
		info, _ = h.info(name)
	}
	h.fsmu.Unlock()
	if err != nil {
		http.Error(w, "webdav: "+err.Error(), statusFor(err, http.StatusInternalServerError))
		return
	}
	if info.etag != "" {
		w.Header().Set("ETag", info.etag)
	}
	if existed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Location", h.href(name, false))
	w.WriteHeader(http.StatusCreated)
}

// conflictOr maps a failed parent lookup: a missing parent is 409 Conflict
// per RFC 4918, anything else keeps its own status.
func conflictOr(err error) int {
	if notFound(err) {
		return http.StatusConflict
	}
	return statusFor(err, http.StatusInternalServerError)
}

// serveMkcol answers MKCOL.
func (h *Handler) serveMkcol(w http.ResponseWriter, r *http.Request, name string) {
	if err := h.checkLocks(r, name); err != nil {
		writeLockError(w, err)
		return
	}
	// RFC 4918 §9.3: a MKCOL with a body it does not understand is 415, not
	// a silently ignored body. Extended MKCOL (RFC 5689) is not implemented,
	// so every body is one it does not understand.
	if n, err := io.CopyN(io.Discard, r.Body, 1); err == nil && n == 1 {
		http.Error(w, "webdav: MKCOL with a request body is not supported", http.StatusUnsupportedMediaType)
		return
	}
	h.fsmu.Lock()
	defer h.fsmu.Unlock()
	if _, err := h.fs.Stat(name); err == nil {
		http.Error(w, "webdav: resource already exists", http.StatusMethodNotAllowed)
		return
	}
	parent, err := h.info(parentOf(name))
	if err != nil {
		http.Error(w, "webdav: parent collection: "+err.Error(), conflictOr(err))
		return
	}
	if !parent.isDir {
		http.Error(w, "webdav: parent is not a collection", http.StatusConflict)
		return
	}
	if err := h.fs.MkDir(name, defaultDirPerm); err != nil {
		http.Error(w, "webdav: "+err.Error(), statusFor(err, http.StatusInternalServerError))
		return
	}
	w.Header().Set("Location", h.href(name, true))
	w.WriteHeader(http.StatusCreated)
}

// serveDelete answers DELETE.
//
// DELETE on a collection is depth-infinity by definition (RFC 4918 §9.6.1):
// there is no way to ask for a shallow one, so the recursion is not optional.
// [github.com/go-filesystems/interface.Filesystem] has DeleteDir, which
// requires the directory to be empty, so the walk is done here.
func (h *Handler) serveDelete(w http.ResponseWriter, r *http.Request, name string) {
	if name == "/" {
		// Deleting the export root would ask the driver to remove the image's
		// own root directory. Refusing is not a limitation: there is no
		// resource above it for the result to be a member of.
		http.Error(w, "webdav: cannot DELETE the root collection", http.StatusForbidden)
		return
	}
	if err := h.checkLocks(r, name); err != nil {
		writeLockError(w, err)
		return
	}
	h.fsmu.Lock()
	defer h.fsmu.Unlock()
	info, err := h.info(name)
	if err != nil {
		http.Error(w, "webdav: "+err.Error(), statusFor(err, http.StatusInternalServerError))
		return
	}
	var failures []response
	h.removeTree(info, &failures)
	if len(failures) == 0 {
		h.locks.releaseTree(name)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// RFC 4918 §9.6.1: when some members could not be removed the response
	// is a 207 naming them, and the resources that *were* removed are not
	// listed — a client learns what is left, not what is gone.
	writeMultistatus(w, multistatus{Responses: failures})
}

// removeTree deletes a resource and everything under it, appending a response
// row for each member that could not be removed. The caller must hold
// [Handler.fsmu].
func (h *Handler) removeTree(info resourceInfo, failures *[]response) {
	if !info.isDir {
		if err := h.fs.DeleteFile(info.path); err != nil {
			*failures = append(*failures, response{
				Href:   h.href(info.path, false),
				Status: statusText(statusFor(err, http.StatusInternalServerError)),
			})
		}
		return
	}
	entries, err := h.fs.ListDir(info.path)
	if err != nil {
		*failures = append(*failures, response{
			Href:   h.href(info.path, true),
			Status: statusText(statusFor(err, http.StatusInternalServerError)),
		})
		return
	}
	before := len(*failures)
	for _, e := range entries {
		n := e.Name()
		if n == "." || n == ".." {
			continue
		}
		child, err := h.info(join(info.path, n))
		if err != nil {
			*failures = append(*failures, response{
				Href:   h.href(join(info.path, n), false),
				Status: statusText(statusFor(err, http.StatusInternalServerError)),
			})
			continue
		}
		h.removeTree(child, failures)
	}
	if len(*failures) != before {
		// A member survived, so the collection is not empty and DeleteDir
		// would fail. Reporting the member's own failure is more useful than
		// adding a second, derived one for the parent.
		return
	}
	if err := h.fs.DeleteDir(info.path); err != nil {
		*failures = append(*failures, response{
			Href:   h.href(info.path, true),
			Status: statusText(statusFor(err, http.StatusInternalServerError)),
		})
	}
}

// destination parses the Destination header into a path inside this export.
//
// A Destination naming another host or another prefix is 502 Bad Gateway per
// RFC 4918 §9.9.4, not 400: the request is well formed, the server simply
// cannot reach across to another namespace. This is also the check that keeps
// a MOVE from being a way out of the export — the destination goes through
// the same [Handler.resource] normalisation as any request path, so "..",
// "%2e%2e" and an absolute host path all land back inside the image.
func (h *Handler) destination(r *http.Request) (string, int) {
	raw := r.Header.Get("Destination")
	if raw == "" {
		return "", http.StatusBadRequest
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", http.StatusBadRequest
	}
	if u.Host != "" && !strings.EqualFold(u.Host, r.Host) {
		return "", http.StatusBadGateway
	}
	dst, ok := h.resource(u.Path)
	if !ok {
		return "", http.StatusBadGateway
	}
	return dst, 0
}

// overwrite reads the Overwrite header, which defaults to T.
func overwrite(v string) (bool, bool) {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "", "T":
		return true, true
	case "F":
		return false, true
	default:
		return false, false
	}
}

// prepareDestination performs the checks MOVE and COPY share and reports the
// status to send on success: 201 when the destination was created, 204 when
// it was replaced. The caller must hold [Handler.fsmu].
func (h *Handler) prepareDestination(w http.ResponseWriter, r *http.Request, src, dst string) (int, bool) {
	if dst == src {
		http.Error(w, "webdav: source and destination are the same", http.StatusForbidden)
		return 0, false
	}
	if dst == "/" {
		// Replacing the export root would ask the driver to delete the
		// image's own root directory and then rename something onto it.
		http.Error(w, "webdav: cannot overwrite the root collection", http.StatusForbidden)
		return 0, false
	}
	if strictlyUnder(src, dst) {
		// Moving or copying a collection into itself would either lose the
		// tree or recurse for ever — see [under] for the shape that made the
		// naive check miss it at the root. RFC 4918 §9.8.5 and §9.9.5 make
		// it 403.
		http.Error(w, "webdav: destination is inside the source", http.StatusForbidden)
		return 0, false
	}
	ow, ok := overwrite(r.Header.Get("Overwrite"))
	if !ok {
		http.Error(w, "webdav: bad Overwrite header", http.StatusBadRequest)
		return 0, false
	}
	parent, err := h.info(parentOf(dst))
	if err != nil {
		http.Error(w, "webdav: destination parent: "+err.Error(), conflictOr(err))
		return 0, false
	}
	if !parent.isDir {
		http.Error(w, "webdav: destination parent is not a collection", http.StatusConflict)
		return 0, false
	}
	existing, err := h.info(dst)
	if err != nil {
		if !notFound(err) {
			http.Error(w, "webdav: "+err.Error(), statusFor(err, http.StatusInternalServerError))
			return 0, false
		}
		return http.StatusCreated, true
	}
	if !ow {
		http.Error(w, "webdav: destination exists and Overwrite is F", http.StatusPreconditionFailed)
		return 0, false
	}
	var failures []response
	h.removeTree(existing, &failures)
	if len(failures) > 0 {
		writeMultistatus(w, multistatus{Responses: failures})
		return 0, false
	}
	h.locks.releaseTree(dst)
	return http.StatusNoContent, true
}

// serveMove answers MOVE.
func (h *Handler) serveMove(w http.ResponseWriter, r *http.Request, name string) {
	dst, bad := h.destination(r)
	if bad != 0 {
		http.Error(w, "webdav: bad Destination", bad)
		return
	}
	if name == "/" {
		http.Error(w, "webdav: cannot MOVE the root collection", http.StatusForbidden)
		return
	}
	// Both ends are checked against the lock table: a MOVE removes the
	// source and creates the destination, so a lock on either is a reason to
	// refuse.
	if err := h.checkLocks(r, name); err != nil {
		writeLockError(w, err)
		return
	}
	if err := h.checkLocks(r, dst); err != nil {
		writeLockError(w, err)
		return
	}
	h.fsmu.Lock()
	defer h.fsmu.Unlock()
	if _, err := h.info(name); err != nil {
		http.Error(w, "webdav: "+err.Error(), statusFor(err, http.StatusInternalServerError))
		return
	}
	code, ok := h.prepareDestination(w, r, name, dst)
	if !ok {
		return
	}
	if err := h.fs.Rename(name, dst); err != nil {
		http.Error(w, "webdav: "+err.Error(), statusFor(err, http.StatusInternalServerError))
		return
	}
	h.locks.releaseTree(name)
	w.Header().Set("Location", h.href(dst, false))
	w.WriteHeader(code)
}

// serveCopy answers COPY.
//
// There is no copy in [github.com/go-filesystems/interface.Filesystem], so
// this reads and writes. For a collection that means the whole subtree passes
// through memory one file at a time — bounded per file by the driver, not by
// [WithMaxBody], which bounds only what arrives from the network.
func (h *Handler) serveCopy(w http.ResponseWriter, r *http.Request, name string) {
	dst, bad := h.destination(r)
	if bad != 0 {
		http.Error(w, "webdav: bad Destination", bad)
		return
	}
	depth, ok := parseDepth(r.Header.Get("Depth"))
	if !ok || depth == depthOne {
		// RFC 4918 §9.8.3: COPY takes 0 or infinity only. Depth 1 has no
		// meaning for a copy — a collection with unpopulated members is not
		// a copy of anything.
		http.Error(w, "webdav: COPY accepts Depth 0 or infinity", http.StatusBadRequest)
		return
	}
	if err := h.checkLocks(r, dst); err != nil {
		writeLockError(w, err)
		return
	}
	h.fsmu.Lock()
	defer h.fsmu.Unlock()
	src, err := h.info(name)
	if err != nil {
		http.Error(w, "webdav: "+err.Error(), statusFor(err, http.StatusInternalServerError))
		return
	}
	code, ok := h.prepareDestination(w, r, name, dst)
	if !ok {
		return
	}
	var failures []response
	h.copyTree(src, dst, depth == depthInfinity, &failures)
	if len(failures) > 0 {
		writeMultistatus(w, multistatus{Responses: failures})
		return
	}
	w.Header().Set("Location", h.href(dst, src.isDir))
	w.WriteHeader(code)
}

// copyTree copies one resource to dst, recursing when deep. The caller must
// hold [Handler.fsmu].
func (h *Handler) copyTree(src resourceInfo, dst string, deep bool, failures *[]response) {
	if !src.isDir {
		data, err := h.fs.ReadFile(src.path)
		if err == nil {
			err = h.fs.WriteFile(dst, data, defaultFilePerm)
		}
		if err != nil {
			*failures = append(*failures, response{
				Href:   h.href(dst, false),
				Status: statusText(statusFor(err, http.StatusInternalServerError)),
			})
		}
		return
	}
	if err := h.fs.MkDir(dst, defaultDirPerm); err != nil {
		*failures = append(*failures, response{
			Href:   h.href(dst, true),
			Status: statusText(statusFor(err, http.StatusInternalServerError)),
		})
		return
	}
	if !deep {
		// Depth 0 on a collection copies the collection itself with no
		// members, which is what RFC 4918 §9.8.3 says it means.
		return
	}
	entries, err := h.fs.ListDir(src.path)
	if err != nil {
		*failures = append(*failures, response{
			Href:   h.href(dst, true),
			Status: statusText(statusFor(err, http.StatusInternalServerError)),
		})
		return
	}
	for _, e := range entries {
		n := e.Name()
		if n == "." || n == ".." {
			continue
		}
		child, err := h.info(join(src.path, n))
		if err != nil {
			*failures = append(*failures, response{
				Href:   h.href(join(dst, n), false),
				Status: statusText(statusFor(err, http.StatusInternalServerError)),
			})
			continue
		}
		h.copyTree(child, join(dst, n), true, failures)
	}
}
