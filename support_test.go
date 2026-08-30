package webdav_test

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
)

// basic renders an Authorization header value.
func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// request drives a handler with a chosen RemoteAddr, which is the only way to
// exercise the cleartext-credential refusal: a test server always binds to
// loopback, and loopback is exempt.
func request(h http.Handler, method, path, remote string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	req.RemoteAddr = remote
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}
