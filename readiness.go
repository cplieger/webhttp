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
// {"status":"unready","reason":"starting up or shutting down"}. Both responses
// carry Cache-Control: no-store.
//
// This is the HTTP SERVING-STATE gate (note the lowercase "ok"), meant for a
// load balancer asking "should this instance receive traffic right now?". It is
// deliberately distinct from the cplieger health library's container
// file-marker probe, which answers {"status":"OK","timestamp":…} for a Docker
// HEALTHCHECK asking "is the process alive?". The two are complementary and are
// not the same endpoint.
//
// no-store is not decoration. Under RFC 9111 a 200 GET carrying no explicit
// freshness information is HEURISTICALLY CACHEABLE, and the answer to "should
// this instance receive traffic right now" is the one answer in a service that
// is never valid a moment later. The unready direction is safe by accident (503
// is not among the heuristically-cacheable statuses), so the reachable failure
// is the dangerous one: a cached "ok" outliving the readiness it reported, which
// keeps traffic arriving at an instance that has begun draining — defeating the
// gate at exactly the moment it exists for. Consumers that front this endpoint
// with a caching reverse proxy (the documented deployment shape for more than
// one of them) were relying on that proxy not to cache.
func ReadinessHandler(c ReadinessChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
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
