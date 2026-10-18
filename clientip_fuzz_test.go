package webhttp_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/webhttp/v4"
)

// FuzzClientIP_spoofProofAndCleanOutput holds three invariants over a raw
// X-Forwarded-For: with no trusted set the result is always the peer; with
// the peer trusted the result carries no comma, whitespace or CR/LF; and it
// equals oracleClientIP, an independent walk of the documented contract, so
// a walk that falls back to the peer, runs the wrong way or skips no hop fails.
func FuzzClientIP_spoofProofAndCleanOutput(f *testing.F) {
	for _, s := range []string{
		"", "1.2.3.4", "1.2.3.4, 10.0.0.1", "  1.2.3.4 , 10.0.0.1",
		"garbage", "1.2.3.4, 2001:db8::1", "\r\nInjected", "a,,b,",
		strings.Repeat("1.2.3.4,", 50), strings.Repeat("1.2.3.4,", 65), "10.0.0.2, 10.0.0.3",
		"9.9.9.9, 8.8.8.8, 10.0.0.1", "2001:db8::1, 10.0.0.5",
		"5.6.7.8, 10.0.0.9, 1.2.3.4", "1.2.3.4 ,\t10.0.0.1",
		"::ffff:1.2.3.4", "fe80::1%eth0", "1.2.3.4%eth0",
		"1.2.3.4,,10.0.0.1", "1.2.3.4, ", "\v1.2.3.4", "\"1.2.3.4, 10.0.0.1",
	} {
		f.Add(s)
	}
	const trustedCIDR = "10.0.0.0/8"
	trusted := netip.MustParsePrefix(trustedCIDR)
	_, trustedNet, err := net.ParseCIDR(trustedCIDR)
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
		if strings.ContainsAny(got, ", \t\r\n") {
			t.Fatalf("trusted-peer ClientIP = %q carries an unclean byte (xff=%q)", got, xff)
		}
		if want := oracleClientIP(peerHost, xff, trustedNet); got != want {
			t.Fatalf("trusted-peer ClientIP = %q, oracle = %q (xff=%q)", got, want, xff)
		}
	})
}

// oracleClientIP is ClientIP's contract for a trusted peer, written on net.IP
// and a reversed copy ranged forward, so a bug shared with the netip walk
// cannot mask itself: RFC 9110 list elements (SP and HTAB trimmed, empty ones
// dropped), at most 64 of them, each an address alone, walked right to left
// past trusted hops.
func oracleClientIP(peer, xff string, trusted *net.IPNet) string {
	var elems []string
	for e := range strings.SplitSeq(xff, ",") {
		if e = strings.Trim(e, " \t"); e != "" {
			elems = append(elems, e)
		}
	}
	if len(elems) > 64 {
		return peer
	}
	slices.Reverse(elems)
	for _, e := range elems {
		ip := net.ParseIP(e)
		if ip == nil {
			break
		}
		if !trusted.Contains(ip) {
			return ip.String()
		}
	}
	return peer
}

// FuzzResolveClient_ipFromPeerOrHeader holds ResolveClient's provenance for
// any peer and header bytes, each "\n" starting a new field line: every field
// equals derivedResolution over the request's parsed forwarding fields, and
// ClientIP is its IP as text; a header's field lines resolve as their comma
// join does (RFC 9110 section 5.2); a forwarded IP is in the raw
// X-Forwarded-For; and UntrustedForwarding needs a header holding a list element.
func FuzzResolveClient_ipFromPeerOrHeader(f *testing.F) {
	for _, tc := range provenanceCases {
		f.Add(tc.remote, tc.xff, tc.realIP, tc.fwdHeader)
	}
	// Each header is non-blank, but "\xc2" and "\xa0" join into U+00A0.
	f.Add("0", "\xc2", "\xa0", " ")
	f.Add("192.168.1.50:4000", "\"192.168.1.20\n203.0.113.9\"", "\n198.51.100.7", "for=10.0.0.9\nfor=\"[2001:db8::1]:_p\"")
	f.Add("10.0.0.1:4000", "8.8.8.8\n\n10.0.0.2", "203.0.113.9\n", "by=\"x\nfor=198.51.100.7;\"")
	f.Add("192.168.1.50:4000", "", "", "for=198.51.100.7;FOR=10.0.0.1\nfor=\"198.51.100.7:80\";by=_x")
	f.Add("10.0.0.1:4000", strings.Repeat("203.0.113.9,", 40)+"\n"+strings.Repeat("10.0.0.2,", 25), strings.Repeat("203.0.113.9,", 65), "for=203.0.113.9;a=1;b=1;c=1;d=1;e=1;f=1;g=1;h=1;i=1;j=1;k=1;l=1;m=1;n=1;o=1;p=1")
	f.Fuzz(func(t *testing.T, remote, xff, realIP, forwarded string) {
		headers := map[string]string{"X-Forwarded-For": xff, "X-Real-IP": realIP, "Forwarded": forwarded}
		r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		r.RemoteAddr = remote
		joined := r.Clone(t.Context())
		for name, v := range headers {
			for line := range strings.SplitSeq(v, "\n") {
				r.Header.Add(name, line)
			}
			joined.Header.Set(name, strings.ReplaceAll(v, "\n", ","))
		}
		got := webhttp.ResolveClient(r, provenanceTrusted...)
		if want := derivedResolution(remote, webhttp.ParseForwarding(r.Header), provenanceTrusted); got != want {
			t.Fatalf("ResolveClient = %+v, derived from its parse %+v (remote=%q, headers=%q)", got, want, remote, headers)
		}
		if ip := webhttp.ClientIP(r, provenanceTrusted...); ip != got.IP.String() {
			t.Fatalf("ClientIP = %q, ResolveClient's IP %v (remote=%q, headers=%q)", ip, got.IP, remote, headers)
		}
		if one := webhttp.ResolveClient(joined, provenanceTrusted...); got != one {
			t.Fatalf("field lines resolve to %+v, their comma join to %+v (headers=%q)", got, one, headers)
		}
		if got.Forwarded && !slices.ContainsFunc(r.Header.Values("X-Forwarded-For"), func(line string) bool {
			return slices.ContainsFunc(strings.Split(line, ","), func(e string) bool {
				a, err := netip.ParseAddr(strings.Trim(e, " \t"))
				return err == nil && a.Unmap() == got.IP
			})
		}) {
			t.Fatalf("forwarded IP %v is in no X-Forwarded-For entry (xff=%q)", got.IP, xff)
		}
		if got.UntrustedForwarding && !slices.ContainsFunc([]string{xff, realIP, forwarded}, func(v string) bool {
			return strings.Trim(v, " \t,\n") != ""
		}) {
			t.Fatalf("UntrustedForwarding with no list element in any header (headers=%q)", headers)
		}
	})
}
