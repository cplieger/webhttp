# webhttp

[![Go Reference](https://pkg.go.dev/badge/github.com/cplieger/webhttp.svg)](https://pkg.go.dev/github.com/cplieger/webhttp)
[![Go version](https://img.shields.io/github/go-mod/go-version/cplieger/webhttp)](https://github.com/cplieger/webhttp/blob/main/go.mod)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/webhttp/badges/coverage.json)](https://github.com/cplieger/webhttp/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/webhttp/badges/mutation.json)](https://github.com/cplieger/webhttp/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13486/badge)](https://www.bestpractices.dev/projects/13486)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/webhttp/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/webhttp)

> Resilient server-side HTTP plumbing for Go

A standalone Go library bundling the server-side pieces almost every service ends up hand-rolling: request-id injection with one-line access logging, a flush/hijack-safe status recorder, composable middleware (panic recovery, security headers, per-route JSON timeout, a shared-bucket rate limiter, a no-store setter, a `Chain` combinator), a spoof-aware client-IP resolver, an exact-match Host allowlist against DNS rebinding, a bind-exposure classifier, an embedded-static file handler with content-hash ETags and precomputed gzip, a CSP inline-script hash extractor, JSON response and error helpers, request-prelude helpers, a request-path canonicalizer matching `ServeMux`'s own cleaning, a constant-time static-credential verifier, an HTTP readiness gate, a graceful server bootstrap with bounded-teardown and cancellation-classification helpers, and a Server-Sent-Events broadcast hub (the `sse` subpackage). Standard-library only, no external runtime dependencies.

webhttp is the inbound-server counterpart to [httpx](https://github.com/cplieger/httpx): httpx makes resilient requests going _out_, webhttp handles the requests coming _in_. The two are complementary and share no code. It ships the mechanism only; each application layers its own route table, error taxonomy, and named helpers on top.

## Install

`go get github.com/cplieger/webhttp@latest`

## Usage

```go
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/cplieger/webhttp"
)

func main() {
	ready := &webhttp.Ready{}

	mux := http.NewServeMux()
	mux.Handle("GET /readyz", webhttp.ReadinessHandler(ready))
	mux.HandleFunc("POST /things", func(w http.ResponseWriter, r *http.Request) {
		if !webhttp.RequireMethod(w, r, http.MethodPost) {
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if !webhttp.DecodeBody(w, r, &body, "invalid thing payload") {
			return
		}
		webhttp.WriteJSONStatus(w, http.StatusCreated, body)
	})

	// Browser-facing service? Decide your Host posture here: parse an
	// operator allowlist with ParseHostList and add hostPolicy.Middleware()
	// to the chain, placed before any cross-origin or CSRF check (see the
	// Host allowlist section below). Machine-facing APIs can skip it.

	// Compose middleware with Chain: the first listed is the outermost wrapper.
	// Logging outermost means a panic recovered below it is logged as its 500,
	// not a misleading 200.
	handler := webhttp.Chain(mux,
		webhttp.Logging(
			webhttp.WithSkipPaths("/events"), // don't log long-lived streams
			// The library derives the metric labels: the method from a closed
			// ten-value set, the path from the matched route. Nothing here can
			// mint a label series per scanner-invented URL or method token.
			webhttp.WithRecordRouteMetric(func(method, path string, status int, d time.Duration) {
				// feed your metrics pipeline here
			}),
		),
		webhttp.Recoverer(),
		webhttp.SecurityHeaders(),
	)

	// Streaming-safe defaults: ReadHeaderTimeout + IdleTimeout set,
	// ReadTimeout/WriteTimeout left unset so SSE/WebSocket work out of the box.
	// WithSlogErrorLog routes net/http's own connection-level lines (an accept
	// error above all) into slog at a level your log rules can match.
	srv := webhttp.NewServer(handler, webhttp.WithSlogErrorLog(slog.LevelError))

	// Bind the listener up front so a port-in-use error surfaces synchronously.
	ln, err := net.Listen("tcp", ":8080")
	if err != nil {
		panic(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ready.Set(true)
	if err := webhttp.Run(ctx, srv, ln, func(context.Context) {
		ready.Set(false) // application teardown on graceful shutdown
	}); err != nil {
		panic(err)
	}
}
```

## API

The bullets below map the surface; symbol-level depth lives in the [godoc](https://pkg.go.dev/github.com/cplieger/webhttp).

### Middleware

All middleware share the standard `func(http.Handler) http.Handler` shape (the `Middleware` type alias) and compose with `Chain`.

- `Chain(h, mw...) http.Handler`: wraps `h`; the first middleware listed is the outermost wrapper, so `Chain(h, A, B, C)` is `A(B(C(h)))`. A nil entry is skipped.
- `Recoverer(opts...) Middleware`: recovers a downstream panic, logs it at `Error` with the stack and request id, then writes a 500 via the configured `ErrorResponder` (the JSON `WriteError` by default). Re-panics `http.ErrAbortHandler`. Options: `WithRecoverLogger`, `WithPanicHook`, `WithRecoverResponder`.
- `SecurityHeaders(opts...) Middleware`: baseline response headers; always `X-Content-Type-Options: nosniff`, defaults `X-Frame-Options: DENY` and `Referrer-Policy: strict-origin-when-cross-origin`. Options: `WithCSP`, `WithFrameOptions`, `WithReferrerPolicy`, `WithPermissionsPolicy`, `WithCOOP`, `WithHSTS(maxAge, includeSubDomains, preload)`.
- `Logging(opts...) Middleware`: `RequestLogger` in `Chain`-composable form; takes the same `LogOption` values.
- `RouteTimeout(h, d, msg) http.Handler`: wraps `http.TimeoutHandler`; on timeout emits a 503 JSON `ErrorResponse` (`code: "timeout"`) carrying the context's request id.
- `RateLimiter(burst int, interval time.Duration, opts...) Middleware`: throttles the wrapped handler through a single process-wide token bucket (`burst` tokens, one accrued every `interval`, one consumed per admitted request); an empty bucket answers 429 (`code: "rate_limited"`) with a `Retry-After` hint. The bucket is shared across all clients: it bounds the aggregate rate of an expensive shared route, not per-client fairness. A non-positive `burst` or `interval` disables limiting (handler returned unwrapped), so a config-driven zero cleanly means "no limit". Options: `WithRateLimitWhen(pred)` (throttle only matching requests), `WithRateLimitError(code, msg)`, `WithRateLimitResponder(fn)` (render the 429 through your own `ErrorResponder` instead of the JSON envelope — the same hook `Recoverer` takes for its 500).
- `SessionCreateRateLimit(path string) Middleware`: a `RateLimiter` preset for endpoints where each admitted request forks an expensive process; gates POST to `path` (exact match) at burst 6, one token per second. Other methods and paths pass through without consuming a token. Apps needing different tuning compose `RateLimiter` directly.
- `FailedAuthRateLimit(when func(*http.Request) bool, msg string) Middleware`: a `RateLimiter` preset for a route guarded by one static credential; throttles requests `when` reports as presenting a failed credential at burst 10, one token per 6 seconds, answering 429 with the fixed code `too_many_auth_failures` and `msg`. A valid credential never draws a token, so the tuning does not have to leave room for the app's own senders; `msg` is the caller's because the credential differs per service (a bearer, an apikey, an app-specific token), and an empty one falls back to `too many failed authentication attempts`. What it bounds is the one-line-per-attempt log flood and the digest cost of an attempt as much as the guessing rate: a network-exposed listener otherwise turns a wire-speed guessing flood into a wire-speed stream of 401s, one access line each. A nil `when` throttles every request the middleware sees, which is the wiring for a caller that has already filtered the failed-auth class itself and wants to attribute the 429 to its own counter; passing the predicate in is the wiring for a caller that lets the middleware decide. Both are in use, and the tuning has one home so services guarding the same shape cannot drift apart.
- `NoStore() Middleware`: sets `Cache-Control: no-store` on every response passing through it, before the next handler runs. The value is fixed: this is the one header a dynamic surface needs, not a cache-policy API (per-asset policy belongs in `WithStaticCacheControl`). Placement and override ordering stay app-owned — the header is set, not locked, so a handler or inner middleware that needs its own value (a long-lived asset, a cacheable preview) simply `Set`s `Cache-Control` and wins, which is why the usual placement is innermost in the `Chain`. Scope it by mounting it on the subtree that needs it. It is not the middleware for a conditional no-store (a response uncacheable only when it carries a `Set-Cookie` has a different trigger and belongs with the code setting the cookie).

Put `Recoverer` inside `Logging` (`Chain(mux, Logging(), Recoverer())`) so a panicked request is logged as its 500 rather than the status recorder's default 200. `SecurityHeaders` builds no Content-Security-Policy: a CSP must match the app's own script and style sources, so pass the exact policy via `WithCSP`; any header default can be omitted with an empty string. HSTS is off by default; enable it only for a service reached exclusively over HTTPS, since the header makes browsers refuse plain-HTTP and untrusted-cert connections for the whole max-age. `RouteTimeout` cannot wrap streaming or hijacking handlers (`http.TimeoutHandler` buffers the entire response); use per-request deadlines via `http.ResponseController` for those.

### Static assets and CSP

- `StaticHandler(fsys fs.FS, opts...) (http.Handler, error)`: serves an embedded (or any `fs.FS`) static tree; `embed.FS` reports a zero ModTime, so a bare `http.FileServer` never revalidates. The handler walks the tree once at construction, precomputing a content-hash (sha256) ETag per file and a gzip representation (kept only when smaller), and serves known assets with the ETag, the cache policy's `Cache-Control`, and `Vary: Accept-Encoding`; everything else falls through to the identity `http.FileServer`. The construction error is non-nil only when walking `fsys` fails (a malformed embed should abort startup).
- `WithStaticCacheControl(fn func(assetPath string) string) StaticOption`: the per-asset `Cache-Control` policy; `fn` sees the normalized asset path and returns the header value, empty to omit. Default `no-cache` (the content-hash ETag makes revalidation a cheap 304).
- `InlineScriptHashes(html []byte) []string`: returns a CSP `'sha256-<base64>'` source token per inline `<script>` element, hashing exactly the bytes a browser hashes; an extractor for pages the app controls, not an HTML sanitizer. Feed the tokens into the app's own policy string via `WithCSP`; a caller whose page is known to carry inline scripts should treat an empty result as a malformed build and fail startup rather than degrade to `'unsafe-inline'`.
- `InlineStyleHashes(html []byte) []string`: the same for inline `<style>` elements, so `style-src` can be hash-pinned instead of `'unsafe-inline'` (a pre-JS loading overlay whose CSS must paint before the external stylesheet loads is the usual case). Shares the script scanner's core, so byte-boundary and malformed-tag behaviour cannot drift between the two. There is no skip rule to match the script scanner's external `src=` case, because a `<style>` element always carries its content inline. Note a style-src hash does not cover inline style **attributes** (`style="…"`), which `style-src-attr` governs and which need `'unsafe-hashes'`: an app whose markup or renderer sets style attributes cannot drop `'unsafe-inline'` on these tokens alone, though a renderer driving CSSOM property setters emits no attribute and is unaffected.

### Client IP

- `ClientIP(r, trusted...) string`: the best-effort client IP. With no trusted ranges (or when the direct peer is not inside one), `X-Forwarded-For` is ignored entirely and the host of `r.RemoteAddr` (the TCP peer, unspoofable at this layer) is returned. Only behind a trusted proxy is the header consulted, walked right-to-left past trusted hops to the first untrusted entry, the correct reading when a proxy appends the peer it saw (Caddy and most reverse proxies do; the leftmost entry is attacker-controlled). `X-Real-IP` is never consulted (client-settable). The caller supplies the trusted CIDRs; the library hardcodes none.
- `ParseCIDRs(entries []string) (nets, invalid)`: parses an operator list of CIDRs or bare IPs (bare is `/32`/`/128`; blanks skipped) into the trusted-proxy set for `ClientIP`/`WithClientIP`; malformed entries are returned separately so a strict caller can reject them while a lenient one logs and uses the valid subset.

The trusted set must contain every proxy hop between the client and the server; if a hop is missing the walk stops there and that hop's address is returned.

### Host allowlist

- `ParseHostList(entries []string, opts...) (*HostPolicy, invalid []string)`: parses an operator allowlist (a config array or a comma-split env var) into an immutable exact-match `HostPolicy`; malformed entries are returned separately (the `ParseCIDRs` shape).
- `CanonicalHost(hostport string) string`: the strict canonicalizer both the entries and each request's `Host` go through (lowercase, no port, no brackets, at most one trailing FQDN dot, IP literals normalized so different spellings of one address compare equal); returns `""` for anything malformed.
- `(*HostPolicy).Middleware() Middleware`: rejects a request whose `Host` is not allowlisted with a 403 JSON `ErrorResponse` (`code: "host_not_allowed"`); an inactive policy returns the handler unwrapped.
- `(*HostPolicy).Allows(r)` / `.Active()` / `.Size()`: the per-request decision and policy introspection.
- `WithLoopbackExempt() HostAllowlistOption`: admits a request when both the socket peer and the `Host` are loopback, so a baked container healthcheck or in-container client keeps working under a browser-facing allowlist; unreachable by rebinding (the attack's `Host` is not loopback) and by remote forgery (a remote peer is not loopback).
- `WithHostAllowlistError(code, msg string) HostAllowlistOption`: override the 403 code and message to name the app's configuration knob.

The gate breaks DNS rebinding (CWE-346): an attacker's page re-resolves its own hostname to this service's address, and the victim's browser then sends requests carrying the attacker's name in `Host`. Matching is exact and purely textual on the canonicalized `Host`: no name resolution (resolving would reopen the race the gate closes), `X-Forwarded-Host` ignored (client-controlled), and malformed `Host` values rejected rather than repaired (repair would collapse distinct wire values onto allowlisted keys). Configure non-ASCII names as Punycode A-labels; matching is byte-exact. Activation is fail-closed: a nil or all-blank entry list leaves the policy inactive (pass-through), but any non-blank entry engages the gate, so even an all-invalid list denies all rather than silently disabling protection. Place the middleware before any cross-origin or CSRF check: a rebinding request makes `Origin` and `Host` agree, so the exact-Host allowlist is what breaks that chain.

### Bind classification

- `ClassifyBind(addr string) BindClass`: classifies a configured listen address ("host:port") by exposure. `BindLoopback` covers loopback IP literals and "localhost" (case-insensitive); `BindExposed` covers wildcard binds, routable IPs, and any other hostname (no resolution is performed; an unresolvable name classifies as exposed); `BindInvalid` means the address is not "host:port" (the zero value, so an uninitialized class never reads as safe). Use it at startup to warn when an unauthenticated surface binds beyond loopback; what to do with an invalid input stays app policy (recipes in the godoc).
- `ClassifyBindHost(host string) BindClass`: classifies a bare host (no port); never returns `BindInvalid`. The fallback door for portless bind values.

### Status recorder

`StatusRecorder` wraps an `http.ResponseWriter` to capture the response status while staying transparent to streaming: `Unwrap` lets `http.NewResponseController` reach the underlying writer's `Flusher`, `Hijacker`, and deadline setters, and it also implements `http.Flusher`/`http.Hijacker`/`io.ReaderFrom` directly, so a handler that type-asserts those interfaces on the writer still works and `io.Copy`/`http.ServeContent` keep the sendfile fast path.

- `NewStatusRecorder(w) *StatusRecorder`: status defaults to 200.
- `.WriteHeader(code)` / `.Write(b)`: records the first explicit code only; implicit 200 on first write.
- `.Status() int` / `.Wrote() bool`: the recorded status, and whether the response is committed (the seam `Recoverer` uses to skip a double write onto an already-started response).
- `.Unwrap()` / `.Flush()` / `.Hijack()` / `.ReadFrom(src)`: passthroughs returning the underlying writer's own results.

### Request id and access logging

- `HeaderRequestID`: the `X-Request-ID` header constant.
- `ValidRequestID(s) bool`: 1 to 64 chars, each `[A-Za-z0-9_-]`.
- `NewRequestID() string`: 16 random bytes, hex-encoded.
- `WithRequestID(ctx, id)` / `RequestIDFromContext(ctx) string`: context threading.
- `RequestLogger(next, opts...) http.Handler`: reuses a valid inbound `X-Request-ID` or mints a fresh id, echoes and threads it, records status via a `StatusRecorder`, and emits one `Info` access-log line per request. Its two attacker-controlled attributes are bounded by default — the path to 512 bytes, the method to 24 — so a megabyte URL cannot buy a megabyte log line.
- `WithLogger(l)` / `WithSkipPaths(paths...)` / `WithSkipFunc(fn)`: the logger and skip rules. Skipped requests still get an id minted, echoed, and threaded, but no access line and no metric hook (a stream's open-to-close duration paired with a synthetic status would mislead).
- `WithSkipUpgrades()`: suppresses the record of a request whose response actually SWITCHED PROTOCOLS — recorded status 101, or a hijack taken before any status was recorded (the two shapes a completed WebSocket handshake takes, depending on whether the library writes the handshake through the `ResponseWriter` or onto the hijacked connection). **A consumer should no longer predict upgrades with a skip predicate**: use this instead, and delete the predicate. A predicate has to answer before the handler runs, so it must model the handshake policy of whichever library will answer — and every version of that guess is wrong somewhere. One consumer's predicate checked that `Sec-WebSocket-Key` was present exactly once, while coder/websocket base64-decodes that header and requires exactly 16 bytes, so every malformed-key 400 was suppressed as if it had upgraded, as was the cross-origin 403 no request-side predicate can model — losing status, duration, request id, and client ip for precisely the refused requests an operator is looking for. This option decides from the response instead, so the 400, the 403, the 405 for a non-GET, and the 426 for missing upgrade headers all keep complete records on the same route. Suppression removes the whole record (no line, no metric hook, no level policy, no path transform), the request id is still minted and echoed, and the interaction with the skip rules is one-way: `WithSkipPaths`/`WithSkipFunc` bypass the recorder before the handler runs, so a match there wins whatever the status turns out to be, and this option can only remove a record, never restore one. Two boundaries keep their record — a handler that never calls `WriteHeader` (the implicit 200 is not 101, so ordinary requests are untouched), and one that writes an explicit status and only then hijacks (a CONNECT tunnel answering 200 told us what it answered). It takes no status argument on purpose: 101 is the one status that means the exchange ended rather than completed, and a general skip-these-statuses knob would only add the ability to delete refusals — for noise control, `WithLogLevel`/`ProbeLogLevel` lowers a line's level instead of removing it.
- `WithTemplatePathsUnder(prefixes ...string)`: declare URL prefixes whose concrete paths carry a CREDENTIAL, and the access line records the matched route template for them (`/api/sessions/{id}`) instead of the path. The template comes from `r.Pattern`, so the router is the source of truth and a new upstream subroute logs correctly with no change here; the method prefix is stripped. A path under a declared prefix that matched NOTHING records the prefix plus `(unmatched)` — never the raw path, because an unrouted request under a credential-bearing prefix still contains the credential, and marking it unmatched makes a new subroute visible rather than mislabelling it onto a route it is not. Paths outside every declared prefix are recorded unchanged, deliberately: a static mount's pattern is `/`, so templating everything would collapse every asset onto one line. Prefer this over hand-writing the equivalent `WithPathFunc` — two apps in this fleet wrote that transform over the same route table and diverged on the unmatched case, which is exactly the decision this makes once. Pass the prefix the route-owning package exports (e.g. the terminal engine's `SessionsSubtreePath`), not a local literal.
- `WithPathFunc(fn func(*http.Request) string)`: the recorded-path policy. fn's return replaces `r.URL.Path` in the access line, the legacy `WithRecordMetric` path argument, and the hook-failure diagnostics — the escape hatch for a path policy `WithTemplatePathsUnder` cannot express (a truncated form, a per-request decision); reach for that option first. Runs at emit time, after routing, so `r.Pattern` is populated (empty on unmatched — return your own fail-closed placeholder for those). Skip rules test the raw path and skipped requests never call it; the request-derived metric hooks are unaffected (`WithRecordMetricRequest` receives the request and owns its own representation, and `WithRecordRouteMetric`'s path label is the matched route). Fail-closed: a panicking or empty-returning fn records `(path-redaction-failed)`, never the raw path.
- `WithMaxLoggedPath(n int)`: the byte cap on the recorded path, replacing the 512-byte default. The cap applies to whatever the path policy produced — the raw `r.URL.Path`, a `WithTemplatePathsUnder` template, your own `WithPathFunc` return, and the fail-closed placeholders — so no policy can miss it. An over-cap value keeps at most `n` bytes, cut on a UTF-8 rune boundary (a split rune reaches the log store as U+FFFD), plus a `...(truncated)` marker so a reader knows the value was cut; a within-cap value is recorded byte-identical. It bounds the LOG only: request size stays `WithMaxHeaderBytes`' job. A non-positive `n` is ignored and the default stands — there is deliberately no way to switch the bound off, because a config-driven `0` would silently reopen the hole it closes. Tighten it when the whole route table is short (128 covers a service serving `/healthz`, `/metrics`, and one templated route). The method's 24-byte bound has no option: the longest method in IANA's registry is `UPDATEREDIRECTREF` at 17 characters, so the ceiling is a fact about HTTP rather than a per-app policy, and anything longer records as a fixed `(overlong)` placeholder — a truncated method would read as one somebody actually tried. Neither bound touches routing, the status, or the `Allow` header.
- `WithClientIP(trusted ...*net.IPNet)` / `WithClientIPFunc(fn)`: add a `client_ip` attribute, resolved by the spoof-proof `ClientIP` or by your own function (for a dynamic or hot-reloaded trusted set). Mutually exclusive; the last one applied wins. Omitted entirely unless supplied, so the default line is unchanged.
- `WithRecordMetric(fn)` / `WithRecordMetricRequest(fn)`: the older metric hooks, kept for the two cases named here. `WithRecordMetric` receives the values the access line recorded (length-bounded, but a raw path is still one label per URL a scanner invents); `WithRecordMetricRequest` receives the request itself, for a metric that genuinely needs something other than the standard pair (a per-tenant series keyed on an id the app validated). Both are emitted even when the handler panics. All three metric options are mutually exclusive; the last one applied wins. Prefer `WithRecordRouteMetric` — these two make the APP responsible for a cardinality bound it can get wrong.
- `WithRecordRouteMetric(fn func(method, path string, status int, d time.Duration))`: the recommended metric hook. The library derives the `(method, path)` label pair (`RouteMetricLabels`) and hands it to fn, so the app never sees the raw request through this option and has no derivation to get wrong. That is the point of it existing as an option rather than only as a function: safety is a property of the wiring, not of every consumer remembering to call the right helper — one consumer in this fleet reached for the raw-request hook, wrote a comment asserting its labels were bounded, and shipped an attacker-controllable method label anyway. Same argument list as `WithRecordMetric`, so switching a consumer over changes the option name and nothing else. Fires from the same deferred emit (a panicking handler is still recorded, a panicking hook is isolated), is excluded on skipped paths, and is unaffected by the recorded-path policy options — those bound a log line, this bounds a label domain.
- `RouteMetricLabels(r) (method, path string)`: the pure derivation behind that option, exported for an app that already has a `WithRecordMetricRequest` hook and wants the standard pair inside it. The **method** label is `r.Method` when it is one of the nine standard methods — GET, HEAD, POST, PUT, DELETE, CONNECT, OPTIONS, TRACE (RFC 9110 §9.3) and PATCH (RFC 5789) — and a fixed `other` bucket for everything else: ten values by construction, whatever arrives on the request line, including a lowercase `get` (HTTP methods are case-sensitive, so it is not GET) or a punctuation token like ``M!#$%&'*+-.^_`|~`` (a legal method that really does reach a handler). The **path** label is the route the mux matched: `r.Pattern` with the method prefix stripped (`GET /beat/{id}` → `/beat/{id}`, so an unknown beat id mints nothing), the pattern itself when it names no method (`/beat/{id}`, a `/` catch-all), and a fixed `unmatched` when nothing matched. So the series ceiling is ten times one more than the route table, and nothing about the traffic can widen it — which matters because the hook fires outside every app auth gate and a minted series is permanent for the process lifetime here and in every observer scraping it. The method comes from the request rather than from the matched pattern so that the log and the metric agree: a pattern-derived method records `GET` for a HEAD probe against a GET-only route (that is how ServeMux routes HEAD) while the access line says HEAD. **One divergence is deliberate**: for a non-standard method the access line records the token verbatim (bounded to 24 bytes) while the metric records `other`, because a log line is read to diagnose one request and needs the real value, while a series is read in aggregate and needs a bounded name — match them up by `request_id`.
- `WithLogLevel(fn func(r *http.Request, status int) slog.Level)`: the access-line level policy (default: every line at `Info`). The canonical use is scrape-noise control on a polled service: 2xx/3xx at `Debug`, `Warn` for 4xx, `Error` for 5xx. Skipped paths never call it; a panicking policy falls back to `Info`.

### JSON responses and errors

- `JSONHeaders(w)`: `application/json` + `X-Content-Type-Options: nosniff`.
- `WriteJSON(w, v)`: 200.
- `WriteJSONStatus(w, code, v)`: headers, status, encode (encode failure logged at `Warn`, not returned).
- `Ok(w)`: 200 `{"ok":true}`.
- `WriteError(w, r, status, code, msg)`: writes `ErrorResponse`; nil-safe when `r` is nil.
- `ErrorResponse{Error, Code, RequestID}`: `Code` and `RequestID` omitted when empty.
- `ErrorResponder`: `func(w, r, status, code, msg)`, the signature of `WriteError` (its canonical instance and the default); middleware that emits an error body takes one so a non-JSON endpoint can render its error on its own content type.

`WriteError` pulls the request id from the request context so a client can correlate a failure with the access log; every library error envelope follows that scheme, with the field omitted when the context carries no id.

### Request prelude

- `MaxJSONBody`: 1 MiB default body cap.
- `LimitBody(w, r, maxBytes)`: wraps the body in `http.MaxBytesReader`. A read past the cap fails with a `*http.MaxBytesError` (test with `errors.As`) and nothing is written, so the status is yours. It also asks `net/http` to close the connection rather than drain the sender's excess, reaching net/http's own writer through the `Unwrap` chain — best-effort, because a middleware that does not unwrap (or `RouteTimeout`, whose buffering writer cannot be unwrapped) blocks that signal. Detect an over-limit body on the read error, never on the close.
- `RequireMethod(w, r, method) bool`: 405 + `false` on mismatch.
- `MethodNotAllowed(w, r, allowed...)`: the 405 refusal on its own, for a route that permits SEVERAL methods and so has no single method to require — the `Allow` header names the whole set (`GET, POST`), the body is the standard `method_not_allowed` envelope. Route each method with the mux (or dispatch on `r.Method`) and refuse the rest with it.
- `SetAllow(w, allowed...)`: just the RFC 9110 `Allow` header, for an `OPTIONS` responder or any other advertisement outside a 405.
- `DecodeBody(w, r, v, errMsg) bool`: cap + decode (reject trailing data); 400 + `false` on failure.
- `DecodeBodyOptional(w, r, v)`: cap + decode, error ignored.
- `DecodeJSONInto(w, r, v, maxBytes) error`: the mechanism behind `DecodeBody`, for apps with their own error envelope or a per-endpoint cap; caps, decodes a single value, rejects trailing data, writes nothing, and returns the error: a `*http.MaxBytesError` (test with `errors.As`) for an oversize body, `ErrTrailingData` for a second JSON value, otherwise a malformed body. Map the result to your own status and envelope.

`Allow` is mandatory on a 405 and is a comma-separated list, so the value is the caller's set joined with `", "`: entries verbatim (a method token is case-sensitive), blanks dropped (a sender must not emit an empty list element), exact duplicates collapsed, and no methods at all rendered as the empty value the spec defines as "this resource allows no methods". `HEAD` is never implied by `GET`: `net/http`'s `ServeMux` serves `HEAD` from a `GET` pattern, so a route whose `GET` has a side effect registers `HEAD` separately to reject it and must not advertise it — pass `http.MethodHead` when the route really serves it.

### Canonical request path

- `CanonicalRequestPath(p string) (clean string, canonical bool)`: returns the request path `http.ServeMux` will route `p` as (`path.Clean`, with a non-root trailing slash put back — net/http's own `cleanPath`), and reports whether `p` already is that path.

`ServeMux` canonicalizes the escaped request path _before_ it selects a pattern, and answers 307 when the result differs, so no registered route can intercept the rewrite. For a browser that is invisible. For a machine sender it is not: a 307 is a **success** status to a client that does not follow redirects (`curl -fsS` without `-L`), so such a caller exits 0 having never reached the handler — nothing recorded, no job run, and nothing anywhere saying the URL was malformed. A route whose whole purpose is a side effect uses this to refuse the non-canonical spelling itself. It returns the cleaned value as well as the verdict because a caller usually needs both: the verdict decides whether to refuse, and the cleaned path is what tells it whether the request cleans _into_ the namespace it guards (`//beat/api` does).

Pass `r.URL.EscapedPath()` to reproduce the mux's cleaning decision exactly. Passing the decoded `r.URL.Path` instead makes the verdict strictly wider — `%2e%2e` decodes to `..`, so an encoded dot segment reads as non-canonical here while the mux draws no redirect for it — which is a legitimate choice (the decoded path is the one the sender believed it was addressing), so pick the value deliberately. `canonical` is the verdict of the cleaning step alone, not "the mux will not redirect this request": the trailing-slash redirect (`/tree` → `/tree/` when a `/tree/` subtree is registered) fires on an already-canonical path and depends on the route table, not the spelling, and a `CONNECT` request is not canonicalized at all. An empty `p` returns `/` and false, and a `p` with no leading slash is rooted before cleaning, so it can never be canonical. Route scope, the refusal's status and body, and any metric counting the class stay app-owned; this is a pure function over a string.

### Static secret verification

- `NewStaticTokenVerifier(configured) StaticTokenVerifier`: build once at startup from the single operator-configured secret guarding an endpoint.
- `StaticTokenVerifier.Verify(presented) bool`: constant-time match of a presented credential; safe for concurrent use.

The verification primitive for static machine credentials (an API key, a bearer token, a basic-auth user or password) where exactly one expected value comes from configuration. The configured secret is SHA-256-hashed once at construction; `Verify` hashes the presented value and compares the two fixed-length digests with `subtle.ConstantTimeCompare`, so no per-call timing varies with the secret's length or content. An empty configured secret fails closed: `Verify` returns `false` for every presented value, including the empty string (otherwise `sha256("")` equals `sha256("")` and an unset secret would grant access to any client presenting no credential). Treat an empty configured value as "auth not configured", never as "no credential required". This verifies one shared secret, not user identities; per-user credential stores, password hashing, and session management belong to the [auth](https://github.com/cplieger/auth) library.

### Readiness

- `Ready`: a concurrency-safe flag; zero value is not ready.
- `(*Ready).Set(ready)` / `(*Ready).Ready() bool`
- `ReadinessChecker`: the `Ready() bool` interface `*Ready` satisfies.
- `ReadinessHandler(c) http.HandlerFunc`: 200 `{"status":"ok"}` when ready, else 503 `{"status":"unready","reason":"starting up or shutting down"}`.

This is the HTTP serving-state gate, for a load balancer asking "should this instance receive traffic right now?". It is deliberately distinct from the [health](https://github.com/cplieger/health) library's container file-marker probe, which answers "is the process alive?" for a Docker `HEALTHCHECK`. The two are complementary, not the same endpoint.

### Server

- `NewServer(handler, opts...) *http.Server`: streaming-safe defaults: `ReadHeaderTimeout` 10s (slowloris guard), `IdleTimeout` 120s, `MaxHeaderBytes` 1 MiB; `ReadTimeout` and `WriteTimeout` unset so streaming works out of the box. Options: `WithReadTimeout`, `WithWriteTimeout`, `WithIdleTimeout`, `WithReadHeaderTimeout`, `WithMaxHeaderBytes`, `WithErrorLog`, `WithSlogErrorLog`.
- `WithSlogErrorLog(level slog.Level) ServerOption`: routes net/http's own connection-level lines — above all `http: Accept error: ...; retrying`, the trace of an exhausted fd budget — into `slog` at `level`, so they arrive as level-carrying records instead of unstructured standard-logger output no level-based log rule can match. The level is deliberately the caller's choice (an accept failure is fatal to a probe-only service and a retryable degradation to others). It reads `slog.Default()` when the option is applied, so install the process logger first; `WithErrorLog` remains the override for any other `*log.Logger`, and the default is unchanged when neither is passed.
- `Run(ctx, srv, ln, onShutdown, opts...) error`: serves until `ctx` is cancelled, then shuts down gracefully: the pre-drain hook (if registered) runs first, then `srv.Shutdown` drains in-flight requests, then `onShutdown` runs for application teardown, all within one shared shutdown grace budget. Options: `WithShutdownGrace(d)` (default 5s); `WithPreDrain(fn)`, a hook invoked after `ctx` cancellation and strictly before the drain starts: the place to flip a readiness gate, cancel the server's `BaseContext`, or drain an SSE hub so long-lived streams release instead of holding the drain open for the whole grace window; `WithServeExit(fn)`, the opt-in teardown for the other exit.
- `ErrShutdownGraceExpired`: the origin marker on a `Run` error caused by the shutdown grace running out. `Run`'s error has two possible deadline origins — a serve error carrying a deadline of the caller's own making, and the graceful sequence outliving the grace — and both can satisfy `errors.Is(err, context.DeadlineExceeded)`, so a caller inferring the second from the bare deadline error is asserting what the value cannot prove. A grace expiry is wrapped so both `errors.Is(err, webhttp.ErrShutdownGraceExpired)` and `errors.Is(err, context.DeadlineExceeded)` hold; the wrapped error stays in the chain, so existing checks are unaffected. A real serve error still takes precedence over the shutdown error and is never marked. What to do about it — naming the grace constant to raise, the log level, the exit code — stays app policy.
- `AwaitDone(ctx, done) bool`: the bounded wait a teardown needs, reporting whether `done` closed before `ctx` expired. It creates no timeout of its own and logs nothing (teardown bodies and their diagnostics stay app-owned): `Run` hands `onShutdown` whatever remains of the one grace budget after the pre-drain phase and the drain spent their share, so a fresh deadline here would be a budget the shutdown sequence does not have. It re-checks `done` after `ctx` fires, which is the point: a drain that consumed the whole grace hands the teardown an already-expired context, and a `select` with both cases ready picks pseudo-randomly, so the naive two-case wait reports a teardown that did finish as still running a fraction of the time.
- `CausedByCancellation(ctx, err) bool`: whether `err` is the observable form of _this_ context's cancellation, so a boundary can tell a routine stop apart from a fault that merely happened at the same moment. It proves the match rather than assuming it — a cancelled context alone is not evidence, or a listener bind that genuinely failed while a signal arrived would read as a clean stop — and it matches `context.Cause(ctx)` as well as `ctx.Err()`, since a `WithCancelCause` cause need not wrap `context.Canceled` and net/http surfaces the cause verbatim. The response (level, message, exit code, retry) stays the caller's.

`Run` has exactly two exits, and only one of them is the graceful sequence. When `Serve` returns on its own instead — a dead accept loop, or a `Shutdown`/`Close` the caller drove itself — `ctx` was never cancelled, the listener is already gone, and neither `WithPreDrain` nor `onShutdown` runs (both are defined against a graceful stop). `WithServeExit(fn)` is the teardown for that path: `fn` gets the whole grace as its budget (no drain spent any of it), `Run` does not call `srv.Shutdown` behind it, and exactly one of the two paths runs per call. It is opt-in, so a caller that registers nothing keeps today's behavior of returning the serve error with no hook at all. Because `ctx` is still live there, a teardown that waits on a goroutine keyed to it (a background loop stopped by the same signal context) must cancel it inside `fn` — otherwise it waits out the whole grace for a goroutine nothing asked to stop.

Streaming apps (SSE, WebSocket, long responses) MUST omit `WithWriteTimeout`, since a write deadline would cut off an in-progress stream. Bind the listener up front (for example with `net.ListenConfig.Listen`) so a port-in-use error surfaces synchronously before `Run`, and pass application teardown as `onShutdown`.

### Server-sent events (`sse` subpackage)

`github.com/cplieger/webhttp/sse` is a broadcast hub for SSE endpoints, the streaming counterpart to the request/response helpers above (`RouteTimeout` deliberately cannot wrap a stream).

```go
hub := sse.NewHub(sse.WithMaxClients(64))

mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
	hub.Serve(w, r,
		sse.WithTopic(r.URL.Query().Get("chat_id")),
		sse.OnConnect(func(w *sse.Writer, floor, head uint64) error {
			return w.Event(head, "connected", fmt.Appendf(nil, `{"floor":%d,"head":%d}`, floor, head))
		}))
})

hub.Publish(sse.Event{Name: "notify", Topic: chatID, Data: payload})
// on shutdown, before srv.Shutdown:
hub.Shutdown()
```

- `NewHub(opts...)`: options `WithReplay(n)` (ring size, default 256; every event gets a monotonic ID, and a reconnect with `Last-Event-ID` replays what the client missed, gap-free and overlap-free), `WithClientBuffer(n)`, `WithMaxClients(n)` (503 beyond the cap; 0 = unlimited), `WithKeepalive(d)` (`: keepalive` comments, default 15s, below common proxy idle timeouts), `WithLogger(l)`.
- `(*Hub).Publish(Event)`: fan-out; assigns the ID, appends to the replay ring, evicts (cancels) a subscriber whose buffer is full rather than blocking, relying on EventSource auto-reconnect plus replay. Nil-safe.
- `(*Hub).Serve(w, r, opts...)`: owns the proxy-defensive headers (`no-transform`, `X-Accel-Buffering: no`), deadline clearing, `Last-Event-ID` replay, keepalives, and frame encoding. An `http.Flusher` reachable through an `Unwrap()` chain works, so wrapping middleware keeps streaming intact; the 500 `streaming_unsupported` refusal fires only when no flusher exists at any depth. Options: `WithTopic(t)` (receive broadcasts plus events scoped to `t`), `OnConnect(fn)` (write a handshake carrying the replay bounds `(floor, head)`; a client whose last-seen ID is below `floor` missed events and should refetch state).
- `(*Hub).Bounds()` / `.ClientCount()` / `.Buffered()`: the replay bounds, subscriber count, and a snapshot of the replay window (for diagnostics endpoints and tests).
- `(*Hub).SetMaxClients(n)`: replace the subscriber cap at runtime (for hot-reloaded configuration); lowering it does not evict connected clients.
- `(*Hub).Shutdown()`: drain gate; cancels every client, and subsequent `Serve` calls get 503. Refusal responses use the standard `ErrorResponse` envelope (codes `sse_unavailable`, `streaming_unsupported`).

## Contributing

Issues and PRs are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for the
conventions and how to run the checks locally.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0. See [LICENSE](LICENSE).
