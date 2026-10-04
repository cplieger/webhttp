# webhttp

[![Go Reference](https://pkg.go.dev/badge/github.com/cplieger/webhttp/v3.svg)](https://pkg.go.dev/github.com/cplieger/webhttp/v3) [![Go version](https://img.shields.io/github/go-mod/go-version/cplieger/webhttp)](https://github.com/cplieger/webhttp/blob/main/go.mod) [![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/webhttp/badges/mutation.json)](https://github.com/cplieger/webhttp/issues?q=label%3Agremlins-tracker)

webhttp hardens your Go `net/http` server with access logging, security headers, a client IP that cannot be spoofed and graceful shutdown.

It replaces the middleware, proxy-header parsing and shutdown sequence you would otherwise write around `http.ServeMux`, and every piece stays a plain `http.Handler`. It ships no router, and your app keeps its own routes and error codes. It uses only the standard library, needs Go 1.27.1 or later and is licensed under Apache-2.0.

## Why use it

webhttp is built for a Go service on `http.ServeMux` that faces a browser or sits behind a proxy.

- `ClientIP` reads `X-Forwarded-For` only from a proxy you trust, so a client cannot fake its address.
- A Host allowlist answers 403 to a `Host` you did not list, which stops DNS rebinding attacks.
- The access log writes `log/slog` lines and caps the path at 512 bytes and the method at 24. Metric labels stay within ten method values and your registered routes.
- `NewServer` leaves the write timeout off, so SSE and WebSocket streams stay open. `Run` drains in-flight requests within a 5-second grace period by default.
- `StaticHandler` serves an `embed.FS` with content-hash ETags and gzip computed once at startup.

Consider [chi](https://github.com/go-chi/chi) if you want a router with route groups, inline middleware and sub-router mounting. Consider [golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate) if you need a token bucket per client or one that waits instead of refusing.

## Install

```sh
go get github.com/cplieger/webhttp/v3@latest
```

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

	"github.com/cplieger/webhttp/v3"
)

func main() {
	ready := &webhttp.Ready{}

	mux := http.NewServeMux()
	mux.Handle("GET /readyz", webhttp.ReadinessHandler(ready))
	mux.HandleFunc("POST /things", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if !webhttp.DecodeBody(w, r, &body, "invalid thing payload") {
			return
		}
		webhttp.WriteJSONStatus(w, http.StatusCreated, body)
	})

	// The first middleware listed is the outermost. Logging sits outside
	// Recoverer so a recovered panic is logged as its 500.
	handler := webhttp.Chain(mux,
		webhttp.Logging(
			webhttp.WithSkipPaths("/events"), // a long-lived stream
			webhttp.WithRecordRouteMetric(func(m webhttp.RequestMetric) {
				// m.Method and m.Path are bounded labels. Feed your metrics here.
			}),
		),
		webhttp.Recoverer(),
		webhttp.SecurityHeaders(),
	)

	// Only header reads and idle connections time out, so streams stay open.
	srv := webhttp.NewServer(handler, webhttp.WithSlogErrorLog(slog.LevelError))

	// Bind first, so a port already in use fails here.
	ln, err := net.Listen("tcp", ":8080")
	if err != nil {
		panic(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ready.Set(true)
	if err := webhttp.Run(ctx, srv, ln, func(context.Context) {
		// application teardown after the drain
	}, webhttp.WithPreDrain(func(context.Context) { ready.Set(false) })); err != nil {
		panic(err)
	}
}
```

A service that browsers reach should also refuse unknown `Host` names. Build a `HostPolicy` with `ParseHostList` and put its `Middleware()` in the chain ahead of any cross-origin or CSRF check. An API that only machines call can skip it. The examples on pkg.go.dev run under `go test`, which keeps them true.

## API

- Middleware: `Chain`, `Recoverer`, `SecurityHeaders`, `Logging`, `RouteTimeout`, `RateLimiter` with its `SessionCreateRateLimit` and `FailedAuthRateLimit` presets, `NoStore`.
- Logging and metrics: `RequestLogger` and its options, request-id helpers, `RouteMetricLabels`, `StatusRecorder`.
- Trust checks: `ClientIP`, `ParseCIDRs`, `ParseHostList` and `HostPolicy`, `CanonicalHost`, `LoopbackOnly`, `ClassifyBind`, `NewStaticTokenVerifier`.
- Requests and responses: `WriteJSON`, `WriteError` and `ErrorCode`, `DecodeBody`, `DecodeJSONInto`, `LimitBody`, `RequireMethod`, `MethodNotAllowed`, `CanonicalRequestPath`.
- Static files: `StaticHandler`, `InlineScriptHashes`, `InlineStyleHashes`.
- Server: `NewServer`, `Run` and its options, `Ready` and `ReadinessHandler`, `AwaitDone`, `CausedByCancellation`.

The full reference is on [pkg.go.dev](https://pkg.go.dev/github.com/cplieger/webhttp/v3).

## Middleware order and safety rules

`Chain(h, A, B, C)` builds `A(B(C(h)))`, so the first entry sees the request first. Any `func(http.Handler) http.Handler` fits in the chain. Put `Recoverer` inside `Logging`, as in `Chain(mux, Logging(), Recoverer())`. In the other order, the access line records 200 for a request whose client received a 500.

Place a `HostPolicy` before any cross-origin or CSRF check. In a DNS rebinding attack, the page's `Origin` and the request's `Host` both name the attacker's domain, so a cross-origin check passes it and only the Host allowlist refuses it. Place `NoStore` innermost, so a handler that sets its own `Cache-Control` still wins.

`SecurityHeaders` always sends `X-Content-Type-Options: nosniff`, and by default `X-Frame-Options: DENY` and `Referrer-Policy: strict-origin-when-cross-origin`. It builds no Content-Security-Policy and sends no HSTS until you pass `WithCSP` or `WithHSTS`. Enable HSTS only for a service reached over HTTPS alone. A browser that has seen the header refuses plain-HTTP and untrusted-certificate connections to the host for the whole max-age.

`RouteTimeout` buffers the whole response, so never wrap a streaming or hijacking handler with it. [Middleware](docs/middleware.md) has each option and default.

Behind a proxy, `ClientIP` is safe only when the trusted set holds every proxy hop. `LoopbackOnly` is not authentication, because a proxy on the same loopback interface that removes every forwarding header passes it. An empty `NewStaticTokenVerifier` secret denies every value. When a page carries inline scripts, treat an empty `InlineScriptHashes` result as a startup error and never fall back to `'unsafe-inline'`.

## Graceful shutdown

When `ctx` is cancelled, `Run` runs the `WithPreDrain` hook, then `srv.Shutdown` drains in-flight requests, then `onShutdown` runs. All three share one grace budget, 5 seconds unless you set `WithShutdownGrace`. Use the pre-drain hook to mark the instance unready or close long-lived streams, so they do not hold the drain open.

When `Serve` returns before `ctx` is cancelled, for example because the listener failed, neither hook runs. Pass `WithServeExit` to run your teardown in that case too. A `Run` error caused by the grace running out wraps `ErrShutdownGraceExpired`. [Server, readiness and shutdown](docs/server.md) has the full sequence.

## Related projects

- [httpx](https://github.com/cplieger/httpx) makes the requests your service sends out survive flaky servers. webhttp handles the requests coming in.
- [health](https://github.com/cplieger/health) is the container liveness probe for a Docker `HEALTHCHECK`. `ReadinessHandler` answers a load balancer instead.
- [auth](https://github.com/cplieger/auth) handles user logins and sessions. `NewStaticTokenVerifier` checks one configured machine credential.

## Documentation

- [Middleware](docs/middleware.md) covers ordering, recovery, security headers, timeouts, rate limiting and `NoStore`.
- [Access logging and metrics](docs/access-logging.md) covers request ids, the log line, its bounds and the metric hooks.
- [Client IP, hosts and credentials](docs/trust-checks.md) covers proxy trust, the Host allowlist and loopback checks.
- [Requests and responses](docs/requests-and-responses.md) covers the JSON helpers, the error envelope, body decoding and path checks.
- [Static assets and CSP hashes](docs/static-assets.md) covers embedded files, caching and hash-pinned inline scripts.
- [Server, readiness and shutdown](docs/server.md) covers timeouts, readiness and the shutdown sequence.

## Credits

`RateLimiter`'s refill and `Retry-After` math follows [golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate), including its guard against a clock that moves backwards.

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for the conventions and how to run the checks locally.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

Apache-2.0. See [LICENSE](LICENSE).
