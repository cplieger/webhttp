package webhttp

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// JSONHeaders sets the standard JSON response headers: an application/json
// content type and the X-Content-Type-Options: nosniff guard against MIME
// sniffing. Call it before WriteHeader.
func JSONHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
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

// ErrorResponse is the JSON error envelope written by WriteError. Code and
// RequestID are omitted from the output when empty.
type ErrorResponse struct {
	// Error is the human-readable error message.
	Error string `json:"error"`
	// Code is an optional machine-readable error code.
	Code string `json:"code,omitempty"`
	// RequestID is the request id for log correlation, when available.
	RequestID string `json:"request_id,omitempty"`
}

// WriteError writes an ErrorResponse with the given HTTP status: msg becomes
// Error, code becomes Code, and the request id is pulled from the request
// context (via RequestIDFromContext) so a client can correlate the failure with
// the access log. It is nil-safe: when r is nil the RequestID field stays
// empty.
//
// WriteError ships the MECHANISM only. Each consuming application keeps its own
// named-helper and error-code taxonomy on top of it.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	resp := ErrorResponse{Error: msg, Code: code}
	if r != nil {
		resp.RequestID = RequestIDFromContext(r.Context())
	}
	WriteJSONStatus(w, status, resp)
}
