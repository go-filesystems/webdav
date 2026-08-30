package webdav

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// errNilStat reports a driver whose Stat returned (nil, nil).
var errNilStat = errors.New("webdav: driver Stat returned a nil Stat with no error")

// maxPropfindBody bounds a PROPFIND request body at 1 MiB. A property list is
// a few hundred bytes; anything approaching this is either a mistake or an
// attempt to make the server allocate, and an XML parser reading from an
// unbounded network reader is the classic way to be made to.
const maxPropfindBody = 1 << 20

// depth values. RFC 4918 spells infinity as the literal string "infinity";
// it is -1 here so that the three cases compare as numbers.
const (
	depthZero     = 0
	depthOne      = 1
	depthInfinity = -1
)

// parseDepth reads the Depth header. A missing header means infinity per RFC
// 4918 §10.2, which matters: a client that omits it on PROPFIND is asking for
// the whole tree, and answering as though it had said 0 would silently give
// it a different answer than it asked for.
func parseDepth(v string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return depthInfinity, true
	case "0":
		return depthZero, true
	case "1":
		return depthOne, true
	case "infinity":
		return depthInfinity, true
	default:
		return 0, false
	}
}

// propfindRequest is the parsed body of a PROPFIND.
type propfindRequest struct {
	allprop  bool
	propname bool
	// names lists the properties a <prop> request asked for, in the order
	// asked. Order is preserved because a client that reads the response
	// positionally — and some do — must not be surprised.
	names []xml.Name
	// include lists <allprop><include> members, which is how RFC 4918 §9.1
	// lets a client ask for a property that allprop is not obliged to
	// return. Everything this server has is in allprop already, so the list
	// is parsed and satisfied rather than ignored.
	include []xml.Name
}

// parsePropfind decodes a PROPFIND body.
//
// An empty body means allprop. That is not a convenience: RFC 4918 §9.1 says
// a client may send no body at all, and the macOS client does exactly that on
// the first request of a mount, so a server that requires a body does not
// mount there.
func parsePropfind(r io.Reader) (propfindRequest, error) {
	dec := xml.NewDecoder(io.LimitReader(r, maxPropfindBody))
	var req propfindRequest
	// inProp and inInclude say where in the document the decoder is, so that
	// a property name is only collected from where a property name belongs.
	// Reading names at any depth would let <prop><foo><bar/></foo></prop>
	// contribute "bar" as a property in its own right; every element that is
	// not one of the four containers is therefore skipped whole rather than
	// descended into.
	var inProp, inInclude, seenRoot bool
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return propfindRequest{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if !seenRoot {
				if t.Name.Space != davNS || t.Name.Local != "propfind" {
					return propfindRequest{}, errBadBody
				}
				seenRoot = true
				continue
			}
			switch {
			case inProp:
				req.names = append(req.names, t.Name)
			case inInclude:
				req.include = append(req.include, t.Name)
			case t.Name.Space == davNS:
				switch t.Name.Local {
				case "prop":
					inProp = true
					continue
				case "include":
					inInclude = true
					continue
				case "allprop":
					req.allprop = true
				case "propname":
					req.propname = true
				}
			}
			if err := dec.Skip(); err != nil {
				return propfindRequest{}, err
			}
		case xml.EndElement:
			if t.Name.Space == davNS {
				switch t.Name.Local {
				case "prop":
					inProp = false
				case "include":
					inInclude = false
				}
			}
		}
	}
	if !seenRoot {
		// No body at all: allprop, per RFC 4918 section 9.1.
		req.allprop = true
		return req, nil
	}
	if !req.allprop && !req.propname && len(req.names) == 0 {
		// <propfind></propfind> with nothing in it names no request type.
		return propfindRequest{}, errBadBody
	}
	return req, nil
}

// errBadBody reports a request body whose root element is not the one the
// method requires, or that names no request type at all.
var errBadBody = errors.New("webdav: malformed request body")

