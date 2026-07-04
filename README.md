# webhttp

[![Go Reference](https://pkg.go.dev/badge/github.com/cplieger/webhttp.svg)](https://pkg.go.dev/github.com/cplieger/webhttp)
[![Go version](https://img.shields.io/github/go-mod/go-version/cplieger/webhttp)](https://github.com/cplieger/webhttp/blob/main/go.mod)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/webhttp/badges/coverage.json)](https://github.com/cplieger/webhttp/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/webhttp/badges/mutation.json)](https://github.com/cplieger/webhttp/issues?q=label%3Agremlins-tracker)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/webhttp/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/webhttp)

> Resilient server-side HTTP plumbing for Go

A standalone Go library bundling the server-side pieces almost every service ends up hand-rolling: request-id injection with one-line access logging, a flush/hijack-safe status recorder, JSON response and error helpers, request-prelude helpers, an HTTP readiness gate, and a graceful server bootstrap. Standard-library only, no external runtime dependencies.

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

	// RequestLogger mints/echoes a request id, threads it through the context,
	// and logs one line per request. Skip long-lived streams so they don't emit
	// a misleading high-latency line at close.
	handler := webhttp.RequestLogger(mux,
		webhttp.WithSkipPaths("/events"),
		webhttp.WithRecordMetric(func(method, path string, status int, d time.Duration) {
			// feed your metrics pipeline here
		}),
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

### Status recorder

`StatusRecorder` wraps an `http.ResponseWriter` to capture the response status while staying transparent to `http.ResponseController`. Its `Unwrap` method is the point: `http.NewResponseController(rec)` walks `Unwrap` to reach a `Flusher` or `Hijacker` on the underlying writer, so SSE, WebSocket, and other streaming handlers keep working behind status-capturing middleware.

- `NewStatusRecorder(w) *StatusRecorder` — status defaults to 200
- `(*StatusRecorder).WriteHeader(code)` — records the first explicit code only
- `(*StatusRecorder).Write(b)` — implicit 200 on first write
- `(*StatusRecorder).Status() int`
- `(*StatusRecorder).Unwrap() http.ResponseWriter`

### Request id and access logging

- `HeaderRequestID` — the `X-Request-ID` header constant
- `ValidRequestID(s) bool` — 1 to 64 chars, each `[A-Za-z0-9_-]`
- `NewRequestID() string` — 16 random bytes hex-encoded, with a charset-safe timestamp fallback
- `WithRequestID(ctx, id)` / `RequestIDFromContext(ctx) string`
- `RequestLogger(next, opts...) http.Handler` — mints/echoes/threads the id, records status via a `StatusRecorder`, emits one `Info` access-log line per request
- Options: `WithLogger(l)`, `WithSkipPaths(paths...)`, `WithRecordMetric(fn)`

An inbound `X-Request-ID` is reused when it satisfies `ValidRequestID`, otherwise a fresh id is minted. Skip-path requests still get an id minted, echoed, and threaded, but are served through the raw writer with no access-log line; a metric hook, if set, still fires for them.

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
- `DecodeBody(w, r, v, errMsg) bool` — cap + decode; 400 + `false` on failure
- `DecodeBodyOptional(w, r, v)` — cap + decode, error ignored
- `LimitedWriter{W, N}` — caps total bytes forwarded, silently dropping the rest

### Readiness

- `Ready` — a concurrency-safe flag; zero value is not ready
- `(*Ready).Set(ready)` / `(*Ready).Ready() bool`
- `ReadinessChecker` — the `Ready() bool` interface `*Ready` satisfies
- `ReadinessHandler(c) http.HandlerFunc` — 200 `{"status":"ok"}` when ready, else 503 `{"status":"unready","reason":"starting up or shutting down"}`

This is the HTTP serving-state gate (lowercase `"ok"`), for a load balancer asking "should this instance receive traffic right now?". It is deliberately distinct from the [health](https://github.com/cplieger/health) library's container file-marker probe (`{"status":"OK","timestamp":…}`), which answers "is the process alive?" for a Docker `HEALTHCHECK`. The two are complementary, not the same endpoint.

### Server

- `NewServer(handler, opts...) *http.Server` — streaming-safe defaults: `ReadHeaderTimeout` 10s (slowloris guard), `IdleTimeout` 120s, `MaxHeaderBytes` 1 MiB; `ReadTimeout` and `WriteTimeout` unset so streaming works out of the box
- Options: `WithReadTimeout`, `WithWriteTimeout`, `WithIdleTimeout`, `WithReadHeaderTimeout`, `WithMaxHeaderBytes`
- `Run(ctx, srv, ln, onShutdown, opts...) error` — serves until `ctx` is cancelled, then shuts down gracefully within the shutdown grace period and runs `onShutdown` for application teardown
- Option: `WithShutdownGrace(d)` (default 5s)

Streaming apps (SSE, WebSocket, long responses) MUST omit `WithWriteTimeout`, since a write deadline would cut off an in-progress stream. Bind the listener up front (for example with `net.ListenConfig.Listen`) so a port-in-use error surfaces synchronously before `Run`, and pass application teardown as `onShutdown`.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude Opus](https://www.anthropic.com/claude) and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0 — see [LICENSE](LICENSE).
