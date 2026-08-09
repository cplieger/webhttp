package webhttp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// loopbackOnlyProbe drives the middleware and reports whether the wrapped
// handler was reached, plus what the client saw.
func loopbackOnlyProbe(t *testing.T, mw Middleware, remote, host string, headers map[string]string) (admitted bool, rec *httptest.ResponseRecorder) {
	t.Helper()
	admitted = false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		admitted = true
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequest(http.MethodGet, "/api/tools", http.NoBody)
	r.RemoteAddr = remote
	r.Host = host
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return admitted, rec
}

func TestLoopbackOnlyAdmitsAnInContainerCaller(t *testing.T) {
	t.Parallel()
	admitted, rec := loopbackOnlyProbe(t, LoopbackOnly(nil), "127.0.0.1:54321", "localhost:9848", nil)
	if !admitted {
		t.Fatalf("a loopback peer with a loopback Host and no provenance headers was refused (%d): that is the only caller this gate exists to admit", rec.Code)
	}
}

// The reason the middleware exists rather than the bare predicate: a reverse
// proxy sharing the server's loopback interface rewrites Host to its upstream
// address by default (nginx, Apache), so BOTH LoopbackRequest legs pass while the
// request is remote. Each provenance header alone must refuse.
func TestLoopbackOnlyRefusesEachProvenanceHeader(t *testing.T) {
	t.Parallel()
	for _, name := range proxyProvenanceHeaders {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			admitted, rec := loopbackOnlyProbe(t, LoopbackOnly(nil),
				"127.0.0.1:54321", "localhost:9848", map[string]string{name: "x"})
			if admitted {
				t.Errorf("%s did not refuse: a same-loopback proxy passes both loopback legs, so the header set is the only thing left distinguishing it", name)
			}
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
		})
	}
}

// An EMPTY provenance header is not evidence: net/http exposes a header a client
// sent with no value, and refusing on presence-with-empty-value would reject an
// in-container caller whose HTTP library sends `Origin:` blank.
func TestLoopbackOnlyIgnoresAnEmptyProvenanceHeader(t *testing.T) {
	t.Parallel()
	admitted, _ := loopbackOnlyProbe(t, LoopbackOnly(nil),
		"127.0.0.1:54321", "localhost:9848", map[string]string{"Origin": ""})
	if !admitted {
		t.Error("an empty Origin refused the request; only a header with a VALUE is evidence of provenance")
	}
}

// Both LoopbackRequest legs still gate, so the middleware cannot be weaker than
// the predicate it composes.
func TestLoopbackOnlyStillRequiresBothLoopbackLegs(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ remote, host string }{
		"remote peer, loopback Host":       {"203.0.113.9:44321", "localhost:9848"},
		"loopback peer, attacker Host":     {"127.0.0.1:54321", "evil.example"},
		"remote peer, remote Host":         {"203.0.113.9:44321", "evil.example"},
		"portless RemoteAddr fails closed": {"127.0.0.1", "localhost:9848"},
		"empty RemoteAddr fails closed":    {"", "localhost:9848"},
		"unparseable Host fails closed":    {"127.0.0.1:54321", "[not-ipv6]:80"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if admitted, _ := loopbackOnlyProbe(t, LoopbackOnly(nil), tc.remote, tc.host, nil); admitted {
				t.Error("admitted a request the two-legged predicate refuses")
			}
		})
	}
}

// The refusal belongs to the app: a consumer with its own error envelope must be
// able to keep it, or adopting the middleware silently changes what its callers
// receive.
func TestLoopbackOnlyUsesTheCallerSuppliedRefusal(t *testing.T) {
	t.Parallel()
	custom := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("my own envelope"))
	})
	admitted, rec := loopbackOnlyProbe(t, LoopbackOnly(custom), "203.0.113.9:1", "evil.example", nil)
	if admitted {
		t.Fatal("the request was admitted")
	}
	if rec.Code != http.StatusTeapot || !strings.Contains(rec.Body.String(), "my own envelope") {
		t.Errorf("refusal = %d %q, want the caller's handler to have written it", rec.Code, rec.Body.String())
	}
}

// The default refusal carries the package envelope with a fixed code, which is
// what a log query or alert rule keys on across services.
func TestLoopbackOnlyDefaultRefusalCarriesTheStandardEnvelope(t *testing.T) {
	t.Parallel()
	_, rec := loopbackOnlyProbe(t, LoopbackOnly(nil), "203.0.113.9:1", "evil.example", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, loopbackRefusalCode) {
		t.Errorf("default refusal body %q does not carry the %q code", body, loopbackRefusalCode)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want JSON: the default refusal must match the package envelope", got)
	}
}

func TestProxiedRequest(t *testing.T) {
	t.Parallel()
	if ProxiedRequest(http.Header{}) {
		t.Error("an empty header set reported provenance")
	}
	for _, name := range proxyProvenanceHeaders {
		h := http.Header{}
		h.Set(name, "value")
		if !ProxiedRequest(h) {
			t.Errorf("%s not detected", name)
		}
	}
	// A header outside the set is not evidence.
	h := http.Header{}
	h.Set("User-Agent", "curl/8.0")
	h.Set("Accept", "*/*")
	if ProxiedRequest(h) {
		t.Error("an ordinary CLI request reported provenance; that would refuse the only caller the gate admits")
	}
}

// The header set is security-sensitive knowledge that had already drifted between
// two hand-rolled copies, which is why it is exported. Pin its membership so a
// silent narrowing fails here.
func TestProxyProvenanceHeaderSetMembership(t *testing.T) {
	t.Parallel()
	want := map[string]bool{
		"Forwarded": true, "X-Forwarded-For": true, "X-Forwarded-Host": true,
		"X-Forwarded-Proto": true, "X-Real-Ip": true, "Sec-Fetch-Site": true,
		"Origin": true,
	}
	if len(proxyProvenanceHeaders) != len(want) {
		t.Errorf("header set has %d entries, want %d: %v", len(proxyProvenanceHeaders), len(want), proxyProvenanceHeaders)
	}
	for _, name := range proxyProvenanceHeaders {
		if !want[name] {
			t.Errorf("unexpected header %q in the set", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("header %q was dropped from the set", name)
	}
}
