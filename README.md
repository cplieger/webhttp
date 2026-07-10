# webhttp

[![Go Reference](https://pkg.go.dev/badge/github.com/cplieger/webhttp.svg)](https://pkg.go.dev/github.com/cplieger/webhttp)
[![Go version](https://img.shields.io/github/go-mod/go-version/cplieger/webhttp)](https://github.com/cplieger/webhttp/blob/main/go.mod)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/webhttp/badges/coverage.json)](https://github.com/cplieger/webhttp/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/webhttp/badges/mutation.json)](https://github.com/cplieger/webhttp/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13486/badge)](https://www.bestpractices.dev/projects/13486)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/webhttp/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/webhttp)

> Resilient server-side HTTP plumbing for Go

A standalone Go library bundling the server-side pieces almost every service ends up hand-rolling: request-id injection with one-line access logging, a flush/hijack-safe status recorder, a composable middleware set (panic recovery, security headers, per-route JSON timeout, and a `Chain` combinator) with a spoof-aware client-IP resolver, JSON response and error helpers, request-prelude helpers, an HTTP readiness gate, and a graceful server bootstrap. Standard-library only, no external runtime dependencies.

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

### Middleware

All middleware share the standard `func(http.Handler) http.Handler` shape (the `Middleware` type alias) and compose with `Chain`.

- `Middleware` — alias for `func(http.Handler) http.Handler`
- `Chain(h, mw...) http.Handler` — wraps `h`; the **first** middleware listed is the **outermost** wrapper (first to see the request, last to touch the response), so `Chain(h, A, B, C)` is `A(B(C(h)))`. A nil entry is skipped.
- `DefaultStack(h, opts...) http.Handler` — the batteries-included counterpart to `Chain`: it composes `Logging`, `Recoverer`, and `SecurityHeaders` in the one correct order (Logging outermost) so the common observability-safe stack is correct-by-construction. Configure each layer with `WithLoggingOptions`, `WithRecovererOptions`, and `WithSecurityHeadersOptions`; the free functions stay the primary API for any custom stack.
- `Recoverer(opts...) Middleware` — recovers a downstream panic, logs it at `Error` with the stack and request id, fires an optional hook, then writes `WriteError(w, r, 500, "internal_error", "internal server error")`. Re-panics `http.ErrAbortHandler` per the net/http contract. Options: `WithRecoverLogger(l)`, `WithPanicHook(fn)`
- `SecurityHeaders(opts...) Middleware` — sets baseline response headers before the next handler. Always `X-Content-Type-Options: nosniff`; defaults `X-Frame-Options: DENY` and `Referrer-Policy: strict-origin-when-cross-origin`. Options: `WithCSP`, `WithFrameOptions`, `WithReferrerPolicy`, `WithPermissionsPolicy`, `WithCOOP`, `WithHSTS(maxAge, includeSubDomains, preload)`
- `Logging(opts...) Middleware` — `RequestLogger` in `Chain`-composable form; takes the same `LogOption` values. `RequestLogger(next, opts...)` stays available for direct use
- `RouteTimeout(h, d, msg) http.Handler` — wraps `http.TimeoutHandler`; on timeout emits a 503 JSON `ErrorResponse` (`code: "timeout"`) as `application/json` instead of the plain-text/HTML default
- `RateLimiter(burst, refillPerSec float64, opts...) Middleware` — throttles the wrapped handler through a single **process-wide** token bucket (standard library only): `burst` tokens, refilled at `refillPerSec` per second, one consumed per admitted request; a request that finds the bucket empty gets a 429 via `WriteError` (`code: "rate_limited"`) and does not reach the handler. The bucket is shared across all clients, so it bounds the **aggregate** rate of an expensive shared route (for example spawning a heavy child process per request), not per-client fairness. Options: `WithRateLimitWhen(pred)` limits only requests the predicate matches (throttle a single method+path on a handler serving several), `WithRateLimitError(code, msg)` overrides the 429 code and message. A **non-positive** `burst` or `refillPerSec` disables limiting (returns the handler unwrapped), so a config-driven zero cleanly means "no limit" (mirroring `RouteTimeout`'s off contract)

Put `Recoverer` **inside** `Logging` (logging outermost, e.g. `Chain(mux, Logging(), Recoverer())`) so a panicked request is logged as its 500 rather than the `StatusRecorder`'s default 200. If `Recoverer` sits outside the logger, the access line is written during the panic unwind and records 200 even though the client still receives the 500. `DefaultStack(mux, opts...)` composes exactly this order for you when you want the common stack without wiring it by hand.

`SecurityHeaders` does **not** build a Content-Security-Policy for you: a CSP is application-specific (it must match the app's own script/style sources), so pass the exact policy via `WithCSP`. Any header default can be omitted by passing an empty string (e.g. `WithFrameOptions("")` when a CSP `frame-ancestors` supersedes it). **HSTS is off by default**; enable it with `WithHSTS` only for a service reached exclusively over HTTPS, since the header makes browsers refuse plain-HTTP and untrusted-cert connections for the whole max-age.

`RouteTimeout` **cannot wrap streaming or hijacking handlers**: `http.TimeoutHandler` buffers the entire response so it can discard it on timeout, so SSE, WebSocket upgrades, and flushing responses do not work through it. Use per-request deadlines (`http.ResponseController.SetWriteDeadline`) for those. Because the body is produced outside request scope, the timeout envelope carries no `request_id`.

### Client IP

- `ClientIP(r, trusted...) string` — the best-effort client IP
- `ParseCIDRs(entries) (nets, invalid)` — parse a config/env list of CIDRs or bare IPs into the trusted-proxy `[]*net.IPNet` for `ClientIP`/`WithClientIP`

With **no** trusted ranges (or when the direct peer is not inside one), `X-Forwarded-For` is ignored entirely and the host part of `r.RemoteAddr` (the TCP peer, unspoofable at this layer) is returned — the spoof-proof default, past which no client-sent header can move the result. Only when the direct peer **is** a trusted proxy is `X-Forwarded-For` consulted, and then it is walked **right-to-left**: each entry that is itself a trusted proxy (one of your own hops, which appended the address it saw) is skipped, and the first untrusted entry from the right is returned as the client. This is the only correct reading when the proxy _appends_ the peer it observed to the header (as Caddy and most reverse proxies do): the **leftmost** entry is then whatever the client _sent_ and is attacker-controlled. The trusted set must therefore contain **every** proxy hop between the client and the server; if a hop is missing the walk stops there and that hop's address is returned. `X-Real-IP` is **not** consulted — it is client-settable and Caddy does not overwrite it, so honoring it would reintroduce a spoof vector (it may return later as an explicit opt-in for a proxy that overwrites it). The caller supplies the trusted CIDRs (typically the reverse proxy's range); the library hardcodes none.

`ParseCIDRs(entries []string) (nets []*net.IPNet, invalid []string)` turns an operator-supplied list (a config-file array or a comma-split env var) into that trusted set: each entry is a CIDR (`10.0.0.0/8`) or a bare IP (`192.168.1.5`, treated as a `/32`/`/128`), blanks are skipped, and malformed entries are returned separately so a strict caller can reject them (config validation) while a lenient one can log and use the valid subset (an env var). It exists so every consumer shares one parser instead of reimplementing the CIDR/bare-IP handling; feed its result straight to `ClientIP`/`WithClientIP`.

### Status recorder

`StatusRecorder` wraps an `http.ResponseWriter` to capture the response status while staying transparent to streaming. It works two complementary ways: `Unwrap` lets `http.NewResponseController(rec)` walk to the underlying writer's `Flusher`, `Hijacker`, and deadline setters, and it also implements `http.Flusher`/`http.Hijacker`/`io.ReaderFrom` directly, so a handler or library that type-asserts those interfaces on the writer (as gorilla/websocket does with `w.(http.Hijacker)`) still works and `io.Copy`/`http.ServeContent` keep the zero-copy sendfile fast path. Each passthrough returns the underlying writer's own result (e.g. `Hijack` errors on an HTTP/2 stream).

- `NewStatusRecorder(w) *StatusRecorder` — status defaults to 200
- `(*StatusRecorder).WriteHeader(code)` — records the first explicit code only
- `(*StatusRecorder).Write(b)` — implicit 200 on first write
- `(*StatusRecorder).Status() int`
- `(*StatusRecorder).Wrote() bool` — reports whether the response is committed (WriteHeader, the first Write/ReadFrom, or a successful Flush/Hijack); the seam Recoverer uses to skip a double-write onto an already-started response
- `(*StatusRecorder).Unwrap() http.ResponseWriter`
- `(*StatusRecorder).Flush()` / `.Hijack()` / `.ReadFrom(src)` — passthroughs to the underlying writer

### Request id and access logging

- `HeaderRequestID` — the `X-Request-ID` header constant
- `ValidRequestID(s) bool` — 1 to 64 chars, each `[A-Za-z0-9_-]`
- `NewRequestID() string` — 16 random bytes hex-encoded, with a charset-safe timestamp fallback
- `WithRequestID(ctx, id)` / `RequestIDFromContext(ctx) string`
- `RequestLogger(next, opts...) http.Handler` — mints/echoes/threads the id, records status via a `StatusRecorder`, emits one `Info` access-log line per request
- Options: `WithLogger(l)`, `WithSkipPaths(paths...)`, `WithSkipFunc(fn)`, `WithRecordMetric(fn)`, `WithClientIP(trusted...)`, `WithClientIPFunc(fn)`
- `WithClientIP(trusted ...*net.IPNet)` adds a `client_ip` attribute resolved by `ClientIP` (spoof-proof; honors `X-Forwarded-For` only from the trusted proxy ranges you pass, else logs the socket peer). Omitted entirely unless the option is supplied, so the default access line is unchanged.
- `WithClientIPFunc(fn func(*http.Request) string)` is the same attribute but resolved by your own function — use it when the trusted set is dynamic (reloaded from config at runtime) or resolution is app-specific. Mutually exclusive with `WithClientIP`; the last one applied wins.

An inbound `X-Request-ID` is reused when it satisfies `ValidRequestID`, otherwise a fresh id is minted. Skip-path requests still get an id minted, echoed, and threaded, but are served through the raw writer with no access-log line and no metric hook (a stream's open-to-close duration paired with a synthetic status would be misleading, which is why the path is skipped).

### JSON responses and errors

- `JSONHeaders(w)` — `application/json` + `X-Content-Type-Options: nosniff`
- `WriteJSON(w, v)` — 200
- `WriteJSONStatus(w, code, v)` — headers, status, encode (encode failure logged at `Warn`, not returned)
- `Ok(w)` — 200 `{"ok":true}`
- `WriteError(w, r, status, code, msg)` — writes `ErrorResponse`; nil-safe when `r` is nil
- `ErrorResponse{Error, Code, RequestID}` — `Code` and `RequestID` omitted when empty

`WriteError` pulls the request id from the request context so a client can correlate a failure with the access log. It ships the mechanism; keep your own named-helper and error-code taxonomy on top.

### Request prelude

- `MaxJSONBody` — 1 MiB default body cap
- `LimitBody(w, r, maxBytes)` — wraps the body in `http.MaxBytesReader`
- `RequireMethod(w, r, method) bool` — 405 + `false` on mismatch
- `DecodeBody(w, r, v, errMsg) bool` — cap + decode (reject trailing data); 400 + `false` on failure
- `DecodeBodyOptional(w, r, v)` — cap + decode, error ignored
- `DecodeJSONInto(w, r, v, maxBytes) error` — the mechanism behind `DecodeBody`, exposed for apps with their own error envelope or a per-endpoint cap: cap + decode + reject-trailing, **writing nothing** and returning the decode error. A `*http.MaxBytesError` (test with `errors.As`) means the body exceeded `maxBytes` (map to 413 or 400 as you choose); `ErrTrailingData` means a second JSON value followed the first; otherwise it's a malformed body. Map the result to your own status/envelope.

### Readiness

- `Ready` — a concurrency-safe flag; zero value is not ready
- `(*Ready).Set(ready)` / `(*Ready).Ready() bool`
- `ReadinessChecker` — the `Ready() bool` interface `*Ready` satisfies
- `ReadinessHandler(c) http.HandlerFunc` — 200 `{"status":"ok"}` when ready, else 503 `{"status":"unready","reason":"starting up or shutting down"}`

This is the HTTP serving-state gate (lowercase `"ok"`), for a load balancer asking "should this instance receive traffic right now?". It is deliberately distinct from the [health](https://github.com/cplieger/health) library's container file-marker probe (`{"status":"OK","timestamp":…}`), which answers "is the process alive?" for a Docker `HEALTHCHECK`. The two are complementary, not the same endpoint.

### Server

- `NewServer(handler, opts...) *http.Server` — streaming-safe defaults: `ReadHeaderTimeout` 10s (slowloris guard), `IdleTimeout` 120s, `MaxHeaderBytes` 1 MiB; `ReadTimeout` and `WriteTimeout` unset so streaming works out of the box
- Options: `WithReadTimeout`, `WithWriteTimeout`, `WithIdleTimeout`, `WithReadHeaderTimeout`, `WithMaxHeaderBytes`, `WithErrorLog`
- `Run(ctx, srv, ln, onShutdown, opts...) error` — serves until `ctx` is cancelled, then shuts down gracefully within the shutdown grace period and runs `onShutdown` for application teardown
- Option: `WithShutdownGrace(d)` (default 5s)

Streaming apps (SSE, WebSocket, long responses) MUST omit `WithWriteTimeout`, since a write deadline would cut off an in-progress stream. Bind the listener up front (for example with `net.ListenConfig.Listen`) so a port-in-use error surfaces synchronously before `Run`, and pass application teardown as `onShutdown`.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude Opus](https://www.anthropic.com/claude) and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0 — see [LICENSE](LICENSE).