// servePropfind answers PROPFIND.
func (h *Handler) servePropfind(w http.ResponseWriter, r *http.Request, name string) {
	depth, ok := parseDepth(r.Header.Get("Depth"))
	if !ok {
		http.Error(w, "webdav: bad Depth", http.StatusBadRequest)
		return
	}
	if depth == depthInfinity {
		// RFC 4918 §9.1 lets a server refuse Depth: infinity, and this one
		// does. The alternative is walking an entire image — for an OCI or
		// squashfs export, possibly millions of entries — inside one request
		// while holding the driver lock, which is a denial of service any
		// client can trigger with one header. The refusal is the one RFC
		// 4918 §9.1 defines, so a client knows to re-ask with Depth: 1.
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, xml.Header+
			`<error xmlns="DAV:"><propfind-finite-depth/></error>`+"\n")
		return
	}
	req, err := parsePropfind(r.Body)
	if err != nil {
		http.Error(w, "webdav: malformed PROPFIND body", StatusUnprocessable)
		return
	}

	h.fsmu.Lock()
	defer h.fsmu.Unlock()

	self, err := h.info(name)
	if err != nil {
		http.Error(w, "webdav: "+err.Error(), statusFor(err, http.StatusInternalServerError))
		return
	}
	ms := multistatus{Responses: []response{h.respond(self, req)}}
	if depth == depthOne && self.isDir {
		entries, err := h.fs.ListDir(name)
		if err != nil {
			http.Error(w, "webdav: "+err.Error(), statusFor(err, http.StatusInternalServerError))
			return
		}
		for _, e := range entries {
			n := e.Name()
			if n == "." || n == ".." {
				// A FAT or ISO directory carries these on disk. Emitting
				// them would make a client that walks hrefs recurse for
				// ever, and "." would appear twice with two names.
				continue
			}
			child, err := h.info(join(name, n))
			if err != nil {
				// A directory entry whose Stat fails is reported as its own
				// row rather than failing the whole listing: one unreadable
				// file must not make a folder unopenable.
				ms.Responses = append(ms.Responses, response{
					Href:   h.href(join(name, n), false),
					Status: statusText(statusFor(err, http.StatusInternalServerError)),
				})
				continue
			}
			ms.Responses = append(ms.Responses, h.respond(child, req))
		}
	}
	writeMultistatus(w, ms)
}

// respond renders one resource's row.
func (h *Handler) respond(info resourceInfo, req propfindRequest) response {
	res := response{Href: h.href(info.path, info.isDir)}
	switch {
	case req.propname:
		// propname asks for the *names* the server has, with no values.
		names := make([]property, 0, len(h.propNames(info)))
		for _, n := range h.propNames(info) {
			names = append(names, prop(n, nil))
		}
		res.Propstat = []propstat{newPropstat(http.StatusOK, names...)}
	case req.allprop:
		res.Propstat = []propstat{newPropstat(http.StatusOK, h.allProps(info)...)}
	default:
		found, missing := h.namedProps(info, req.names)
		if len(found) > 0 {
			res.Propstat = append(res.Propstat, newPropstat(http.StatusOK, found...))
		}
		if len(missing) > 0 {
			res.Propstat = append(res.Propstat, newPropstat(http.StatusNotFound, missing...))
		}
	}
	return res
}

// propNames lists the properties this server has for a resource, which is
// what propname answers and what allprop iterates.
func (h *Handler) propNames(info resourceInfo) []string {
	names := []string{"resourcetype", "displayname", "getlastmodified", "creationdate",
		"getetag", "getcontenttype", "supportedlock", "lockdiscovery"}
	if !info.isDir {
		// getcontentlength is the one property RFC 4918 leaves undefined for
		// a collection: there is no body to have a length.
		names = append(names, "getcontentlength")
	}
	if info.isDir && h.total > 0 {
		names = append(names, "quota-available-bytes", "quota-used-bytes")
	}
	return names
}

// allProps renders every property with its value.
func (h *Handler) allProps(info resourceInfo) []property {
	names := h.propNames(info)
	out := make([]property, 0, len(names))
	for _, n := range names {
		// The result of the lookup is not checked here. propNames lists only
		// names propValue knows, and that invariant is held by a test
		// (TestEveryNamedPropertyHasAValue) rather than by a branch in this
		// loop that no request could reach.
		p, _ := h.propValue(info, xml.Name{Space: davNS, Local: n})
		out = append(out, p)
	}
	return out
}

// namedProps splits a client's property list into the ones this server has
// and the ones it does not.
func (h *Handler) namedProps(info resourceInfo, names []xml.Name) (found, missing []property) {
	for _, n := range names {
		if p, ok := h.propValue(info, n); ok {
			found = append(found, p)
			continue
		}
		// A property the server does not have is reported by name under 404
		// — with no value, because there is none. Answering the whole
		// PROPFIND with 404 instead is the common bug: it tells the client
		// the *resource* is missing.
		missing = append(missing, property{XMLName: n})
	}
	return found, missing
}

