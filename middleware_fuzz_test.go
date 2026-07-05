package webhttp_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/webhttp"
)

// FuzzClientIP_spoofProofAndCleanOutput fuzzes the raw, attacker-controlled
// X-Forwarded-For header against three strong security invariants of ClientIP:
//
//  1. Spoof-proof default: with NO trusted proxies the result is ALWAYS the
//     socket peer regardless of XFF, so no client-sent header can move the
//     result off the real peer.
//  2. Clean output: with the peer trusted, every returned value is a normalized
//     net.IP.String() or the peer host and never carries a comma, whitespace,
//     or CR/LF from the raw header into a log line or a downstream header.
//  3. Selection contract: with the peer trusted, the result equals an
//     INDEPENDENT right-to-left, skip-trusted-hop reimplementation of the
//     contract (oracleClientIP, which never calls ClientIP). This pins the
//     actual hop-selection logic, so a regression that always falls back to the
//     peer, walks the wrong direction, fails to skip an own hop, or returns the
//     wrong boundary entry fails the target instead of slipping past the
//     cleanliness check.
//
// The seed corpus includes adversarial inputs: a CR/LF-prefixed value, empty
// and doubled commas, an overlong repeated chain, an all-trusted-hops chain
// that must fall back to the peer, a left-spoofed prefix ahead of a real
// boundary, mixed IPv4/IPv6 hops, and whitespace-padded entries, so the walk's
// selection logic is exercised, not only its output formatting.
func FuzzClientIP_spoofProofAndCleanOutput(f *testing.F) {
	for _, s := range []string{
		"", "1.2.3.4", "1.2.3.4, 10.0.0.1", "  1.2.3.4 , 10.0.0.1",
		"garbage", "1.2.3.4, 2001:db8::1", "\r\nInjected", "a,,b,",
		strings.Repeat("1.2.3.4,", 50), "10.0.0.2, 10.0.0.3",
		// Selection-contract seeds: a left-spoofed prefix ahead of the real
		// boundary, an IPv6 boundary behind a trusted hop, a trusted hop
		// between two untrusted entries, and tab/space-padded fields.
		"9.9.9.9, 8.8.8.8, 10.0.0.1", "2001:db8::1, 10.0.0.5",
		"5.6.7.8, 10.0.0.9, 1.2.3.4", "1.2.3.4 ,\t10.0.0.1",
	} {
		f.Add(s)
	}
	_, trusted, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, xff string) {
		const peerHost = "10.0.0.1"
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = peerHost + ":9999"
		req.Header.Set("X-Forwarded-For", xff)

		if got := webhttp.ClientIP(req); got != peerHost {
			t.Fatalf("no trusted set: ClientIP = %q, want peer %q (xff=%q)", got, peerHost, xff)
		}

		got := webhttp.ClientIP(req, trusted)
		// Cleanliness: the returned token never leaks a raw-header byte.
		if strings.ContainsAny(got, ", \t\r\n") {
			t.Fatalf("trusted-peer ClientIP = %q carries an unclean byte (xff=%q)", got, xff)
		}
		// Selection contract: with the peer trusted, ClientIP must return
		// exactly what an INDEPENDENT right-to-left, skip-trusted-hop walk of
		// the same header yields. This catches selection regressions (always
		// falling back to the peer, walking left-to-right, failing to skip an
		// own hop, or picking the wrong boundary entry) that the cleanliness
		// check alone would pass. The oracle is derived from ClientIP's
		// documented contract and never calls ClientIP.
		if want := oracleClientIP(req.RemoteAddr, xff, []*net.IPNet{trusted}); got != want {
			t.Fatalf("trusted-peer ClientIP = %q, oracle = %q (xff=%q)", got, want, xff)
		}
	})
}

// oracleClientIP is an independent reference reimplementation of ClientIP's
// documented right-to-left trusted-hop selection contract, used only to check
// ClientIP against a second, differently-structured implementation. It does
// NOT call webhttp.ClientIP. Contract: resolve the peer host from RemoteAddr;
// if the peer is unparseable or not in the trusted set, return the peer host
// (spoof-proof default); otherwise scan X-Forwarded-For from the RIGHT, skip
// entries that are themselves trusted hops, stop at the first malformed entry,
// and return the first untrusted parseable entry (normalized via IP.String()),
// falling back to the peer host when no usable entry remains. Written with a
// reversed fresh copy ranged forward and inlined containment checks (not the
// SUT's slices.Backward / a shared helper) so a shared copy-paste bug cannot
// mask a real selection regression.
func oracleClientIP(remoteAddr, xff string, trusted []*net.IPNet) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr // no host:port form; use verbatim (never trusted).
	}
	contained := func(ip net.IP) bool {
		for _, n := range trusted {
			if n != nil && n.Contains(ip) {
				return true
			}
		}
		return false
	}
	peer := net.ParseIP(host)
	if peer == nil || !contained(peer) {
		return host // spoof-proof default: peer untrusted or unparseable.
	}
	if xff != "" {
		// Reverse a fresh copy of the split header and range it FORWARD, a
		// distinct iteration mechanism from ClientIP's slices.Backward, so the
		// oracle checks the same right-to-left contract via different code.
		rev := slices.Clone(strings.Split(xff, ","))
		slices.Reverse(rev)
		for _, part := range rev {
			ip := net.ParseIP(strings.TrimSpace(part))
			if ip == nil {
				break // malformed boundary: stop, trust nothing further left.
			}
			if contained(ip) {
				continue // our own trusted hop: keep walking left.
			}
			return ip.String() // first untrusted entry from the right.
		}
	}
	return host // trusted peer, no usable forwarded entry: fall back to peer.
}
