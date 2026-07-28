package webhttp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
)

// ErrTrailingData is returned by DecodeJSONInto when the body holds more than a
// single JSON value (data follows the first decoded value). Callers that map
// decode errors to a status treat it like any other malformed-body error (400).
var ErrTrailingData = errors.New("webhttp: unexpected data after JSON value")

// MaxJSONBody is the default maximum JSON request-body size (1 MiB) applied by
// DecodeBody and DecodeBodyOptional.
const MaxJSONBody int64 = 1 << 20

// LimitBody caps the request body at maxBytes by replacing r.Body with an
// http.MaxBytesReader. Call it before reading the body.
//
// It writes NOTHING to w, so detecting and answering an over-limit body is the
// CALLER's job, on the read error — that error is the ONLY signal the condition
// reliably delivers:
//
//	webhttp.LimitBody(w, r, maxBytes)
//	body, err := io.ReadAll(r.Body)
//	var tooLarge *http.MaxBytesError
//	if errors.As(err, &tooLarge) {
//		// tooLarge.Limit == maxBytes. The status is yours: 413, 400, or none
//		// at all (log it and drop the request).
//	}
//
// Every read path in the package keeps that contract: DecodeJSONInto returns
// the *http.MaxBytesError untouched, and DecodeBody maps it — like any other
// decode failure — to a 400.
//
// # The connection-close signal, and when it is absent
//
// http.MaxBytesReader also tells net/http that the request was too large, which
// makes the server add Connection: close and close the connection after the
// reply rather than draining the sender's remaining bytes. It delivers that by
// type-asserting an UNEXPORTED net/http interface on the ResponseWriter it was
// handed, so it reaches net/http ONLY when the writer is net/http's own — no
// third-party wrapper can satisfy an unexported method, and MaxBytesReader does
// not walk Unwrap itself. LimitBody therefore walks w's Unwrap chain (the
// http.ResponseController convention) and hands MaxBytesReader the writer at
// the end of it. The middlewares here that wrap the writer to record a status
// (Logging and Recoverer, via StatusRecorder) implement Unwrap, so a normal
// Chain keeps the signal.
//
// It is still absent, with no way for this package to recover it, when:
//
//   - some wrapper in the chain does not implement Unwrap() http.ResponseWriter
//     (a third-party middleware): the walk stops at that wrapper;
//   - the handler runs under RouteTimeout / http.TimeoutHandler, whose
//     buffering writer is not unwrappable;
//   - w is not a net/http writer at all (an httptest.ResponseRecorder).
//
// In every one of those cases the read still fails with a *http.MaxBytesError —
// which is why the caller-side check above is the contract and the close is
// not. Independently of this signal, net/http closes the connection anyway when
// more than 256 KiB of the body is left unread.
func LimitBody(w http.ResponseWriter, r *http.Request, maxBytes int64) {
	r.Body = http.MaxBytesReader(baseResponseWriter(w), r.Body, maxBytes)
}

// unwrapWalkLimit bounds the baseResponseWriter walk. Any real chain is a
// handful of wrappers deep; the bound is what makes a writer whose Unwrap
// returns itself (or a cycle of two) terminate instead of spinning. The walk
// deliberately does not compare writers to detect the cycle: == on interface
// values panics when the dynamic type is not comparable, and a middleware
// writer is free to be such a type.
const unwrapWalkLimit = 16

// baseResponseWriter returns the writer at the end of w's Unwrap chain: the
// http.ResponseWriter net/http itself passed to the handler, when every wrapper
// in between implements Unwrap() http.ResponseWriter. It is what LimitBody
// hands to http.MaxBytesReader so the too-large signal reaches net/http's own
// writer (see LimitBody for why that signal cannot travel through a wrapper).
//
// The returned writer is used ONLY as that signal's target — never written
// through — so unwrapping cannot bypass a wrapper's own behavior: a
// StatusRecorder still sees, and records, everything the handler writes.
//
// A chain that stops early (a wrapper without Unwrap, an Unwrap returning nil)
// yields the last writer reached, which is exactly the writer today's callers
// pass by hand: no worse than not walking at all.
func baseResponseWriter(w http.ResponseWriter) http.ResponseWriter {
	for range unwrapWalkLimit {
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return w
		}
		inner := u.Unwrap()
		if inner == nil {
			return w
		}
		w = inner
	}
	return w
}

