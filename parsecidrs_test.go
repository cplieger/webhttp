package webhttp_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cplieger/webhttp/v2"
)

func TestParseCIDRs(t *testing.T) {
	cases := []struct {
		name        string
		in          []string
		wantNets    int
		wantInvalid []string
	}{
		{"empty", nil, 0, nil},
		{"blank and whitespace skipped", []string{"", "  ", "\t"}, 0, nil},
		{"cidr v4", []string{"10.0.0.0/8"}, 1, nil},
		{"cidr v6", []string{"2001:db8::/32"}, 1, nil},
		{"bare ipv4 becomes host route", []string{"192.168.1.5"}, 1, nil},
		{"bare ipv6 becomes host route", []string{"::1"}, 1, nil},
		{"trims surrounding whitespace", []string{"  10.0.0.0/8  "}, 1, nil},
		{"mixed valid", []string{"10.0.0.0/8", "172.16.0.1", "fd00::/8"}, 3, nil},
		{"collects invalid, keeps valid", []string{"10.0.0.0/8", "nope", "999.999.0.0/8"}, 1, []string{"nope", "999.999.0.0/8"}},
		{"all invalid", []string{"garbage"}, 0, []string{"garbage"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nets, invalid := webhttp.ParseCIDRs(tc.in)
			if len(nets) != tc.wantNets {
				t.Errorf("got %d nets, want %d", len(nets), tc.wantNets)
			}
			if len(invalid) != len(tc.wantInvalid) {
				t.Fatalf("got invalid %v, want %v", invalid, tc.wantInvalid)
			}
			for i := range invalid {
				if invalid[i] != tc.wantInvalid[i] {
					t.Errorf("invalid[%d] = %q, want %q", i, invalid[i], tc.wantInvalid[i])
				}
			}
		})
	}
}

// The parsed set must actually drive ClientIP's trusted-proxy resolution: a
// peer inside a parsed CIDR resolves the real client from X-Forwarded-For.
func TestParseCIDRs_feedsClientIP(t *testing.T) {
	nets, invalid := webhttp.ParseCIDRs([]string{"192.0.2.0/24"})
	if len(invalid) != 0 || len(nets) != 1 {
		t.Fatalf("ParseCIDRs = %d nets, %v invalid; want 1 net, no invalid", len(nets), invalid)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.1:1234" // trusted proxy peer
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := webhttp.ClientIP(r, nets...); got != "203.0.113.9" {
		t.Errorf("ClientIP = %q, want the forwarded client 203.0.113.9", got)
	}
	// A bare-IP entry (host route) trusts exactly that peer.
	only, _ := webhttp.ParseCIDRs([]string{"192.0.2.1"})
	if got := webhttp.ClientIP(r, only...); got != "203.0.113.9" {
		t.Errorf("ClientIP with /32 = %q, want 203.0.113.9", got)
	}
}

// TestParseCIDRs_v4in6PeerMatchesIPv4Entry pins the IPv4-in-IPv6 equivalence
// the trusted-proxy set relies on, which nothing else asserted.
//
// It matters operationally: a dual-stack listener delivers an IPv4 proxy's
// r.RemoteAddr as "::ffff:10.0.0.7", so if that did not match a "10.0.0.0/8"
// entry the peer would read as untrusted, X-Forwarded-For would be ignored, and
// every access line would record the proxy instead of the client. It fails in
// the safe direction, which is exactly why it would go unnoticed.
//
// It matters for the implementation too, and that is the durable reason to pin
// it. net.IPNet.Contains normalizes both spellings for free, so the current code
// gets this right without trying (ParseCIDRs pairs a 16-byte 4-in-6 IP with a
// 4-byte mask for a bare IPv4 entry, and Contains still answers correctly). A
// net/netip port does NOT: measured on go1.27.0,
// netip.PrefixFrom(netip.MustParseAddr("::ffff:203.0.113.7"), 128).
// Contains(netip.MustParseAddr("203.0.113.7")) is false, because netip.Addr
// keeps Is4In6 distinct from Is4 and a faithful port would have to call Unmap()
// on both the entry and the peer. This test is what would catch that.
func TestParseCIDRs_v4in6PeerMatchesIPv4Entry(t *testing.T) {
	nets, invalid := webhttp.ParseCIDRs([]string{"10.0.0.0/8", "192.0.2.1", "::1"})
	if len(invalid) != 0 || len(nets) != 3 {
		t.Fatalf("ParseCIDRs = %d nets, %v invalid; want 3 nets, no invalid", len(nets), invalid)
	}

	const client = "203.0.113.9"
	cases := []struct {
		name, peer string
		wantClient string
	}{
		{"plain IPv4 peer in a CIDR entry", "10.0.0.7", client},
		{"4-in-6 peer in the same CIDR entry", "::ffff:10.0.0.7", client},
		{"plain IPv4 peer as a bare host entry", "192.0.2.1", client},
		{"4-in-6 peer against that bare host entry", "::ffff:192.0.2.1", client},
		{"IPv6 loopback entry, canonical spelling", "::1", client},
		{"IPv6 loopback entry, expanded spelling", "0:0:0:0:0:0:0:1", client},
		// The negative direction, so the test is not vacuous: an untrusted peer
		// in either spelling keeps its own address and the header is ignored.
		{"untrusted plain IPv4 peer", "198.51.100.20", "198.51.100.20"},
		{"untrusted 4-in-6 peer", "::ffff:198.51.100.20", "::ffff:198.51.100.20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = net.JoinHostPort(tc.peer, "5000")
			r.Header.Set("X-Forwarded-For", client)
			if got := webhttp.ClientIP(r, nets...); got != tc.wantClient {
				t.Errorf("ClientIP(peer %s) = %q, want %q", tc.peer, got, tc.wantClient)
			}
		})
	}
}
