package webhttp

import (
	"net"
	"strings"
)

// BindClass is the network-exposure classification of a configured listen
// address — the value an operator puts in a bind knob ("KWEB_ADDR",
// "WT_ADDR", "LISTEN_ADDR") before the process calls net.Listen. Apps use it
// at boot to drive exposure POLICY (warn that an unauthenticated surface is
// reachable beyond loopback, flag an open-and-public deployment); the
// classification itself is the shared mechanism.
//
// Classify the CONFIGURED bind only. A live socket peer (r.RemoteAddr) is a
// different concern with different inputs (the Go server always supplies a
// parseable ip:port, and the question is "who connected", not "where do I
// listen"); peer decisions belong to their own gates (e.g. HostPolicy's
// loopback carve-out), never to this classifier.
type BindClass int

const (
	// BindInvalid means the address does not parse as "host:port"
	// (net.SplitHostPort rejected it: no port, stray brackets, too many
	// colons). The zero value, so an uninitialized classification never
	// reads as a safe loopback bind. What to do with it is app policy —
	// see the recipes on ClassifyBind.
	BindInvalid BindClass = iota
	// BindLoopback means the host names the loopback interface explicitly:
	// a 127.0.0.0/8 or ::1 literal (including 4-in-6 forms of 127/8), or
	// the name "localhost" in any case. Only these are "safe" in the
	// exposure sense: the listener is unreachable from other machines.
	BindLoopback
	// BindExposed means the listener is (or may be) reachable beyond
	// loopback: a wildcard bind (empty host, "0.0.0.0", "::"), any
	// routable IP literal, or any hostname other than "localhost". A
	// hostname is classified WITHOUT resolution — fail-public: the
	// classifier cannot know what "myhost" resolves to at bind time, and
	// resolving here would race the bind itself. A zoned IPv6 literal
	// ("::1%lo") also lands here via the hostname path (net.ParseIP
	// rejects zones); binding one is exotic enough that the fail-public
	// reading is the safe one.
	BindExposed
)

// String returns the classification as a short lowercase word for log
// attributes: "invalid", "loopback", or "exposed".
func (c BindClass) String() string {
	switch c {
	case BindLoopback:
		return "loopback"
	case BindExposed:
		return "exposed"
	default:
		return "invalid"
	}
}

// ClassifyBind classifies a configured listen address of the "host:port"
// form net.Listen accepts. The HOST decides the class; the port is not
// validated (net.Listen owns that failure, with a better error than any
// pre-check here could give).
//
// An address net.SplitHostPort cannot split returns BindInvalid, and what
// that means is deliberately the caller's policy. The three shipped policies,
// as recipes:
//
//	// Fail-silent (web-terminal-kiro): an unparseable addr will fail at
//	// Listen anyway; only a definite exposure warrants the warning.
//	if webhttp.ClassifyBind(addr) == webhttp.BindExposed { warn(...) }
//
//	// Fail-public (pg-autodump): a spurious warning is preferable to a
//	// silently unflagged open endpoint.
//	if webhttp.ClassifyBind(addr) != webhttp.BindLoopback { warn(...) }
//
//	// Classify-the-unsplit-input (web-terminal-server): a portless value
//	// like "127.0.0.1" is read as a bare host and classified anyway.
//	class := webhttp.ClassifyBind(addr)
//	if class == webhttp.BindInvalid { class = webhttp.ClassifyBindHost(addr) }
//	if class != webhttp.BindLoopback { warn(...) }
//
// No name resolution is performed and no repair is attempted; like
// CanonicalHost, the classifier reports what the input IS rather than
// guessing what the operator meant.
func ClassifyBind(addr string) BindClass {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return BindInvalid
	}
	return ClassifyBindHost(host)
}

// ClassifyBindHost classifies a bare bind host (no port): the host half of a
// listen address, or a whole portless bind value under the
// classify-the-unsplit-input recipe above. It never returns BindInvalid — a
// bare host always classifies: "localhost" (any case) and loopback IP
// literals are BindLoopback; everything else, including the empty string
// (the wildcard "listen on all interfaces" host), unspecified addresses
// ("0.0.0.0", "::"), routable IPs, and unresolved hostnames, is BindExposed.
func ClassifyBindHost(host string) BindClass {
	if strings.EqualFold(host, "localhost") {
		return BindLoopback
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return BindLoopback
	}
	return BindExposed
}
