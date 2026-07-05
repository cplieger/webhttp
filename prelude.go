package webhttp

import (
	"encoding/json"
	"io"
	"net/http"
)

// MaxJSONBody is the default maximum JSON request-body size (1 MiB) applied by
// DecodeBody and DecodeBodyOptional.
const MaxJSONBody int64 = 1 << 20

// LimitBody caps the request body at maxBytes by replacing r.Body with an
// http.MaxBytesReader. A read past the limit then fails with a
// *http.MaxBytesError (and net/http is asked to close the connection); it does
// not by itself write a 413, so the caller chooses the response status.
// DecodeBody, for example, surfaces the resulting decode error as a 400. Call
// it before reading the body.
func LimitBody(w http.ResponseWriter, r *http.Request, maxBytes int64) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
}

// RequireMethod reports whether the request method equals method. On a mismatch
// it sets the RFC 9110 Allow header to method, writes a 405 error response
// (code "method_not_allowed"), and returns false, so a handler can guard with:
//
//	if !webhttp.RequireMethod(w, r, http.MethodPost) {
//		return
//	}
func RequireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		w.Header().Set("Allow", method)
		WriteError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return false
	}
	return true
}

// DecodeBody limits the body to MaxJSONBody and decodes exactly one JSON value
// into v. The decoded value must be the entire body: trailing data or a second
// JSON value is rejected. On any decode failure it writes a 400 error response
// (code "bad_request") carrying errMsg and returns false; on success it returns
// true.
func DecodeBody(w http.ResponseWriter, r *http.Request, v any, errMsg string) bool {
	LimitBody(w, r, MaxJSONBody)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		WriteError(w, r, http.StatusBadRequest, "bad_request", errMsg)
		return false
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		WriteError(w, r, http.StatusBadRequest, "bad_request", errMsg)
		return false
	}
	return true
}

// DecodeBodyOptional limits the body to MaxJSONBody and attempts a JSON decode
// into v, ignoring any error. Use it when the body is optional and a missing or
// malformed body should leave v at its zero value rather than fail the request.
func DecodeBodyOptional(w http.ResponseWriter, r *http.Request, v any) {
	LimitBody(w, r, MaxJSONBody)
	_ = json.NewDecoder(r.Body).Decode(v)
}