// SetAllow sets the RFC 9110 Allow header to the set of methods a route
// advertises as supported: the entries joined with ", " (RFC 9110 Section
// 10.2.1, "Allow = #method"). A 405 response MUST carry the header, which is
// why MethodNotAllowed and RequireMethod both render it here; the spec permits
// it on any other response too, so an OPTIONS handler can advertise its route
// with the same call.
//
// Entries are method tokens (http.MethodGet and friends) emitted verbatim,
// because a method token is case-sensitive (Section 9.1): the header must name
// what the route actually compares a request method against, not a
// canonicalized spelling of it. An empty entry is dropped, so a set assembled
// from configuration cannot emit the empty list element a sender must not
// generate (Section 5.6.1.1), and an exact duplicate is collapsed because the
// field names a set. Passing no methods sets the header to the empty value,
// which is the spec's own encoding for "this resource allows no methods" — a
// route disabled by configuration — and keeps the 405's MUST satisfied.
//
// HEAD is deliberately NOT implied by GET. The field reports what THIS route
// advertises, and a route may refuse HEAD on purpose: net/http's ServeMux
// serves a HEAD request from a GET pattern, so a route whose GET carries a
// side effect (recording a heartbeat, say) registers HEAD separately to reject
// it and must not then advertise it. Pass http.MethodHead explicitly when the
// route serves it.
func SetAllow(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", allowValue(allowed))
}

// allowValue renders the Allow field value for an advertised method set. The
// single-method case returns the entry unchanged (the RequireMethod path, and
// the reason a one-method Allow value is byte-identical to a hand-set header).
func allowValue(allowed []string) string {
	if len(allowed) == 1 {
		return allowed[0]
	}
	uniq := make([]string, 0, len(allowed))
	for _, m := range allowed {
		if m == "" || slices.Contains(uniq, m) {
			continue
		}
		uniq = append(uniq, m)
	}
	return strings.Join(uniq, ", ")
}

// MethodNotAllowed writes the 405 refusal for a request whose method the route
// does not support: it sets the Allow header to the allowed set (see SetAllow)
// and writes the standard error response (code "method_not_allowed"). It is
// the rejection half of RequireMethod, exported because a route may permit
// SEVERAL methods and so cannot express its refusal as a single-method guard:
//
//	// POST /things and GET /things are registered; HEAD is not served.
//	mux.HandleFunc("HEAD /things", func(w http.ResponseWriter, r *http.Request) {
//		webhttp.MethodNotAllowed(w, r, http.MethodGet, http.MethodPost)
//	})
//
// A handler that dispatches on the method itself (a switch over r.Method)
// calls it from the default branch with the same list. Either way the 405
// carries the full Allow list RFC 9110 requires, instead of the one method a
// guard could name.
func MethodNotAllowed(w http.ResponseWriter, r *http.Request, allowed ...string) {
	SetAllow(w, allowed...)
	WriteError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

// RequireMethod reports whether the request method equals method. On a mismatch
// it writes the standard 405 via MethodNotAllowed, naming method as the only
// allowed one, and returns false, so a handler can guard with:
//
//	if !webhttp.RequireMethod(w, r, http.MethodPost) {
//		return
//	}
//
// A route that permits more than one method has no single method to require:
// route each method with the mux (net/http's method-prefixed patterns) or
// dispatch on r.Method, and refuse the rest with MethodNotAllowed so the 405
// advertises the whole set.
func RequireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		MethodNotAllowed(w, r, method)
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
	var extra json.RawMessage
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
		return nil // exactly one value: the body held no trailing data
	case err != nil:
		return err // a malformed second value
	default:
		return ErrTrailingData // any well-formed second JSON value followed the first
	}
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
