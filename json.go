package webhttp

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
)

// JSONHeaders sets the standard JSON response headers: an application/json
// content type and the X-Content-Type-Options: nosniff guard against MIME
// sniffing. Call it before WriteHeader.
//
// The nosniff guard is set only when the header is still unset, so a stack
// that composes SecurityHeaders keeps that middleware as the header's single
// writer while a stack without it still gets the guard on every JSON body
// this library writes.
func JSONHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if h.Get("X-Content-Type-Options") == "" {
		h.Set("X-Content-Type-Options", "nosniff")
	}
}

// WriteJSON writes v as a JSON body with a 200 status.
func WriteJSON(w http.ResponseWriter, v any) {
	WriteJSONStatus(w, http.StatusOK, v)
}

// WriteJSONStatus sets the JSON headers, writes the status code, and encodes v
// as the response body. The status is committed before encoding begins, so an
// encode failure cannot change it; such a failure is logged at Warn rather than
// returned, because the response line is already on the wire.
func WriteJSONStatus(w http.ResponseWriter, code int, v any) {
	JSONHeaders(w)
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("webhttp: json encode failed after status committed", "code", code, "error", err)
	}
}

// Ok writes a 200 response with the JSON body {"ok":true}. It is the canonical
// success acknowledgement for an endpoint that has no other payload.
func Ok(w http.ResponseWriter) {
	WriteJSON(w, struct {
		OK bool `json:"ok"`
	}{OK: true})
}

// ErrorCode is the MACHINE-READABLE token carried in the error envelope's
// "code" field — the value a client switches on and an operator's log query and
// alert rules key on. It is a distinct type from the human message so the two
// cannot be transposed at a call site: WriteError, WithHostAllowlistError and
// WithRateLimitError all take the pair (ErrorCode, string) in that order, and
// handing them a message-shaped value where the code goes is a compile error
// wherever either side is a typed constant or a variable.
//
// The grammar is a token, not a sentence: lowercase ASCII letters, digits and
// underscores — "host_not_allowed", "rate_limited", "too_many_auth_failures".
// That is what every code in this library and in every consuming application
// already spells, so it is written down here rather than left to convention. A
// code containing a SPACE is a bug, not a message: the sentence belongs in the
// msg argument beside it, and a code is not a place to put prose.
//
// The empty code is legal and means "omit the field": ErrorResponse.Code is
// omitempty, so an application with a bare {"error":…} taxonomy passes "" and
// gets no code on the wire.
//
// A malformed code is NOT repaired and does NOT panic — see errorEnvelope: the
// envelope encoder substitutes the fixed InvalidErrorCode token so the machine
// slot always holds a machine value, and logs the offending code once per
// process.
type ErrorCode string

// InvalidErrorCode is what the envelope encoder emits in place of a code that
// does not match ErrorCode's grammar. It is a code of its own rather than a
// reused "internal_error" so that a broken code is distinguishable on the wire
// from a genuine internal fault (the request's status is unchanged, and a 400
// must not start claiming a server error), and rather than an empty code so it
// is distinguishable from an application that deliberately omits the field.
// This mirrors the access log's separation of "(path-redaction-failed)" — the
// policy itself broke — from "(unmatched)", a routine outcome.
const InvalidErrorCode ErrorCode = "invalid_error_code"

// ErrorResponse is the JSON error envelope written by WriteError. Code and
// RequestID are omitted from the output when empty.
type ErrorResponse struct {
	// Error is the human-readable error message.
	Error string `json:"error"`
	// Code is an optional machine-readable error code.
	Code ErrorCode `json:"code,omitempty"`
	// RequestID is the request id for log correlation, when available.
	RequestID string `json:"request_id,omitempty"`
}

// WriteError writes an ErrorResponse with the given HTTP status: msg becomes
// Error, code becomes Code, and the request id is pulled from the request
// context (via RequestIDFromContext) so a client can correlate the failure with
// the access log. It is nil-safe: when r is nil the RequestID field stays
// empty.
//
// The code is the machine token and msg is the sentence for a human; they are
// separately typed so the pair cannot be handed over transposed (see
// ErrorCode). A code that is not a well-formed token is replaced with
// InvalidErrorCode rather than emitted, and never panics — this runs per
// request on an error path.
//
// WriteError ships the MECHANISM only. Each consuming application keeps its own
// named-helper and error-code taxonomy on top of it.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code ErrorCode, msg string) {
	WriteJSONStatus(w, status, errorEnvelope(r, code, msg))
}

