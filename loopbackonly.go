package webhttp

import "net/http"

// LoopbackOnly is the MIDDLEWARE half of the loopback-only admission decision,
// beside the LoopbackRequest predicate this package already exports.
//
// The predicate answers "did this request come from inside" from the two fields
// that cannot be spoofed at this layer. This adds the second half every real
// consumer of that predicate turned out to need: a refusal on proxy and browser
// PROVENANCE headers. LoopbackRequest deliberately ignores those headers in both
// directions — they can never admit and never refuse — and its documentation
// tells an app that wants the deny to "compose that deny around this predicate
// rather than finding it folded in". That guidance stands; this is that
// composition, written once. The predicate is untouched, so nothing that calls
// it directly changes behaviour.
//
// Why the deny is needed at all, and why it is not paranoia: the consumer of a
// loopback-only surface is an in-container CLI client, which sends none of these
// headers, so their presence is positive evidence the request did NOT originate
// inside the container. It matters because a reverse proxy SHARING the server's
// loopback interface (host networking, a shared network namespace) rewrites Host
// to its upstream address by default in both nginx and Apache — which satisfies
// LoopbackRequest's Host leg while the peer leg is satisfied by the proxy itself.
// Both legs pass and the request is remote. The provenance headers are what
// distinguishes it.
//
// It has a known ceiling, stated because a gate whose limits are unwritten gets
// trusted past them: a same-loopback proxy that strips every provenance header
// is indistinguishable from an in-container caller and is admitted. Closing that
// needs authentication, not a header rule.
//
// refuse writes the rejection. Pass nil for the package default (403 with the
// standard error envelope); pass your own to keep an envelope or wording your
// consumers already depend on — the refusal is the whole of what a rejected
// caller is told, so it belongs to the app, not here.
func LoopbackOnly(refuse http.Handler) Middleware {
	if refuse == nil {
		refuse = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			WriteError(w, r, http.StatusForbidden, loopbackRefusalCode,
				"this endpoint is loopback-only; call it from inside the container")
		})
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !LoopbackRequest(r) || ProxiedRequest(r.Header) {
				refuse.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// loopbackRefusalCode is the default refusal's envelope code. Fixed rather than a
// parameter for the reason the failed-auth throttle's code is: the code is what
// an operator's log queries and alert rules key on across services, while the
// MESSAGE is what differs per surface. A consumer needing a different code passes
// its own refuse handler.
const loopbackRefusalCode = "loopback_only"

// proxyProvenanceHeaders are the headers a request acquires by travelling through
// a browser or a reverse proxy.
//
// Sec-Fetch-Site and Origin are in the set because a BROWSER is not an
// in-container CLI client either: a page that reached a loopback-shared server
// has provenance a curl does not. Note that this makes the set broader than
// "proxied" alone, which is why the exported predicate is named for requests
// that came from somewhere rather than for proxies specifically.
var proxyProvenanceHeaders = []string{
	"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
	"X-Real-Ip", "Sec-Fetch-Site", "Origin",
}

// ProxiedRequest reports whether h carries any evidence the request was forwarded
// by a proxy or issued by a browser.
//
// Exported alongside the middleware because the header SET is the drifting
// knowledge: two consumers had hand-rolled it, and a consumer that cannot adopt
// the middleware wholesale (one with its own admission pipeline) should still be
// able to share the list rather than maintain a third copy. It is a positive
// signal only — never treat a missing header as evidence of anything.
func ProxiedRequest(h http.Header) bool {
	for _, name := range proxyProvenanceHeaders {
		if h.Get(name) != "" {
			return true
		}
	}
	return false
}
