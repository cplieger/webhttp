package webhttp_test

import (
	"net"
	"strings"
	"testing"

	"github.com/cplieger/webhttp"
)

// FuzzClassifyBind asserts the classifier never panics and holds four
// invariants:
//
//  1. Split agreement — ClassifyBind returns BindInvalid exactly when
//     net.SplitHostPort rejects the input, so the "host:port" grammar the
//     classifier accepts is precisely the one net.Listen parses (no private
//     dialect on either side).
//  2. Door agreement — for any splittable input, ClassifyBind(addr) equals
//     ClassifyBindHost(host-part): the two public doors share one
//     classification, so an app using the classify-the-unsplit-input
//     fallback recipe cannot diverge from an app splitting up front.
//  3. Totality of the host door — ClassifyBindHost never returns
//     BindInvalid, and every result of either door is one of the three
//     declared classes (String() never renders a number).
//  4. Case-insensitivity — classification never depends on letter case
//     ("LOCALHOST", "Localhost", IPv6 hex digits), the drift axis that
//     produced web-terminal-server's spurious exposure warning.
func FuzzClassifyBind(f *testing.F) {
	seeds := []string{
		":9848", "0.0.0.0:9848", "[::]:9848",
		"127.0.0.1:9848", "127.0.0.2:9848", "[::1]:9848",
		"[::ffff:127.0.0.1]:9848", "localhost:9848", "LOCALHOST:9848",
		"Localhost:7681", "192.168.1.5:9848", "203.0.113.7:9848",
		"[2001:db8::1]:9848", "myhost:9848", "foo.localhost:80",
		"[::1%lo]:80", "127.0.0.1:", "localhost:http",
		"9848", "127.0.0.1", "myhost", "", "127.0.0.1:80:90",
		"::1:9848", "[::1:9848", "127.0.0.001:80", "0:0:0:0:0:0:0:1",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, addr string) {
		class := webhttp.ClassifyBind(addr)

		// Invariant 3 (range): every result is a declared class.
		switch class {
		case webhttp.BindInvalid, webhttp.BindLoopback, webhttp.BindExposed:
		default:
			t.Fatalf("ClassifyBind(%q) = %d, not a declared BindClass", addr, int(class))
		}

		// Invariant 1: BindInvalid iff net.SplitHostPort rejects the input.
		host, _, err := net.SplitHostPort(addr)
		if (err != nil) != (class == webhttp.BindInvalid) {
			t.Fatalf("ClassifyBind(%q) = %v but SplitHostPort err = %v", addr, class, err)
		}

		if err == nil {
			// Invariant 2: the two doors agree on the host part.
			if got := webhttp.ClassifyBindHost(host); got != class {
				t.Fatalf("ClassifyBind(%q) = %v but ClassifyBindHost(%q) = %v", addr, class, host, got)
			}
		}

		// Invariant 3 (totality) + 4 on the host door, treating the raw
		// input as a bare host (the unsplit-input fallback path).
		hostClass := webhttp.ClassifyBindHost(addr)
		if hostClass == webhttp.BindInvalid {
			t.Fatalf("ClassifyBindHost(%q) = BindInvalid; the host door must be total", addr)
		}
		if got := webhttp.ClassifyBindHost(strings.ToUpper(addr)); got != hostClass {
			t.Fatalf("ClassifyBindHost case-sensitive: %q = %v but upper = %v", addr, hostClass, got)
		}
		if got := webhttp.ClassifyBindHost(strings.ToLower(addr)); got != hostClass {
			t.Fatalf("ClassifyBindHost case-sensitive: %q = %v but lower = %v", addr, hostClass, got)
		}
	})
}
