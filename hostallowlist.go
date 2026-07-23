package webhttp

import (
	"net"
	"net/http"
	"strings"
)

// CanonicalHost normalizes an HTTP Host header value (or a configured allowlist
// entry) for exact comparison: strip an optional :port, strip IPv6 brackets,
// lowercase, trim a trailing FQDN dot, and canonicalize IP literals through
// net.ParseIP so textually different spellings of the same address (::1 vs
// 0:0:0:0:0:0:0:1, 127.0.0.1 vs 127.0.0.001) compare equal.
//
// It returns "" for a value that carries no host — a lone port (":9848"), the
// empty string, or "[]" — which HostPolicy treats as an unusable entry. The
// function is idempotent: CanonicalHost(CanonicalHost(x)) == CanonicalHost(x).
//
// No name resolution is performed. Resolving a hostname to compare against the
// request would reopen the very DNS-rebinding race a host allowlist exists to
// close (the attacker controls what the name resolves to), so matching is
// purely textual on the canonicalized Host.
func CanonicalHost(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	// Unwrap a bracketed IPv6 literal (SplitHostPort leaves "[::1]" bracketed
	// when there was no :port to split).
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	host = strings.TrimRight(strings.ToLower(host), ".")
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	// Not an IP literal. A legal registered hostname carries no brackets and no
	// colon (an IPv6 literal was caught by ParseIP above), so any that remain
	// are malformed input ("[[", "[a:b]" -> "a:b", "0.:0"). Strip every stray
	// bracket, drop the port-like colon suffix, and trim trailing dots the
	// strips may expose, so canonicalization reaches a fixed point. Without this
	// a second pass would strip the next leftover bracket, re-split the residual
	// colon, or re-trim an exposed dot and return a different value, breaking
	// idempotence (each variant found by fuzzing).
	host = bracketStripper.Replace(host)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return strings.TrimRight(host, ".")
}

// bracketStripper removes every square bracket from a malformed non-IP host
// candidate (a well-formed bracketed IPv6 literal is handled before it runs).
var bracketStripper = strings.NewReplacer("[", "", "]", "")

// HostAllowlistOption configures a HostPolicy built by ParseHostList.
type HostAllowlistOption func(*hostPolicyConfig)

// hostPolicyConfig holds resolved HostPolicy configuration.
type hostPolicyConfig struct {
	code           string
	msg            string
	loopbackExempt bool
}

// WithLoopbackExempt admits a request whenever BOTH its socket peer and its
// Host are loopback, regardless of the allowlist — the container-internal
// carve-out. It keeps a service's own loopback clients working under any
// allowlist: a baked Docker healthcheck (curl http://127.0.0.1:PORT), an
// in-container agent hitting localhost, a sidecar probe. Without it, an
// allowlist of browser-facing hostnames rejects those callers and can brick a
// health signal.
//
// The carve-out is unreachable by the attacks a host allowlist defends against.
// A DNS-rebinding request carries the ATTACKER'S hostname in Host, so it fails
// the loopback-Host test even when it originates from a browser on this same
// machine (a loopback peer); and a remote client forging Host: 127.0.0.1 is not
// a loopback socket peer, so it fails the peer test. Both conditions must hold,
// and only genuinely local traffic can satisfy both.
//
// Off by default: a bare allowlist rejects everything not listed, loopback
// included. Opt in when the service runs behind a loopback health check or
// serves in-container clients.
func WithLoopbackExempt() HostAllowlistOption {
	return func(c *hostPolicyConfig) { c.loopbackExempt = true }
}

// WithHostAllowlistError sets the error code and message written in the 403
// JSON envelope (via WriteError) when a request's Host is not allowed. Defaults
// to "host_not_allowed" / "host not allowed". Override it to name the app's own
// configuration knob, e.g. "host not allowed; add it to KWEB_ALLOWED_HOSTS".
func WithHostAllowlistError(code, msg string) HostAllowlistOption {
	return func(c *hostPolicyConfig) {
		c.code, c.msg = code, msg
	}
}

// HostPolicy is an immutable, canonicalized exact-match Host allowlist. Build
// one with ParseHostList and apply it with Middleware; the same policy answers
// the per-request decision through Allows. The zero value and a nil pointer are
// both inactive (permissive), so a policy parsed from unset configuration is a
// safe pass-through.
type HostPolicy struct {
	allowed        map[string]struct{}
	code           string
	msg            string
	loopbackExempt bool
	active         bool
}

