# Server, readiness and shutdown

This page is for a developer starting and stopping a webhttp server. It covers the server defaults, routing `net/http`'s own errors into `slog`, the readiness endpoint and the shutdown sequence `Run` follows.

## NewServer

`NewServer(handler, opts...)` returns an `*http.Server` with defaults that suit streaming:

| Setting | Default | Option |
| --- | --- | --- |
| `ReadHeaderTimeout` | 10s, a slowloris guard | `WithReadHeaderTimeout` |
| `IdleTimeout` | 120s | `WithIdleTimeout` |
| `MaxHeaderBytes` | 1 MiB | `WithMaxHeaderBytes` |
| `ReadTimeout` | unset | `WithReadTimeout` |
| `WriteTimeout` | unset | `WithWriteTimeout` |
| `ErrorLog` | the standard logger | `WithErrorLog`, `WithSlogErrorLog` |

With no read or write timeout, SSE, WebSocket and other long-lived responses work. A streaming app never sets `WithWriteTimeout`, because a write deadline cuts off a stream in progress.

Only header reading is bounded by time, so a slow request body is not. A handler that does not stream adds `WithReadTimeout`. A streaming handler sets per-request deadlines with `http.ResponseController.SetReadDeadline` and `SetWriteDeadline` instead. `LimitBody` bounds the size of a body, not the time it takes to send.

A field `NewServer` does not set needs no option, because you can assign it on the returned server, such as `srv.MaxHeaderValueCount = 64`. A request refused by that cap, or by `MaxHeaderBytes`, gets its 431 below the handler. It therefore produces no access line and no security headers.

### net/http's own errors

`WithSlogErrorLog(level)` sends `net/http`'s connection-level lines into `slog` at `level`. The main one is `http: Accept error: ...; retrying`, the sign that the process has run out of file descriptors, which no request log reports.

The level is your choice. An accept failure is fatal for a service whose only job is answering probes, and a recoverable problem for one that retries.

The option reads `slog.Default()` when `NewServer` applies it. Install your process logger first, because a later `slog.SetDefault` does not change a server already built. `WithErrorLog` takes any other `*log.Logger`. With neither option, `net/http`'s standard logger is used.

## Readiness

- `Ready` is a flag that is safe for concurrent use. Its zero value is not ready.
- `(*Ready).Set(ready)` and `(*Ready).Ready()` write and read it.
- `ReadinessChecker` is the `Ready() bool` interface that `*Ready` satisfies.
- `ReadinessHandler(c)` answers 200 `{"status":"ok"}` when `c` is ready, and otherwise 503 `{"status":"unready","reason":"starting up or shutting down"}`.

Both responses carry `Cache-Control: no-store`. Under RFC 9111, a 200 with no freshness information may be cached heuristically. A cached "ok" that outlives the readiness it reported would keep traffic arriving at an instance that has started to drain.

This endpoint answers a load balancer asking whether the instance should receive traffic now. It is a different endpoint from the [health](https://github.com/cplieger/health) library's container probe, which answers whether the process is alive for a Docker `HEALTHCHECK`, with `{"status":"OK","timestamp":...}`. Use both.

## Run

`Run(ctx, srv, ln, onShutdown, opts...)` serves `srv` on `ln` until `ctx` is cancelled, then shuts down. Bind `ln` before you call it, for example with `net.ListenConfig.Listen`, so a port already in use fails at once.

When `ctx` is cancelled, `Run` sets one deadline, now plus the grace, and runs three steps against it:

1. The `WithPreDrain` hook, if one is registered.
2. `srv.Shutdown`, which drains in-flight requests.
3. `onShutdown`, if it is not nil, for your application's teardown.

Each step gets what the earlier steps left of the one budget, not a fresh window. `WithShutdownGrace(d)` sets the grace, 5 seconds by default.

`Run` treats `http.ErrServerClosed` as a clean stop. It returns the first other error it sees, a serve error before a shutdown error, or nil after a clean stop.

### The pre-drain hook

`WithPreDrain(fn)` runs after `ctx` is cancelled and strictly before the drain starts. Use it to mark the instance unready so a load balancer stops sending traffic, and to end long-lived connections. That can mean cancelling the server's `BaseContext` or closing an SSE hub. Otherwise `Shutdown` waits the whole grace for those connections. A nil `fn` is ignored.

### When Serve stops on its own

`Run` has two exits, and only one is the graceful sequence. When `Serve` returns before `ctx` is cancelled, because the accept loop died or you called `Shutdown` or `Close` yourself, the listener is already gone. Neither `WithPreDrain` nor `onShutdown` runs, and `Run` returns the serve error.

`WithServeExit(fn)` registers the teardown for that path. `fn` gets the whole grace, because no drain spent any of it, and `Run` does not call `srv.Shutdown` behind it. Exactly one of the two paths runs per call. Without the option, nothing runs on that path.

`ctx` is still live there. So a teardown that waits on a goroutine stopped by the same signal context must cancel that context inside `fn`, or it waits out the whole grace. A teardown that does not depend on cancellation can go in both places: `Run(ctx, srv, ln, teardown, WithServeExit(teardown))`.

### Telling errors apart

`Run`'s error can carry a deadline from two places. One is your own deadline inside a serve error, and the other is the shutdown grace running out. Both satisfy `errors.Is(err, context.DeadlineExceeded)`. A grace expiry is also wrapped in `ErrShutdownGraceExpired`, so `errors.Is(err, webhttp.ErrShutdownGraceExpired)` identifies it. A real serve error takes precedence and is never marked.

`CausedByCancellation(ctx, err)` reports whether `err` is this context's cancellation, so a boundary can tell a routine stop from a fault that happened at the same moment. A cancelled context alone is not proof, because a bind that really failed while a signal arrived must still read as a failure.

It matches `context.Cause(ctx)` as well as `ctx.Err()`. A cause passed to `context.WithCancelCause` need not wrap `context.Canceled`, and `net/http` reports the cause as it is. A nil context, a nil error or a context that is not cancelled report false.

### Waiting in a teardown

`AwaitDone(ctx, done)` waits until `done` closes or `ctx` expires, and reports whether `done` closed in time. It creates no timeout of its own and logs nothing. `onShutdown` already receives what is left of the grace, so a fresh deadline would be a budget the shutdown does not have.

After `ctx` fires, it checks `done` once more. A drain that used the whole grace hands the teardown an expired context. A `select` with both cases ready picks one at random, so the plain two-case wait sometimes reports a finished teardown as still running. A nil `done` never closes, so the wait then ends with `ctx`.

What to log, at which level, and what exit code to use stay your decisions for all three helpers.
