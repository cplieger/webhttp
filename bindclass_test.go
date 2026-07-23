package webhttp_test

import (
	"testing"

	"github.com/cplieger/webhttp"
)

// TestClassifyBind pins the classification table for configured "host:port"
// listen addresses. The rows are the union of the three consumer tables the
// classifier replaced (web-terminal-kiro isExposedBind, web-terminal-server
// isLoopbackHost/warnIfExposed, pg-autodump listenIsPublic), so a migration
// regression in any app's semantics fails here first.
func TestClassifyBind(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want webhttp.BindClass
	}{
		// Wildcards: reachable on every interface.
		{name: "wildcard empty host", addr: ":9848", want: webhttp.BindExposed},
		{name: "ipv4 wildcard", addr: "0.0.0.0:9848", want: webhttp.BindExposed},
		{name: "ipv6 wildcard", addr: "[::]:9848", want: webhttp.BindExposed},

		// Explicit loopback: unreachable from other machines.
		{name: "ipv4 loopback", addr: "127.0.0.1:9848", want: webhttp.BindLoopback},
		{name: "ipv4 loopback subnet", addr: "127.0.0.2:9848", want: webhttp.BindLoopback},
		{name: "ipv4 loopback high octet", addr: "127.255.255.254:80", want: webhttp.BindLoopback},
		{name: "ipv6 loopback", addr: "[::1]:9848", want: webhttp.BindLoopback},
		{name: "4in6 loopback", addr: "[::ffff:127.0.0.1]:9848", want: webhttp.BindLoopback},
		{name: "localhost name", addr: "localhost:9848", want: webhttp.BindLoopback},
		{name: "localhost is case-insensitive", addr: "LOCALHOST:9848", want: webhttp.BindLoopback},
		{name: "localhost mixed case", addr: "Localhost:7681", want: webhttp.BindLoopback},

		// Routable IPs and names: reachable beyond loopback.
		{name: "LAN ip", addr: "192.168.1.5:9848", want: webhttp.BindExposed},
		{name: "public ip", addr: "203.0.113.7:9848", want: webhttp.BindExposed},
		{name: "ipv6 global", addr: "[2001:db8::1]:9848", want: webhttp.BindExposed},
		{name: "hostname is exposed without resolution", addr: "myhost:9848", want: webhttp.BindExposed},
		{name: "localhost subdomain is a plain hostname", addr: "foo.localhost:80", want: webhttp.BindExposed},
		{name: "localhost trailing dot is a plain hostname", addr: "localhost.:80", want: webhttp.BindExposed},
		{name: "whitespace-padded localhost is a plain hostname", addr: " localhost :80", want: webhttp.BindExposed},
		{name: "4in6 routable", addr: "[::ffff:192.0.2.1]:80", want: webhttp.BindExposed},

		// SplitHostPort accepts a bracketed NAME (brackets are transparent,
		// not IPv6-only syntax there), so the host inside still classifies.
		{name: "bracketed localhost still classifies", addr: "[localhost]:80", want: webhttp.BindLoopback},
		// The hex and uppercase spellings of 4-in-6 loopback are the same
		// address; ParseIP canonicalizes before IsLoopback.
		{name: "4in6 loopback hex form", addr: "[::ffff:7f00:1]:80", want: webhttp.BindLoopback},
		{name: "4in6 loopback uppercase hex", addr: "[::FFFF:127.0.0.1]:9848", want: webhttp.BindLoopback},
		// strings.EqualFold is Unicode SIMPLE folding, so the long-s fold of
		// "localhost" matches — documented on BindLoopback, shared with the
		// two origin apps that already folded (kiro, pg-autodump).
		{name: "unicode simple fold of localhost matches", addr: "localho\u017ft:80", want: webhttp.BindLoopback},
		// net.ParseIP rejects zoned literals, so the hostname path
		// classifies them: fail-public, matching all three origin copies.
		{name: "zoned ipv6 loopback is exposed", addr: "[::1%lo]:80", want: webhttp.BindExposed},

		// The port is not validated: net.Listen owns that failure.
		{name: "empty port still classifies", addr: "127.0.0.1:", want: webhttp.BindLoopback},
		{name: "non-numeric port still classifies", addr: "localhost:http", want: webhttp.BindLoopback},

		// Unsplittable input: the class is BindInvalid and policy is the
		// caller's (fail-silent, fail-public, or host-form fallback).
		{name: "bare port number", addr: "9848", want: webhttp.BindInvalid},
		{name: "portless ipv4 loopback", addr: "127.0.0.1", want: webhttp.BindInvalid},
		{name: "portless hostname", addr: "myhost", want: webhttp.BindInvalid},
		{name: "empty string", addr: "", want: webhttp.BindInvalid},
		{name: "too many colons", addr: "127.0.0.1:80:90", want: webhttp.BindInvalid},
		{name: "unbracketed ipv6 with port", addr: "::1:9848", want: webhttp.BindInvalid},
		{name: "stray bracket", addr: "[::1:9848", want: webhttp.BindInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := webhttp.ClassifyBind(tc.addr); got != tc.want {
				t.Errorf("ClassifyBind(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

// TestClassifyBindHost pins the bare-host door: the host half of a listen
// address, or a whole portless bind under the classify-the-unsplit-input
// recipe (web-terminal-server's fallback). It never returns BindInvalid.
func TestClassifyBindHost(t *testing.T) {
	cases := []struct {
		name string
		host string
		want webhttp.BindClass
	}{
		{name: "empty host is the wildcard", host: "", want: webhttp.BindExposed},
		{name: "ipv4 unspecified", host: "0.0.0.0", want: webhttp.BindExposed},
		{name: "ipv6 unspecified", host: "::", want: webhttp.BindExposed},
		{name: "ipv4 loopback", host: "127.0.0.1", want: webhttp.BindLoopback},
		{name: "ipv6 loopback", host: "::1", want: webhttp.BindLoopback},
		{name: "ipv6 loopback long form", host: "0:0:0:0:0:0:0:1", want: webhttp.BindLoopback},
		{name: "localhost any case", host: "lOcAlHoSt", want: webhttp.BindLoopback},
		{name: "LAN ip", host: "192.168.1.5", want: webhttp.BindExposed},
		{name: "hostname", host: "myhost", want: webhttp.BindExposed},
		{name: "zoned loopback is exposed", host: "::1%lo", want: webhttp.BindExposed},
		{name: "leading-zero ipv4 is not an ip literal", host: "127.0.0.001", want: webhttp.BindExposed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := webhttp.ClassifyBindHost(tc.host); got != tc.want {
				t.Errorf("ClassifyBindHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

// TestBindClassString pins the log-attribute rendering, including the zero
// value: an uninitialized BindClass must read "invalid", never a safe class.
func TestBindClassString(t *testing.T) {
	cases := []struct {
		class webhttp.BindClass
		want  string
	}{
		{class: webhttp.BindInvalid, want: "invalid"},
		{class: webhttp.BindLoopback, want: "loopback"},
		{class: webhttp.BindExposed, want: "exposed"},
		{class: webhttp.BindClass(0), want: "invalid"}, // zero value fails safe
		{class: webhttp.BindClass(99), want: "invalid"},
	}
	for _, tc := range cases {
		if got := tc.class.String(); got != tc.want {
			t.Errorf("BindClass(%d).String() = %q, want %q", int(tc.class), got, tc.want)
		}
	}
}
