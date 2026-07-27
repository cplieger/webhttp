# webhttp

[![Go Reference](https://pkg.go.dev/badge/github.com/cplieger/webhttp.svg)](https://pkg.go.dev/github.com/cplieger/webhttp)
[![Go version](https://img.shields.io/github/go-mod/go-version/cplieger/webhttp)](https://github.com/cplieger/webhttp/blob/main/go.mod)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/webhttp/badges/coverage.json)](https://github.com/cplieger/webhttp/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/webhttp/badges/mutation.json)](https://github.com/cplieger/webhttp/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13486/badge)](https://www.bestpractices.dev/projects/13486)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/webhttp/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/webhttp)

> Resilient server-side HTTP plumbing for Go

A standalone Go library bundling the server-side pieces almost every service ends up hand-rolling: request-id injection with one-line access logging, a flush/hijack-safe status recorder, composable middleware (panic recovery, security headers, per-route JSON timeout, a shared-bucket rate limiter, a `Chain` combinator), a spoof-aware client-IP resolver, an exact-match Host allowlist against DNS rebinding, a bind-exposure classifier, an embedded-static file handler with content-hash ETags and precomputed gzip, a CSP inline-script hash extractor, JSON response and error helpers, request-prelude helpers, a constant-time static-credential verifier, an HTTP readiness gate, a graceful server bootstrap, and a Server-Sent-Events broadcast hub (the `sse` subpackage). Standard-library only, no external runtime dependencies.

webhttp is the inbound-server counterpart to [httpx](https://github.com/cplieger/httpx): httpx makes resilient requests going _out_, webhttp handles the requests coming _in_. The two are complementary and share no code. It ships the mechanism only; each application layers its own route table, error taxonomy, and named helpers on top.

## Install

`go get github.com/cplieger/webhttp@latest`

## Usage

```go
package main

import (
	"context"
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
			webhttp.WithRecordMetric(func(method, path string, status int, d time.Duration) {
				// feed your metrics pipeline here
			}),
		),
		webhttp.Recoverer(),
		webhttp.SecurityHeaders(),
	)

	// Streaming-safe defaults: ReadHeaderTimeout + IdleTimeout set,
	// ReadTimeout/WriteTimeout left unset so SSE/WebSocket work out of the box.
	srv := webhttp.NewServer(handler)

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
- `RateLimiter(burst int, interval time.Duration, opts...) Middleware`: throttles the wrapped handler through a single process-wide token bucket (`burst` tokens, one accrued every `interval`, one consumed per admitted request); an empty bucket answers 429 (`code: "rate_limited"`) with a `Retry-After` hint. The bucket is shared across all clients: it bounds the aggregate rate of an expensive shared route, not per-client fairness. A non-positive `burst` or `interval` disables limiting (handler returned unwrapped), so a config-driven zero cleanly means "no limit". Options: `WithRateLimitWhen(pred)` (throttle only matching requests), `WithRateLimitError(code, msg)`.
- `SessionCreateRateLimit(path string) Middleware`: a `RateLimiter` preset for endpoints where each admitted request forks an expensive process; gates POST to `path` (exact match) at burst 6, one token per second. Other methods and paths pass through without consuming a token. Apps needing different tuning compose `RateLimiter` directly.

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
- `RequestLogger(next, opts...) http.Handler`: reuses a valid inbound `X-Request-ID` or mints a fresh id, echoes and threads it, records status via a `StatusRecorder`, and emits one `Info` access-log line per request.
- `WithLogger(l)` / `WithSkipPaths(paths...)` / `WithSkipFunc(fn)`: the logger and skip rules. Skipped requests still get an id minted, echoed, and threaded, but no access line and no metric hook (a stream's open-to-close duration paired with a synthetic status would mislead).
- `WithTemplatePathsUnder(prefixes ...string)`: declare URL prefixes whose concrete paths carry a CREDENTIAL, and the access line records the matched route template for them (`/api/sessions/{id}`) instead of the path. The template comes from `r.Pattern`, so the router is the source of truth and a new upstream subroute logs correctly with no change here; the method prefix is stripped. A path under a declared prefix that matched NOTHING records the prefix plus `(unmatched)` — never the raw path, because an unrouted request under a credential-bearing prefix still contains the credential, and marking it unmatched makes a new subroute visible rather than mislabelling it onto a route it is not. Paths outside every declared prefix are recorded unchanged, deliberately: a static mount's pattern is `/`, so templating everything would collapse every asset onto one line. Prefer this over hand-writing the equivalent `WithPathFunc` — two apps in this fleet wrote that transform over the same route table and diverged on the unmatched case, which is exactly the decision this makes once. Pass the prefix the route-owning package exports (e.g. the terminal engine's `SessionsSubtreePath`), not a local literal.
- `WithPathFunc(fn func(*http.Request) string)`: the recorded-path policy. fn's return replaces `r.URL.Path` in the access line, the legacy `WithRecordMetric` path argument, and the hook-failure diagnostics — the escape hatch for a path policy `WithTemplatePathsUnder` cannot express (a truncated form, a per-request decision); reach for that option first. Runs at emit time, after routing, so `r.Pattern` is populated (empty on unmatched — return your own fail-closed placeholder for those). Skip rules test the raw path and skipped requests never call it; `WithRecordMetricRequest` is unaffected (it receives the request and owns its own representation). Fail-closed: a panicking or empty-returning fn records `(path-redaction-failed)`, never the raw path.
- `WithClientIP(trusted ...*net.IPNet)` / `WithClientIPFunc(fn)`: add a `client_ip` attribute, resolved by the spoof-proof `ClientIP` or by your own function (for a dynamic or hot-reloaded trusted set). Mutually exclusive; the last one applied wins. Omitted entirely unless supplied, so the default line is unchanged.
- `WithRecordMetric(fn)` / `WithRecordMetricRequest(fn)`: a per-request metric hook, keyed on method+path or request-aware; the request-aware form can key bounded-cardinality metrics on the matched route template via `r.Pattern` (empty on an unmatched request; collapse those to one label) instead of the raw, scanner-controlled URL path. Emitted even when the handler panics. Mutually exclusive; the last one applied wins.
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
- `LimitBody(w, r, maxBytes)`: wraps the body in `http.MaxBytesReader`.
- `RequireMethod(w, r, method) bool`: 405 + `false` on mismatch.
- `MethodNotAllowed(w, r, allowed...)`: the 405 refusal on its own, for a route that permits SEVERAL methods and so has no single method to require — the `Allow` header names the whole set (`GET, POST`), the body is the standard `method_not_allowed` envelope. Route each method with the mux (or dispatch on `r.Method`) and refuse the rest with it.
- `SetAllow(w, allowed...)`: just the RFC 9110 `Allow` header, for an `OPTIONS` responder or any other advertisement outside a 405.
- `DecodeBody(w, r, v, errMsg) bool`: cap + decode (reject trailing data); 400 + `false` on failure.
- `DecodeBodyOptional(w, r, v)`: cap + decode, error ignored.
- `DecodeJSONInto(w, r, v, maxBytes) error`: the mechanism behind `DecodeBody`, for apps with their own error envelope or a per-endpoint cap; caps, decodes a single value, rejects trailing data, writes nothing, and returns the error: a `*http.MaxBytesError` (test with `errors.As`) for an oversize body, `ErrTrailingData` for a second JSON value, otherwise a malformed body. Map the result to your own status and envelope.

`Allow` is mandatory on a 405 and is a comma-separated list, so the value is the caller's set joined with `", "`: entries verbatim (a method token is case-sensitive), blanks dropped (a sender must not emit an empty list element), exact duplicates collapsed, and no methods at all rendered as the empty value the spec defines as "this resource allows no methods". `HEAD` is never implied by `GET`: `net/http`'s `ServeMux` serves `HEAD` from a `GET` pattern, so a route whose `GET` has a side effect registers `HEAD` separately to reject it and must not advertise it — pass `http.MethodHead` when the route really serves it.

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

- `NewServer(handler, opts...) *http.Server`: streaming-safe defaults: `ReadHeaderTimeout` 10s (slowloris guard), `IdleTimeout` 120s, `MaxHeaderBytes` 1 MiB; `ReadTimeout` and `WriteTimeout` unset so streaming works out of the box. Options: `WithReadTimeout`, `WithWriteTimeout`, `WithIdleTimeout`, `WithReadHeaderTimeout`, `WithMaxHeaderBytes`, `WithErrorLog`.
- `Run(ctx, srv, ln, onShutdown, opts...) error`: serves until `ctx` is cancelled, then shuts down gracefully: the pre-drain hook (if registered) runs first, then `srv.Shutdown` drains in-flight requests, then `onShutdown` runs for application teardown, all within one shared shutdown grace budget. Options: `WithShutdownGrace(d)` (default 5s); `WithPreDrain(fn)`, a hook invoked after `ctx` cancellation and strictly before the drain starts: the place to flip a readiness gate, cancel the server's `BaseContext`, or drain an SSE hub so long-lived streams release instead of holding the drain open for the whole grace window.

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