// propValue renders one property, or reports that this server has no such
// property for this resource.
func (h *Handler) propValue(info resourceInfo, n xml.Name) (property, bool) {
	if n.Space != davNS {
		// Dead properties in other namespaces are not stored: the Filesystem
		// contract has no extended attributes, so there is nowhere to keep
		// one. Saying so is better than inventing an empty value.
		return property{}, false
	}
	switch n.Local {
	case "resourcetype":
		if info.isDir {
			return prop("resourcetype", []byte(`<collection xmlns="DAV:"/>`)), true
		}
		// An empty resourcetype is what a non-collection has, and it must
		// still be present: a client that finds no resourcetype at all
		// cannot tell a file from a server that forgot to answer.
		return prop("resourcetype", nil), true
	case "displayname":
		return prop("displayname", textValue(info.name)), true
	case "getlastmodified":
		return prop("getlastmodified", textValue(info.modTime.UTC().Format(http.TimeFormat))), true
	case "creationdate":
		// RFC 4918 requires ISO 8601 here and an HTTP-date above; they are
		// genuinely different formats for the same instant.
		return prop("creationdate", textValue(info.modTime.UTC().Format(time.RFC3339))), true
	case "getetag":
		return prop("getetag", textValue(info.etag)), true
	case "supportedlock":
		return prop("supportedlock", []byte(supportedLockXML)), true
	case "lockdiscovery":
		return prop("lockdiscovery", h.discoveryXML(info.path)), true
	case "getcontentlength":
		if info.isDir {
			return property{}, false
		}
		return prop("getcontentlength", textValue(strconv.FormatUint(info.size, 10))), true
	case "getcontenttype":
		return prop("getcontenttype", textValue(contentTypeOf(info))), true
	case "quota-available-bytes":
		if !info.isDir || h.total == 0 {
			return property{}, false
		}
		return prop("quota-available-bytes", textValue(strconv.FormatUint(h.avail, 10))), true
	case "quota-used-bytes":
		if !info.isDir || h.total == 0 {
			return property{}, false
		}
		return prop("quota-used-bytes", textValue(strconv.FormatUint(h.total-h.avail, 10))), true
	default:
		return property{}, false
	}
}

// serveProppatch answers PROPPATCH.
//
// Every requested change is refused, with 403 per property in a conformant
// 207 body. This is a deliberate answer rather than a missing feature: RFC
// 4918 §9.2 requires a server to store arbitrary dead properties, and
// [github.com/go-filesystems/interface.Filesystem] has no extended-attribute
// or property store to put them in — there is nowhere on a FAT32 or ISO 9660
// image for a <Z:my-property> to live. Accepting them and dropping them would
// be worse: a client would believe the value was kept.
//
// The visible consequence is that the macOS Finder's PROPPATCH after an
// upload (it tries to set getlastmodified) fails. The file is written; only
// its timestamp is not what the client asked for, which is the same gap
// [TimeStat] describes.
func (h *Handler) serveProppatch(w http.ResponseWriter, r *http.Request, name string) {
	if err := h.checkLocks(r, name); err != nil {
		writeLockError(w, err)
		return
	}
	names, err := parseProppatch(r.Body)
	if err != nil {
		http.Error(w, "webdav: malformed PROPPATCH body", StatusUnprocessable)
		return
	}
	h.fsmu.Lock()
	info, statErr := h.info(name)
	h.fsmu.Unlock()
	if statErr != nil {
		http.Error(w, "webdav: "+statErr.Error(), statusFor(statErr, http.StatusInternalServerError))
		return
	}
	refused := make([]property, 0, len(names))
	for _, n := range names {
		refused = append(refused, property{XMLName: n})
	}
	writeMultistatus(w, multistatus{Responses: []response{{
		Href:     h.href(info.path, info.isDir),
		Propstat: []propstat{newPropstat(http.StatusForbidden, refused...)},
	}}})
}

// parseProppatch collects the property names a PROPPATCH would set or remove.
// Values are not read: none of them can be stored, so decoding them would be
// work done only to throw it away.
func parseProppatch(r io.Reader) ([]xml.Name, error) {
	dec := xml.NewDecoder(io.LimitReader(r, maxPropfindBody))
	var names []xml.Name
	var inProp, seenRoot bool
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if !seenRoot {
				if t.Name.Space != davNS || t.Name.Local != "propertyupdate" {
					return nil, errBadBody
				}
				seenRoot = true
				continue
			}
			switch {
			case inProp:
				names = append(names, t.Name)
			case t.Name.Space == davNS:
				switch t.Name.Local {
				case "prop":
					inProp = true
					continue
				case "set", "remove":
					// Containers: <prop> is inside them, so descend.
					continue
				}
			}
			if err := dec.Skip(); err != nil {
				return nil, err
			}
		case xml.EndElement:
			if t.Name.Space == davNS && t.Name.Local == "prop" {
				inProp = false
			}
		}
	}
	if !seenRoot {
		return nil, errBadBody
	}
	return names, nil
}
