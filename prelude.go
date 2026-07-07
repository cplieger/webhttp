package webhttp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// ErrTrailingData is returned by DecodeJSONInto when the body holds more than a
// single JSON value (data follows the first decoded value). Callers that map
// decode errors to a status treat it like any other malformed-body error (400).
var ErrTrailingData = errors.New("webhttp: unexpected data after JSON value")

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

// DecodeJSONInto caps the request body at maxBytes (via LimitBody) and decodes
// exactly one JSON value into v, rejecting trailing data (a second value or any
// bytes past the first are an error). It returns the decode error and writes
// NOTHING to w — so callers that layer their own error taxonomy or need a
// non-default cap can map the result themselves. The returned error is:
//
//   - a *http.MaxBytesError (test with errors.As) when the body exceeded
//     maxBytes — the caller decides 413 vs 400;
//   - ErrTrailingData when a second JSON value follows the first;
//   - otherwise the underlying *json.SyntaxError / *json.UnmarshalTypeError /
//     io.EOF (empty body) for a malformed body — typically a 400.
//
// It is the shared mechanism behind DecodeBody: DecodeBody is DecodeJSONInto at
// the MaxJSONBody cap plus a coded-400 WriteError on any error. Apps with a
// bare {"error":…} envelope, a per-endpoint size cap, or a 413/400 split (see
// the webhttp consumers) build their own one-liner on it instead of reproducing
// the cap + decode + trailing-check by hand.
func DecodeJSONInto(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) error {
	LimitBody(w, r, maxBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return ErrTrailingData
	}
	return nil
}

// DecodeBody limits the body to MaxJSONBody and decodes exactly one JSON value
// into v. The decoded value must be the entire body: trailing data or a second
// JSON value is rejected. On any decode failure it writes a 400 error response
// (code "bad_request") carrying errMsg and returns false; on success it returns
// true. It is DecodeJSONInto at the default cap with a coded-400 response — see
// DecodeJSONInto for the mechanism and the app-taxonomy escape hatch.
func DecodeBody(w http.ResponseWriter, r *http.Request, v any, errMsg string) bool {
	if err := DecodeJSONInto(w, r, v, MaxJSONBody); err != nil {
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