// errorEnvelope builds the ErrorResponse for r under the package's universal
// correlation scheme: the request id is pulled from the request context when
// present (nil-safe on r) and omitted otherwise. It is the single home of the
// scheme AND of the code grammar; every library error body — written live by
// WriteError or pre-rendered by errorBodyJSON — goes through it, so a malformed
// code cannot reach the wire through any of them.
func errorEnvelope(r *http.Request, code ErrorCode, msg string) ErrorResponse {
	resp := ErrorResponse{Error: msg, Code: checkedErrorCode(code)}
	if r != nil {
		resp.RequestID = RequestIDFromContext(r.Context())
	}
	return resp
}

// warnOnce logs one Warn per instance and swallows every later call. It exists
// because the malformed-error-code diagnostic sits on a per-request error path:
// the condition is a programming error, so logging it per occurrence would turn
// one bad literal into a log flood on whichever route hits it. A type rather
// than a bare sync.Once so the at-most-once behavior is a unit of its own.
type warnOnce struct {
	once sync.Once
}

// warn logs msg with args the first time it is called on this instance.
func (o *warnOnce) warn(msg string, args ...any) {
	o.once.Do(func() { slog.Warn(msg, args...) })
}

// malformedCodeWarn bounds the malformed-error-code diagnostic to one line per
// process. Every later occurrence stays visible on the wire instead, because the
// response itself carries InvalidErrorCode.
var malformedCodeWarn warnOnce

// checkedErrorCode returns code when it matches ErrorCode's grammar and
// InvalidErrorCode when it does not, logging the offender once per process.
//
// It REFUSES rather than repairs (the same stance CanonicalHost takes): there is
// no rewriting of a sentence into a token, because any such rewrite would put a
// value on the wire that no client contract contains. And it does not PANIC,
// which is the other tempting shape and is wrong here for a reason worth
// stating: this runs per request, on the path a request takes when something has
// already gone wrong, so a panic would convert a handled 400 into either a
// dropped connection or — under Recoverer — a 500 that misreports the client's
// error as a server fault. A validating panic belongs at construction time,
// where it fires once before serving starts and cannot be reached by traffic;
// an error code arrives per call and has no construction step to guard.
func checkedErrorCode(code ErrorCode) ErrorCode {
	if validErrorCode(code) {
		return code
	}
	malformedCodeWarn.warn("webhttp: malformed error code replaced in the error envelope; a code is a machine token [a-z0-9_], and a sentence belongs in the message",
		"code", string(code), "replacement", string(InvalidErrorCode))
	return InvalidErrorCode
}

// validErrorCode reports whether code matches ErrorCode's grammar: any number
// of lowercase ASCII letters, digits and underscores. The empty code is valid
// and means "omit the field", so the loop's zero-iteration case is deliberate.
func validErrorCode(code ErrorCode) bool {
	for i := range len(code) {
		switch c := code[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
		default:
			return false
		}
	}
	return true
}

// errorBodyJSON renders the exact ErrorResponse JSON that WriteError would
// write for r, as a string, for APIs that require a pre-rendered body instead
// of writing at request time (http.TimeoutHandler is the one such consumer
// today). ErrorResponse is a fixed struct of string fields, so marshaling
// cannot fail; the fallback literal keeps the body valid JSON even if that
// ever changes.
func errorBodyJSON(r *http.Request, code ErrorCode, msg string) string {
	b, err := json.Marshal(errorEnvelope(r, code, msg))
	if err != nil {
		return `{"error":"internal error"}`
	}
	return string(b)
}

// ErrorResponder renders an error response body. It has the exact signature of
// WriteError, which is its canonical instance and the library-wide default, so
// the JSON envelope is the zero-config behavior. Middleware that emits an error
// body (Recoverer via WithRecoverResponder, RateLimiter via
// WithRateLimitResponder) accepts one so a non-JSON endpoint - an XML or
// plain-text service - can keep its error body on its own
// content type instead of the default JSON. A responder owns writing the status
// and any headers, and is invoked only when the response has not been committed.
//
// A responder receives the ErrorCode as the library resolved it from its
// caller — unvalidated, because the grammar is enforced by the JSON envelope
// encoder and a responder rendering another content type owns its own body. A
// responder that emits the code into a machine-readable slot should route it
// through this package's WriteError, or check it the same way.
type ErrorResponder func(w http.ResponseWriter, r *http.Request, status int, code ErrorCode, msg string)
