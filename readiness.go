package webhttp

import (
	"net/http"
	"sync/atomic"
)

// Ready is a concurrency-safe readiness flag. Its zero value reports not ready,
// so a service starts unready until it calls Set(true) once initialization
// completes, and can flip back to unready during shutdown.
type Ready struct {
	flag atomic.Bool
}

// Set records whether the service is ready to serve traffic.
func (r *Ready) Set(ready bool) {
	r.flag.Store(ready)
}

// Ready reports whether the service is currently ready to serve traffic.
func (r *Ready) Ready() bool {
	return r.flag.Load()
}

// ReadinessChecker is the readiness view ReadinessHandler needs. *Ready
// satisfies it; a service with a composite readiness model can supply its own
// implementation.
type ReadinessChecker interface {
	// Ready reports whether the service should receive traffic right now.
	Ready() bool
}

var _ ReadinessChecker = (*Ready)(nil)

// readinessResponse is the JSON body ReadinessHandler writes. A struct (rather
// than a map) fixes the key order to {"status":…,"reason":…}: encoding/json
// sorts map keys alphabetically, which would otherwise emit
// {"reason":…,"status":…}. Reason is omitted when empty.
type readinessResponse struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// ReadinessHandler returns a handler that reports serving state as JSON: 200
// with {"status":"ok"} when c reports ready, otherwise 503 with
// {"status":"unready","reason":"starting up or shutting down"}.
//
// This is the HTTP SERVING-STATE gate (note the lowercase "ok"), meant for a
// load balancer asking "should this instance receive traffic right now?". It is
// deliberately distinct from the cplieger health library's container
// file-marker probe, which answers {"status":"OK","timestamp":…} for a Docker
// HEALTHCHECK asking "is the process alive?". The two are complementary and are
// not the same endpoint.
func ReadinessHandler(c ReadinessChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if c.Ready() {
			WriteJSONStatus(w, http.StatusOK, readinessResponse{Status: "ok"})
			return
		}
		WriteJSONStatus(w, http.StatusServiceUnavailable, readinessResponse{
			Status: "unready",
			Reason: "starting up or shutting down",
		})
	}
}
