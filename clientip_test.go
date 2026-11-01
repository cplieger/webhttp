package webhttp_test

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/webhttp/v4"
)

var provenanceTrusted = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("2001:db8:a::/48"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("::ffff:172.16.0.0/108"),
}

type provenanceCase struct {
	name                      string
	remote                    string
	xff, realIP, fwdHeader    string
	ip, peer                  string
	forwarded, untrusted, pub bool
}

var provenanceCases = []provenanceCase{
	{
		name: "trusted peer with no header answers the peer", remote: "10.0.0.1:4000",
		ip: "10.0.0.1", peer: "10.0.0.1",
	},
	{
		name: "trusted peer forwarding a private client", remote: "10.0.0.1:4000", xff: "192.168.1.20",
		ip: "192.168.1.20", peer: "10.0.0.1", forwarded: true,
	},
	{
		name: "two trusted hops walk right to left", remote: "10.0.0.1:4000", xff: "192.168.1.20, 10.0.0.2",
		ip: "192.168.1.20", peer: "10.0.0.1", forwarded: true,
	},
	{
		name: "a spoofed left entry is not the client", remote: "10.0.0.1:4000", xff: "8.8.8.8, 192.168.1.20, 10.0.0.2",
		ip: "192.168.1.20", peer: "10.0.0.1", forwarded: true,
	},
	{
		name: "repeated header lines join as one list", remote: "10.0.0.1:4000", xff: "8.8.8.8\n203.0.113.9",
		ip: "203.0.113.9", peer: "10.0.0.1", forwarded: true,
	},
	{
		name: "trusted hops only answer the peer", remote: "10.0.0.1:4000", xff: "10.0.0.3, 10.0.0.2",
		ip: "10.0.0.1", peer: "10.0.0.1",
	},
	{
		name: "a malformed boundary entry answers the peer", remote: "10.0.0.1:4000", xff: "192.168.1.20, junk",
		ip: "10.0.0.1", peer: "10.0.0.1",
	},
	{
		name: "a zoned entry is malformed", remote: "10.0.0.1:4000", xff: "fe80::9%eth0",
		ip: "10.0.0.1", peer: "10.0.0.1",
	},
	{
		name: "X-Real-IP from a trusted peer is not read", remote: "10.0.0.1:4000", realIP: "192.168.1.20",
		ip: "10.0.0.1", peer: "10.0.0.1",
	},
	{
		name: "an IPv4-mapped peer matches its IPv4 prefix", remote: "[::ffff:10.0.0.1]:4000", xff: "::ffff:192.168.1.20",
		ip: "192.168.1.20", peer: "10.0.0.1", forwarded: true,
	},
	{
		name: "an IPv4 peer matches an IPv4-mapped prefix", remote: "172.16.0.5:4000", xff: "192.168.1.20",
		ip: "192.168.1.20", peer: "172.16.0.5", forwarded: true,
	},
	{
		name: "an IPv6 trusted peer", remote: "[2001:db8:a::1]:4000", xff: "fd00::5",
		ip: "fd00::5", peer: "2001:db8:a::1", forwarded: true,
	},
	{
		name: "untrusted peer sending a private X-Forwarded-For", remote: "192.168.1.50:4000", xff: "192.168.1.20",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "untrusted peer sending a public X-Forwarded-For", remote: "192.168.1.50:4000", xff: "192.168.1.20, 203.0.113.9",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true, pub: true,
	},
	{
		name: "untrusted peer sending a public entry with a port", remote: "192.168.1.50:4000", xff: "198.51.100.7:4711",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true, pub: true,
	},
	{
		name: "untrusted peer sending an IPv4-mapped public entry", remote: "192.168.1.50:4000", xff: "::ffff:198.51.100.7",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true, pub: true,
	},
	{
		name: "loopback, link-local and unspecified entries are not public", remote: "192.168.1.50:4000", xff: "127.0.0.1, fe80::1, ::1, 0.0.0.0",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "shared address space is public", remote: "192.168.1.50:4000", xff: "100.64.0.1",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true, pub: true,
	},
	{
		name: "untrusted peer sending a private X-Real-IP", remote: "192.168.1.50:4000", realIP: "10.1.2.3",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "untrusted peer sending a public X-Real-IP", remote: "192.168.1.50:4000", realIP: "203.0.113.9",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true, pub: true,
	},
	{
		name: "untrusted peer sending a public Forwarded for", remote: "192.168.1.50:4000", fwdHeader: "for=192.0.2.60;proto=http;by=10.0.0.1",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true, pub: true,
	},
	{
		name: "a quoted bracketed Forwarded node with a port", remote: "192.168.1.50:4000", fwdHeader: `For="[2001:db8:cafe::17]:4711"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true, pub: true,
	},
	{
		name: "a quoted bracketed Forwarded node without a port", remote: "192.168.1.50:4000", fwdHeader: `for="[2001:db8:cafe::17]"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true, pub: true,
	},
	{
		name: "an IPv4-mapped private entry is not public", remote: "192.168.1.50:4000", xff: "::ffff:10.1.2.3",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "an IPv4-mapped public node with a port", remote: "192.168.1.50:4000", xff: "[::ffff:198.51.100.7]:80",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true, pub: true,
	},
	{
		name: "a later Forwarded element names the public client", remote: "192.168.1.50:4000", fwdHeader: "for=192.168.0.9, for=198.51.100.7",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true, pub: true,
	},
	{
		name: "an obfuscated Forwarded node is not public", remote: "192.168.1.50:4000", fwdHeader: "for=unknown;by=192.0.2.1, for=_hidden",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded IPv4 node with an obfuscated port", remote: "192.168.1.50:4000", fwdHeader: `for="198.51.100.7:_proxy"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true, pub: true,
	},
	{
		name: "a Forwarded IPv6 node with an obfuscated port", remote: "192.168.1.50:4000", fwdHeader: `for="[2001:db8:cafe::17]:_p.x-1"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true, pub: true,
	},
	{
		name: "a Forwarded IPv4-mapped node is IPv6", remote: "192.168.1.50:4000", fwdHeader: `for="[::ffff:198.51.100.7]:80"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true, pub: true,
	},
	{
		name: "a Forwarded quoted-pair is decoded", remote: "192.168.1.50:4000", fwdHeader: `for="\198.51.100.7"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true, pub: true,
	},
	{
		name: "a Forwarded comma inside quotes splits nothing", remote: "192.168.1.50:4000", fwdHeader: `for="_x,for=203.0.113.9,"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded semicolon inside quotes splits nothing", remote: "192.168.1.50:4000", fwdHeader: `by="_x;for=203.0.113.9;"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded empty obfuscated port is malformed", remote: "192.168.1.50:4000", fwdHeader: `for="198.51.100.7:_"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded obfuscated port outside its alphabet is malformed", remote: "192.168.1.50:4000", fwdHeader: `for="198.51.100.7:_a+b"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded six-digit port is malformed", remote: "192.168.1.50:4000", fwdHeader: `for="198.51.100.7:123456"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded empty port is malformed", remote: "192.168.1.50:4000", fwdHeader: `for="[2001:db8:cafe::17]:"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded bracketed IPv4 node is malformed", remote: "192.168.1.50:4000", fwdHeader: `for="[198.51.100.7]"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded unbracketed IPv6 node is malformed", remote: "192.168.1.50:4000", fwdHeader: `for="2001:db8:cafe::17"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded zoned node is malformed", remote: "192.168.1.50:4000", fwdHeader: `for="[2001:db8:cafe::17%eth0]"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded bytes after the bracket are malformed", remote: "192.168.1.50:4000", fwdHeader: `for="[2001:db8:cafe::17]x"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded unclosed bracket is malformed", remote: "192.168.1.50:4000", fwdHeader: `for="[2001:db8:cafe::17"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded unterminated quote is malformed", remote: "192.168.1.50:4000", fwdHeader: `for="198.51.100.7`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded trailing escape is malformed", remote: "192.168.1.50:4000", fwdHeader: `for="198.51.100.7\`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded byte after the closing quote is malformed", remote: "192.168.1.50:4000", fwdHeader: `for="198.51.100.7"x`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded quoted-string spans field lines", remote: "192.168.1.50:4000", fwdHeader: "for=\"_foo\nbar\", for=198.51.100.7",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true, pub: true,
	},
	{
		name: "a Forwarded element repeating for is malformed", remote: "192.168.1.50:4000", fwdHeader: "for=192.168.0.9;for=198.51.100.7",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "an unquoted Forwarded node with a port is malformed", remote: "192.168.1.50:4000", fwdHeader: "for=198.51.100.7:80",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "an unquoted bracketed Forwarded node is malformed", remote: "192.168.1.50:4000", fwdHeader: "for=[2001:db8::1]",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded parameter repeated in another case is malformed", remote: "192.168.1.50:4000", fwdHeader: "for=198.51.100.7;by=10.0.0.1;BY=10.0.0.2",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded pair value outside the token alphabet is malformed", remote: "192.168.1.50:4000", fwdHeader: "for=198.51.100.7;proto=ht@p",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded pair with no name is malformed", remote: "192.168.1.50:4000", fwdHeader: "for=198.51.100.7;=http",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded space after a semicolon is malformed", remote: "192.168.1.50:4000", fwdHeader: "for=198.51.100.7; proto=http",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded by that is not a node is malformed", remote: "192.168.1.50:4000", fwdHeader: `for=198.51.100.7;by="proxy.example"`,
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a Forwarded control byte in a quoted-string is malformed", remote: "192.168.1.50:4000", fwdHeader: "for=198.51.100.7;ext=\"a\x01\"",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "Forwarded parameter names fold case", remote: "192.168.1.50:4000", fwdHeader: "FOR=198.51.100.7;Proto=https;By=_edge",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true, pub: true,
	},
	{
		name: "a Forwarded element of empty pairs names nothing", remote: "192.168.1.50:4000", fwdHeader: ";;",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "an empty X-Forwarded-For element is skipped", remote: "10.0.0.1:4000", xff: "203.0.113.9,,10.0.0.2",
		ip: "203.0.113.9", peer: "10.0.0.1", forwarded: true,
	},
	{
		name: "a trailing X-Forwarded-For comma is no entry", remote: "10.0.0.1:4000", xff: "203.0.113.9, ",
		ip: "203.0.113.9", peer: "10.0.0.1", forwarded: true,
	},
	{
		name: "an empty X-Forwarded-For line is no entry", remote: "10.0.0.1:4000", xff: "203.0.113.9\n\n10.0.0.2",
		ip: "203.0.113.9", peer: "10.0.0.1", forwarded: true,
	},
	{
		name: "a vertical tab is not optional whitespace", remote: "10.0.0.1:4000", xff: "\v203.0.113.9",
		ip: "10.0.0.1", peer: "10.0.0.1",
	},
	{
		name: "an X-Forwarded-For entry with a port answers the peer", remote: "10.0.0.1:4000", xff: "203.0.113.9:4711",
		ip: "10.0.0.1", peer: "10.0.0.1",
	},
	{
		name: "a stray quote does not swallow an appended entry", remote: "10.0.0.1:4000", xff: "\"evil, 203.0.113.9",
		ip: "203.0.113.9", peer: "10.0.0.1", forwarded: true,
	},
	{
		name: "a blank header is no evidence", remote: "192.168.1.50:4000", xff: "  ", realIP: "\t",
		ip: "192.168.1.50", peer: "192.168.1.50",
	},
	{
		name: "a no-break space is an entry, not whitespace", remote: "192.168.1.50:4000", realIP: "\u00a0",
		ip: "192.168.1.50", peer: "192.168.1.50", untrusted: true,
	},
	{
		name: "a header of empty list elements is no evidence", remote: "192.168.1.50:4000", xff: ",", realIP: " , \t", fwdHeader: "\n,",
		ip: "192.168.1.50", peer: "192.168.1.50",
	},
	{
		name: "a zoned peer is never trusted", remote: "[fe80::1%eth0]:4000", xff: "192.168.1.20",
		ip: "fe80::1%eth0", peer: "fe80::1%eth0", untrusted: true,
	},
	{
		name: "an unparseable peer is never trusted", remote: "garbage", xff: "203.0.113.9",
		untrusted: true, pub: true,
	},
}

func (tc provenanceCase) request() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	r.RemoteAddr = tc.remote
	for name, v := range map[string]string{"X-Forwarded-For": tc.xff, "X-Real-IP": tc.realIP, "Forwarded": tc.fwdHeader} {
		if v == "" {
			continue
		}
		for line := range strings.SplitSeq(v, "\n") {
			r.Header.Add(name, line)
		}
	}
	return r
}

func addrOrZero(t *testing.T, s string) netip.Addr {
	t.Helper()
	if s == "" {
		return netip.Addr{}
	}
	return netip.MustParseAddr(s)
}

func TestResolveClientReportsProvenance(t *testing.T) {
	t.Parallel()
	for _, tc := range provenanceCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := webhttp.ClientResolution{
				IP:                  addrOrZero(t, tc.ip),
				Peer:                addrOrZero(t, tc.peer),
				Forwarded:           tc.forwarded,
				UntrustedForwarding: tc.untrusted,
				IgnoredPublic:       tc.pub,
			}
			if got := webhttp.ResolveClient(tc.request(), provenanceTrusted...); got != want {
				t.Errorf("ResolveClient = %+v, want %+v", got, want)
			}
		})
	}
}

func TestResolveClientWithNoTrustedSetIgnoresEveryHeader(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	r.RemoteAddr = "10.0.0.1:4000"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	want := webhttp.ClientResolution{
		IP:                  netip.MustParseAddr("10.0.0.1"),
		Peer:                netip.MustParseAddr("10.0.0.1"),
		UntrustedForwarding: true,
		IgnoredPublic:       true,
	}
	if got := webhttp.ResolveClient(r); got != want {
		t.Errorf("ResolveClient = %+v, want %+v", got, want)
	}
}

// derivedResolution is ResolveClient's contract applied to the projection of
// its one parse of the forwarding fields, so a resolver that reads a header
// on its own, or drops a parsed fact, disagrees with it.
func derivedResolution(remote string, f webhttp.ForwardingFields, trusted []netip.Prefix) webhttp.ClientResolution {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	peer, err := netip.ParseAddr(host)
	if err == nil {
		peer = peer.Unmap()
	}
	isTrusted := func(a netip.Addr) bool {
		return slices.ContainsFunc(trusted, func(p netip.Prefix) bool {
			return p.Contains(a) || (a.Is4() && p.Contains(netip.AddrFrom16(a.As16())))
		})
	}
	isPublic := func(a netip.Addr) bool { return a.IsGlobalUnicast() && !a.IsPrivate() }
	res := webhttp.ClientResolution{IP: peer, Peer: peer}
	if !isTrusted(peer) {
		res.UntrustedForwarding = len(f.XFF)+len(f.RealIP)+len(f.Forwarded) > 0
		for _, e := range slices.Concat(f.XFF, f.RealIP) {
			res.IgnoredPublic = res.IgnoredPublic || isPublic(e.Addr)
		}
		for _, e := range f.Forwarded {
			res.IgnoredPublic = res.IgnoredPublic || (e.Valid && isPublic(e.For))
		}
		return res
	}
	for i := len(f.XFF) - 1; i >= 0 && f.XFF[i].Bare; i-- {
		if !isTrusted(f.XFF[i].Addr) {
			res.IP, res.Forwarded = f.XFF[i].Addr, true
			break
		}
	}
	return res
}

func TestResolveClientAgreesWithItsParse(t *testing.T) {
	t.Parallel()
	for _, tc := range provenanceCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := tc.request()
			want := derivedResolution(tc.remote, webhttp.ParseForwarding(r.Header), provenanceTrusted)
			if got := webhttp.ResolveClient(r, provenanceTrusted...); got != want {
				t.Errorf("ResolveClient = %+v, derived from its parse %+v", got, want)
			}
		})
	}
}

func headerOf(name string, lines ...string) http.Header {
	h := http.Header{}
	for _, l := range lines {
		h.Add(name, l)
	}
	return h
}

func TestParseForwardingReadsListElements(t *testing.T) {
	t.Parallel()
	a := netip.MustParseAddr
	cases := []struct {
		name  string
		lines []string
		want  []webhttp.ListEntry
	}{
		{"no field", nil, nil},
		{"only empty elements", []string{",", " , \t", ""}, nil},
		{"an empty element between two", []string{"192.0.2.1,,10.0.0.1"}, []webhttp.ListEntry{{a("192.0.2.1"), true}, {a("10.0.0.1"), true}}},
		{"field lines join in order", []string{"192.0.2.1", "", "10.0.0.1"}, []webhttp.ListEntry{{a("192.0.2.1"), true}, {a("10.0.0.1"), true}}},
		{"SP and HTAB are trimmed", []string{" 192.0.2.1 ,\t10.0.0.1\t"}, []webhttp.ListEntry{{a("192.0.2.1"), true}, {a("10.0.0.1"), true}}},
		{"other whitespace is kept", []string{"\v192.0.2.1"}, []webhttp.ListEntry{{}}},
		{"a mapped entry is unmapped", []string{"::ffff:192.0.2.1"}, []webhttp.ListEntry{{a("192.0.2.1"), true}}},
		{"a port names the address but is not bare", []string{"192.0.2.1:80"}, []webhttp.ListEntry{{a("192.0.2.1"), false}}},
		{"a bracketed IPv6 entry with a port", []string{"[2001:db8::1]:80"}, []webhttp.ListEntry{{a("2001:db8::1"), false}}},
		{"a quoted entry", []string{`"192.0.2.1"`}, []webhttp.ListEntry{{a("192.0.2.1"), false}}},
		{"a zoned entry names nothing", []string{"fe80::1%eth0"}, []webhttp.ListEntry{{}}},
		{"a comma inside quotes still splits", []string{`"192.0.2.1, 10.0.0.1"`}, []webhttp.ListEntry{{a("192.0.2.1"), false}, {a("10.0.0.1"), false}}},
		{"64 elements are read", []string{repeatList("192.0.2.1", 64)}, slices.Repeat([]webhttp.ListEntry{{a("192.0.2.1"), true}}, 64)},
		{"65 elements read as one entry naming nothing", []string{repeatList("192.0.2.1", 65)}, []webhttp.ListEntry{{}}},
		{"the cap counts across field lines", []string{repeatList("192.0.2.1", 33), repeatList("192.0.2.1", 32)}, []webhttp.ListEntry{{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := webhttp.ParseForwarding(headerOf("X-Forwarded-For", tc.lines...))
			if !slices.Equal(f.XFF, tc.want) {
				t.Errorf("X-Forwarded-For %q parses to %+v, want %+v", tc.lines, f.XFF, tc.want)
			}
			r := webhttp.ParseForwarding(headerOf("X-Real-IP", tc.lines...))
			if !slices.Equal(r.RealIP, tc.want) {
				t.Errorf("X-Real-IP %q parses to %+v, want %+v", tc.lines, r.RealIP, tc.want)
			}
		})
	}
}

func TestParseForwardingReadsForwardedElements(t *testing.T) {
	t.Parallel()
	a := netip.MustParseAddr
	invalid := webhttp.ForwardedElement{}
	cases := []struct {
		name  string
		lines []string
		want  []webhttp.ForwardedElement
	}{
		{"empty elements are dropped", []string{"for=192.0.2.1, ,for=192.0.2.2", ","}, []webhttp.ForwardedElement{{a("192.0.2.1"), true}, {a("192.0.2.2"), true}}},
		{"a quoted-string spans field lines", []string{`for="_a`, `b", for=192.0.2.1`}, []webhttp.ForwardedElement{invalid, {a("192.0.2.1"), true}}},
		{"an element without for is valid", []string{"proto=https;host=example.com"}, []webhttp.ForwardedElement{{Valid: true}}},
		{"a repeated for", []string{"for=192.0.2.1;for=192.0.2.2"}, []webhttp.ForwardedElement{invalid}},
		{"a repeated non-for parameter", []string{"for=192.0.2.1;proto=http;PROTO=https"}, []webhttp.ForwardedElement{invalid}},
		{"a bad token in a non-for pair", []string{"for=192.0.2.1;proto=ht[t]p"}, []webhttp.ForwardedElement{invalid}},
		{"a bad token as a parameter name", []string{"for=192.0.2.1;pro(to)=http"}, []webhttp.ForwardedElement{invalid}},
		{"an unquoted node-port", []string{"for=192.0.2.1:80"}, []webhttp.ForwardedElement{invalid}},
		{"an unquoted IPv6 node", []string{"for=[2001:db8::1]"}, []webhttp.ForwardedElement{invalid}},
		{"a quoted node-port", []string{`for="192.0.2.1:80"`}, []webhttp.ForwardedElement{{a("192.0.2.1"), true}}},
		{"a quoted IPv6 node", []string{`for="[2001:db8::1]"`}, []webhttp.ForwardedElement{{a("2001:db8::1"), true}}},
		{"a quoted mapped node is unmapped", []string{`for="[::ffff:192.0.2.1]"`}, []webhttp.ForwardedElement{{a("192.0.2.1"), true}}},
		{"an unknown node with a port", []string{`for="unknown:80"`}, []webhttp.ForwardedElement{{Valid: true}}},
		{"an obfuscated node with an obfuscated port", []string{`for="_node:_port"`}, []webhttp.ForwardedElement{{Valid: true}}},
		{"a value after the value", []string{`for="192.0.2.1"x`}, []webhttp.ForwardedElement{invalid}},
		{"a missing value", []string{"for=;proto=http"}, []webhttp.ForwardedElement{invalid}},
		{"a pair without =", []string{"for"}, []webhttp.ForwardedElement{invalid}},
		{"an unterminated quoted-string", []string{`for="192.0.2.1`}, []webhttp.ForwardedElement{invalid}},
		{"a quoted-pair is decoded", []string{`for="\1\9\2.0.2.1"`}, []webhttp.ForwardedElement{{a("192.0.2.1"), true}}},
		{"64 elements are read", []string{repeatList("for=192.0.2.1", 64)}, slices.Repeat([]webhttp.ForwardedElement{{a("192.0.2.1"), true}}, 64)},
		{"65 elements read as one invalid element", []string{repeatList("for=192.0.2.1", 65)}, []webhttp.ForwardedElement{invalid}},
		{"16 pairs are read", []string{forwardedPairs(16)}, []webhttp.ForwardedElement{{a("198.51.100.7"), true}}},
		{"17 pairs make the element invalid", []string{forwardedPairs(17)}, []webhttp.ForwardedElement{invalid}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := webhttp.ParseForwarding(headerOf("Forwarded", tc.lines...)).Forwarded; !slices.Equal(got, tc.want) {
				t.Errorf("Forwarded %q parses to %+v, want %+v", tc.lines, got, tc.want)
			}
		})
	}
}

