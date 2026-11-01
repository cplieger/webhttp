package webhttp_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"

	"github.com/cplieger/webhttp/v4"
)

func TestParseCIDRs(t *testing.T) {
	t.Parallel()
	p := netip.MustParsePrefix
	cases := []struct {
		name        string
		in          []string
		want        []netip.Prefix
		wantInvalid []string
	}{
		{"no entries", nil, nil, nil},
		{"empty entries are skipped", []string{"", "  ", "\t"}, nil, nil},
		{"a bare IPv4 address is a host prefix", []string{"192.168.1.5"}, []netip.Prefix{p("192.168.1.5/32")}, nil},
		{"a bare IPv6 address is a host prefix", []string{"2001:db8::1"}, []netip.Prefix{p("2001:db8::1/128")}, nil},
		{"an IPv4 CIDR", []string{"10.0.0.0/8"}, []netip.Prefix{p("10.0.0.0/8")}, nil},
		{"an IPv6 CIDR", []string{"2001:db8::/32"}, []netip.Prefix{p("2001:db8::/32")}, nil},
		{"host bits are masked", []string{"10.1.2.3/8", "2001:db8::1/32"}, []netip.Prefix{p("10.0.0.0/8"), p("2001:db8::/32")}, nil},
		{"an IPv4-mapped CIDR is unmapped", []string{"::ffff:10.0.0.0/104"}, []netip.Prefix{p("10.0.0.0/8")}, nil},
		{"an IPv4-mapped address is unmapped", []string{"::ffff:192.0.2.1"}, []netip.Prefix{p("192.0.2.1/32")}, nil},
		{"the whole mapped range is all of IPv4", []string{"::ffff:0.0.0.0/96"}, []netip.Prefix{p("0.0.0.0/0")}, nil},
		{"surrounding whitespace is trimmed", []string{"  10.0.0.0/8\t"}, []netip.Prefix{p("10.0.0.0/8")}, nil},
		{
			"invalid entries come back in input order",
			[]string{"nope", "10.0.0.0/8", " 999.999.0.0/8", "fe80::1%eth0", "10.0.0.0/33", "fe80::/10%eth0"},
			[]netip.Prefix{p("10.0.0.0/8")},
			[]string{"nope", "999.999.0.0/8", "fe80::1%eth0", "10.0.0.0/33", "fe80::/10%eth0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, invalid := webhttp.ParseCIDRs(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Errorf("prefixes = %v, want %v", got, tc.want)
			}
			if !slices.Equal(invalid, tc.wantInvalid) {
				t.Errorf("invalid = %q, want %q", invalid, tc.wantInvalid)
			}
		})
	}
}

// A dual-stack listener delivers an IPv4 proxy's RemoteAddr as
// "::ffff:10.0.0.7"; a parsed IPv4 entry must still trust it, or every
// request reads as coming from the proxy.
func TestParseCIDRsTrustsBothPeerSpellings(t *testing.T) {
	t.Parallel()
	prefixes, invalid := webhttp.ParseCIDRs([]string{"10.0.0.0/8", "192.0.2.1", "::1", "::ffff:172.16.0.0/108"})
	if len(invalid) != 0 || len(prefixes) != 4 {
		t.Fatalf("ParseCIDRs = %v, %v invalid; want 4 prefixes, no invalid", prefixes, invalid)
	}
	const client = "203.0.113.9"
	cases := []struct {
		name, peer string
		wantClient string
	}{
		{"plain IPv4 peer in a CIDR entry", "10.0.0.7", client},
		{"mapped peer in the same CIDR entry", "::ffff:10.0.0.7", client},
		{"plain IPv4 peer as a bare host entry", "192.0.2.1", client},
		{"mapped peer against that bare host entry", "::ffff:192.0.2.1", client},
		{"plain IPv4 peer in a mapped CIDR entry", "172.16.0.9", client},
		{"IPv6 loopback entry, canonical spelling", "::1", client},
		{"IPv6 loopback entry, expanded spelling", "0:0:0:0:0:0:0:1", client},
		{"untrusted plain IPv4 peer", "198.51.100.20", "198.51.100.20"},
		{"untrusted mapped peer reads as IPv4", "::ffff:198.51.100.20", "198.51.100.20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
			r.RemoteAddr = net.JoinHostPort(tc.peer, "5000")
			r.Header.Set("X-Forwarded-For", client)
			if got := webhttp.ClientIP(r, prefixes...); got != tc.wantClient {
				t.Errorf("ClientIP(peer %s) = %q, want %q", tc.peer, got, tc.wantClient)
			}
		})
	}
}