// ParseHostList parses operator-supplied allowlist entries (a config array or a
// comma-split environment variable) into a HostPolicy, returning the entries it
// could not use separately — a shape deliberately identical to ParseCIDRs so a
// strict caller can reject bad input while a lenient caller logs it and
// proceeds.
//
// Each entry is trimmed; a blank entry is skipped and does not engage the gate.
// A usable entry is canonicalized via CanonicalHost and added to the allowlist.
// An entry is reported as invalid (returned, never added) when it contains "/"
// (a pasted URL like http://host, a path, or a CIDR confused with a proxy list)
// or canonicalizes to the empty host (a lone ":9848" that belongs in the bind
// address, ".", "[]").
//
// Activation, and the fail-closed contract: the policy is INACTIVE only when no
// non-blank entry was supplied at all (unset or all-whitespace configuration) —
// then Middleware is a pass-through and Allows returns true, the
// backward-compatible "accept every Host" default. As soon as ANY non-blank
// entry is supplied the gate is ACTIVE, even if every entry was invalid: a
// misconfigured allowlist (all entries malformed) then rejects every request
// rather than silently disabling the protection. Because invalid entries are
// returned rather than retained as unmatchable keys, a caller that wants to
// fail loud can inspect the invalid slice at startup and refuse to boot; a
// caller that wants to proceed logs it and runs with the valid subset (or, when
// the subset is empty, a fail-closed deny-all).
//
// Pair it with WithLoopbackExempt for a containerized service whose own
// loopback health check or in-container clients must keep working, and with
// WithHostAllowlistError to name the app's configuration knob in the 403.
func ParseHostList(entries []string, opts ...HostAllowlistOption) (policy *HostPolicy, invalid []string) {
	c := &hostPolicyConfig{code: "host_not_allowed", msg: "host not allowed"}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	p := &HostPolicy{
		allowed:        make(map[string]struct{}),
		code:           c.code,
		msg:            c.msg,
		loopbackExempt: c.loopbackExempt,
	}
	for _, e := range entries {
		s := strings.TrimSpace(e)
		if s == "" {
			continue // a blank entry never engages the gate
		}
		p.active = true // any non-blank entry engages the gate (fail closed)
		key := CanonicalHost(s)
		if key == "" || strings.Contains(s, "/") {
			invalid = append(invalid, s)
			continue
		}
		p.allowed[key] = struct{}{}
	}
	return p, invalid
}

// Active reports whether the gate is engaged. A policy parsed from unset or
// all-blank configuration is inactive: Middleware is a pass-through and Allows
// returns true for every request. A nil policy is inactive.
func (p *HostPolicy) Active() bool {
	return p != nil && p.active
}

// Size reports the number of valid, canonicalized hosts in the allowlist
// (excluding entries ParseHostList reported as invalid). A nil policy has size
// zero.
func (p *HostPolicy) Size() int {
	if p == nil {
		return 0
	}
	return len(p.allowed)
}

// Allows reports whether the request is admitted. An inactive (or nil) policy
// admits every request. An active policy admits a request only when its
// canonicalized Host is in the allowlist, or — when WithLoopbackExempt was set
// — when both the socket peer and the Host are loopback. r.Host is the only
// host value consulted; X-Forwarded-Host is deliberately ignored (it is
// client-controlled and this check must hold on the direct-exposure path).
func (p *HostPolicy) Allows(r *http.Request) bool {
	if p == nil || !p.active {
		return true
	}
	host := CanonicalHost(r.Host)
	if _, ok := p.allowed[host]; ok {
		return true
	}
	if p.loopbackExempt && isLoopbackHost(host) && isLoopbackPeer(r.RemoteAddr) {
		return true
	}
	return false
}

// Middleware returns the allowlist as webhttp middleware. An inactive (or nil)
// policy returns the next handler unwrapped — the "off" contract shared with
// RateLimiter and RouteTimeout, so a policy from unset configuration adds no
// overhead. An active policy rejects a request whose Host is not allowed with a
// 403 via WriteError (code and message set by WithHostAllowlistError), before
// the next handler runs.
//
// Placement: put it OUTERMOST among request-authorization middleware, before a
// cross-origin or CSRF check, so a disallowed host is rejected before any route
// runs — a DNS-rebinding request makes Origin and Host agree, so a same-origin
// check alone would admit it; the exact-Host allowlist is what breaks that
// chain (CWE-346).
func (p *HostPolicy) Middleware() Middleware {
	if p == nil || !p.active {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p.Allows(r) {
				next.ServeHTTP(w, r)
				return
			}
			WriteError(w, r, http.StatusForbidden, p.code, p.msg)
		})
	}
}

// isLoopbackHost reports whether a CanonicalHost-normalized value names the
// local host: the literal "localhost" or any loopback IP (127.0.0.0/8, ::1).
func isLoopbackHost(canon string) bool {
	if canon == "localhost" {
		return true
	}
	ip := net.ParseIP(canon)
	return ip != nil && ip.IsLoopback()
}

// isLoopbackPeer reports whether an http.Request.RemoteAddr belongs to a
// loopback socket peer. RemoteAddr is set by the server from the accepted
// connection, so it cannot be spoofed at this layer; forwarded headers play no
// part. A malformed value fails closed (not loopback).
func isLoopbackPeer(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