func repeatList(elem string, n int) string {
	return strings.Join(slices.Repeat([]string{elem}, n), ", ")
}

func forwardedPairs(n int) string {
	var b strings.Builder
	b.WriteString("for=198.51.100.7")
	for i := 1; i < n; i++ {
		fmt.Fprintf(&b, ";p%d=x", i)
	}
	return b.String()
}

func TestResolveClientTreatsAnOverCapFieldAsInvalid(t *testing.T) {
	t.Parallel()
	const trustedPeer, untrustedPeer = "10.0.0.1:4000", "192.168.1.50:4000"
	a := netip.MustParseAddr
	forwardedFrom := func(client netip.Addr) webhttp.ClientResolution {
		return webhttp.ClientResolution{IP: client, Peer: a("10.0.0.1"), Forwarded: true}
	}
	peerOnly := webhttp.ClientResolution{IP: a("10.0.0.1"), Peer: a("10.0.0.1")}
	ignored := func(public bool) webhttp.ClientResolution {
		return webhttp.ClientResolution{IP: a("192.168.1.50"), Peer: a("192.168.1.50"), UntrustedForwarding: true, IgnoredPublic: public}
	}
	hops := func(n int) string { return "203.0.113.9, " + repeatList("10.0.0.2", n-1) }
	cases := []struct {
		name   string
		remote string
		header http.Header
		want   webhttp.ClientResolution
	}{
		{"a trusted peer's 64 X-Forwarded-For entries are walked", trustedPeer, headerOf("X-Forwarded-For", hops(64)), forwardedFrom(a("203.0.113.9"))},
		{"a trusted peer's 65 X-Forwarded-For entries answer the peer", trustedPeer, headerOf("X-Forwarded-For", hops(65)), peerOnly},
		{"a trusted peer's 100,001 X-Forwarded-For entries answer the peer", trustedPeer, headerOf("X-Forwarded-For", hops(100_001)), peerOnly},
		{"65 entries over two field lines answer the peer", trustedPeer, headerOf("X-Forwarded-For", hops(33), repeatList("10.0.0.2", 32)), peerOnly},
		{"64 ignored X-Forwarded-For entries name a public address", untrustedPeer, headerOf("X-Forwarded-For", repeatList("203.0.113.9", 64)), ignored(true)},
		{"65 ignored X-Forwarded-For entries name nothing", untrustedPeer, headerOf("X-Forwarded-For", repeatList("203.0.113.9", 65)), ignored(false)},
		{"65 ignored X-Real-IP entries name nothing", untrustedPeer, headerOf("X-Real-IP", repeatList("203.0.113.9", 65)), ignored(false)},
		{"64 Forwarded elements name a public address", untrustedPeer, headerOf("Forwarded", repeatList("for=203.0.113.9", 64)), ignored(true)},
		{"65 Forwarded elements name nothing", untrustedPeer, headerOf("Forwarded", repeatList("for=203.0.113.9", 65)), ignored(false)},
		{"a 16-pair Forwarded element names a public address", untrustedPeer, headerOf("Forwarded", forwardedPairs(16)), ignored(true)},
		{"a 100,001-pair Forwarded element names nothing", untrustedPeer, headerOf("Forwarded", forwardedPairs(100_001)), ignored(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
			r.RemoteAddr, r.Header = tc.remote, tc.header
			if got := webhttp.ResolveClient(r, provenanceTrusted...); got != tc.want {
				t.Errorf("ResolveClient = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The allocation count pins the parser's bounded storage, not its total work
// (the byte scan stays linear in the header's length): past the cap doubling
// the field changes nothing, and under it doubling the entries at most
// doubles them. Not parallel: AllocsPerRun counts every goroutine's
// allocations.
func TestResolveClientParseWorkIsLinearAndCapped(t *testing.T) {
	allocs := func(h http.Header) float64 {
		r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		r.RemoteAddr, r.Header = "10.0.0.1:4000", h
		return testing.AllocsPerRun(100, func() { webhttp.ResolveClient(r, provenanceTrusted...) })
	}
	cases := []struct {
		name     string
		header   func(n int) http.Header
		n        int
		maxRatio float64
	}{
		{"X-Forwarded-For past the cap", func(n int) http.Header { return headerOf("X-Forwarded-For", repeatList("10.0.0.2", n)) }, 1_000, 1},
		{"Forwarded elements past the cap", func(n int) http.Header { return headerOf("Forwarded", repeatList("for=10.0.0.2", n)) }, 1_000, 1},
		{"one Forwarded element's pairs past the cap", func(n int) http.Header { return headerOf("Forwarded", forwardedPairs(n)) }, 1_000, 1},
		{"X-Forwarded-For under the cap", func(n int) http.Header { return headerOf("X-Forwarded-For", repeatList("10.0.0.2", n)) }, 32, 2},
		{"Forwarded elements under the cap", func(n int) http.Header {
			return headerOf("Forwarded", repeatList(`for="10.0.0.2:80";proto=https`, n))
		}, 32, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			once, twice := allocs(tc.header(tc.n)), allocs(tc.header(2*tc.n))
			if twice > tc.maxRatio*once {
				t.Errorf("%d entries allocate %v times per parse, %d entries %v; want at most %v times as many", tc.n, once, 2*tc.n, twice, tc.maxRatio)
			}
		})
	}
}

// RFC 7239 section 4 lets a value be a token or a quoted-string, so a value
// that is a valid token parses the same either way.
func TestParseForwardingQuotedTokenMatchesUnquoted(t *testing.T) {
	t.Parallel()
	for _, pair := range [][2]string{
		{"for=192.0.2.1", `for="192.0.2.1"`},
		{"for=unknown", `for="unknown"`},
		{"for=_hidden", `for="_hidden"`},
		{"for=192.0.2.1;proto=https", `for=192.0.2.1;proto="https"`},
		{"for=192.0.2.1;by=_edge", `for="192.0.2.1";by="_edge"`},
		{"for=203.0.113.9;host=example.com", `for="203.0.113.9";host="example.com"`},
	} {
		t.Run(pair[0], func(t *testing.T) {
			t.Parallel()
			unquoted := webhttp.ParseForwarding(headerOf("Forwarded", pair[0])).Forwarded
			quoted := webhttp.ParseForwarding(headerOf("Forwarded", pair[1])).Forwarded
			if len(unquoted) != 1 || !unquoted[0].Valid || !slices.Equal(unquoted, quoted) {
				t.Errorf("%s parses to %+v, %s to %+v; want one equal valid element", pair[0], unquoted, pair[1], quoted)
			}
		})
	}
}

// ClientIP has no logic of its own, so it agrees with ResolveClient on every
// combination of these peers, headers and trust sets, mapped spellings included.
func TestClientIPMatchesResolveClient(t *testing.T) {
	t.Parallel()
	mappedSet, _ := webhttp.ParseCIDRs([]string{"::ffff:10.0.0.0/104", "2001:db8:a::/48"})
	trustSets := [][]netip.Prefix{
		nil,
		{netip.MustParsePrefix("10.0.0.0/8")},
		{netip.MustParsePrefix("::ffff:10.0.0.0/104")},
		mappedSet,
		{netip.MustParsePrefix("0.0.0.0/0")},
		{netip.MustParsePrefix("::/0")},
	}
	peers := []string{
		"10.0.0.1:4000", "[::ffff:10.0.0.1]:4000", "[::ffff:198.51.100.20]:4000", "198.51.100.20:4000",
		"[2001:db8:a::1]:4000", "[fe80::1%eth0]:4000", "192.168.1.50", "garbage", "",
	}
	xffs := []string{
		"", "203.0.113.9", "8.8.8.8, 10.0.0.2", "203.0.113.9,,10.0.0.2", "junk, 10.0.0.2",
		"::ffff:198.51.100.7", `"x, 203.0.113.9`, "10.0.0.3", "2001:db8:a::9, 2001:db8:b::1",
	}
	forwardeds := []string{"", "for=198.51.100.7", `for="[2001:db8::1]:80"`, "for=unknown", "for=198.51.100.7:80"}
	for _, trusted := range trustSets {
		for _, peer := range peers {
			for _, xff := range xffs {
				for _, fwd := range forwardeds {
					r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
					r.RemoteAddr = peer
					if xff != "" {
						r.Header.Set("X-Forwarded-For", xff)
					}
					if fwd != "" {
						r.Header.Set("Forwarded", fwd)
					}
					want := webhttp.ResolveClient(r, trusted...).IP.String()
					if got := webhttp.ClientIP(r, trusted...); got != want {
						t.Errorf("ClientIP = %q, ResolveClient's IP %q (peer %q, xff %q, forwarded %q, trusted %v)", got, want, peer, xff, fwd, trusted)
					}
				}
			}
		}
	}
}
