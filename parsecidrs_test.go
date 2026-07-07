package webhttp_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cplieger/webhttp"
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
