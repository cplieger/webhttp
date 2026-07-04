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
// http.MaxBytesReader. A read past the limit then fails and the server responds
// 413 instead of buffering an unbounded payload. Call it before reading the
// body.
func LimitBody(w http.ResponseWriter, r *http.Request, maxBytes int64) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
}

// RequireMethod reports whether the request method equals method. On a mismatch
// it writes a 405 error response (code "method_not_allowed") and returns false,
// so a handler can guard with:
//
//	if !webhttp.RequireMethod(w, r, http.MethodPost) {
//		return
//	}
func RequireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		WriteError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return false
	}
	return true
}

// DecodeBody limits the body to MaxJSONBody and decodes a single JSON value
// into v. On any decode failure it writes a 400 error response (code
// "bad_request") carrying errMsg and returns false; on success it returns true.
func DecodeBody(w http.ResponseWriter, r *http.Request, v any, errMsg string) bool {
	LimitBody(w, r, MaxJSONBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
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

// LimitedWriter wraps an io.Writer and caps the total bytes forwarded at N.
// Once N bytes have been written, further bytes are silently dropped; every
// Write still reports its full input length and a nil error, so the cap is
// transparent to the caller. An N of zero or less drops everything. It is the
// write-side counterpart to io.LimitReader, useful for bounding how much of a
// stream is mirrored into a buffer.
type LimitedWriter struct {
	// W is the underlying writer that receives the un-dropped bytes.
	W io.Writer
	// N is the remaining byte budget; it decreases as bytes are forwarded.
	N int64
}

// Write forwards up to the remaining N bytes of p to the underlying writer and
// silently drops the rest, reporting len(p) written so the cap is transparent.
// If the underlying writer errors, that error and its short count are returned.
func (lw *LimitedWriter) Write(p []byte) (int, error) {
	if lw.N <= 0 {
		return len(p), nil
	}
	total := len(p)
	if int64(total) > lw.N {
		p = p[:lw.N]
	}
	n, err := lw.W.Write(p)
	lw.N -= int64(n)
	if err != nil {
		return n, err
	}
	return total, nil
}
