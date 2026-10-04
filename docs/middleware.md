# Middleware

This page is for a developer assembling a webhttp middleware stack. It covers how `Chain` orders middleware, panic recovery, security headers, per-route timeouts, rate limiting and `NoStore`.

## Chain and ordering

Every middleware has the standard `func(http.Handler) http.Handler` shape, which the `Middleware` type alias names. A standard-library middleware therefore drops into the same chain with no adapter. The method value `cop.Handler` of an `http.NewCrossOriginProtection()` is one:

```go
cop := http.NewCrossOriginProtection()
cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	webhttp.WriteError(w, r, http.StatusForbidden, "cross_origin_denied", "cross-origin request denied")
}))
h := webhttp.Chain(mux, webhttp.Logging(), webhttp.Recoverer(), hostPolicy.Middleware(), cop.Handler, webhttp.SecurityHeaders())
```

The deny handler runs inside the chain, so the refusal uses the webhttp error envelope with its request id. Keep the Host allowlist outside it, for the reason [Client IP, hosts and credentials](trust-checks.md#host-allowlist) gives.

`Chain(h, mw...)` wraps `h`, and the first middleware listed is the outermost wrapper. So `Chain(h, A, B, C)` is `A(B(C(h)))`. A nil entry is skipped.

The usual stack is `Chain(mux, Logging(), Recoverer(), SecurityHeaders())`. Logging is outermost, so a panic that `Recoverer` catches below it is logged as the 500 the client received. With `Recoverer` outside the logger, the access line records the status recorder's default 200 instead.

## Recoverer

`Recoverer(opts...)` recovers a panic from a downstream handler. It logs the panic at `Error` with the stack and the request id, calls any `WithPanicHook` callback, then writes a 500.

The 500 goes through the configured `ErrorResponder`. The default is `WriteError`, which sends `{"error":"internal server error","code":"internal_error"}`. Pass `WithRecoverResponder` to render the 500 on another content type, and `WithRecoverLogger` to log somewhere other than `slog.Default()`.

If the handler already wrote headers or body before it panicked, the status is on the wire. `Recoverer` then skips the 500 body, but it still logs the panic and calls the hook.

`http.ErrAbortHandler` is not recovered. `Recoverer` panics again with it, as the `net/http` contract asks, so the server aborts the response the way the handler meant. It is not logged and fires no hook.

## SecurityHeaders

`SecurityHeaders(opts...)` sets baseline response headers. It always sends `X-Content-Type-Options: nosniff`. By default it also sends `X-Frame-Options: DENY` and `Referrer-Policy: strict-origin-when-cross-origin`.

| Option | Sets |
| --- | --- |
| `WithCSP(policy)` | `Content-Security-Policy`, none by default |
| `WithFrameOptions(v)` | `X-Frame-Options`, default `DENY` |
| `WithReferrerPolicy(v)` | `Referrer-Policy`, default `strict-origin-when-cross-origin` |
| `WithPermissionsPolicy(v)` | `Permissions-Policy`, none by default |
| `WithCOOP(v)` | `Cross-Origin-Opener-Policy`, none by default |
| `WithHSTS(HSTS{...})` | `Strict-Transport-Security`, off by default |

Pass an empty string to leave out a header that has a default. The middleware builds no Content-Security-Policy, because a policy must match your own script and style sources. Pass the exact policy with `WithCSP`. [Static assets and CSP hashes](static-assets.md) shows how to hash-pin inline scripts in it.

### HSTS

Enable HSTS only for a service reached over HTTPS alone. A browser that has seen the header refuses plain-HTTP and untrusted-certificate connections to the host for the whole max-age.

`HSTS` has three named fields: `MaxAge`, `IncludeSubdomains` and `Preload`. `MaxAge` is truncated to whole seconds, and a negative value counts as zero.

The zero `HSTS` value renders `max-age=0`, which tells a browser to forget a policy it already holds. To turn HSTS off, leave `WithHSTS` out.

The browsers' preload list sets two rules, and `WithHSTS` enforces both. `Preload` needs `IncludeSubdomains`, and it needs a `MaxAge` of at least one year. A policy that breaks either rule has the `preload` directive dropped from the header, and the problem is logged.

The max-age rule also catches a common unit mistake. A bare `31536000` assigned to a `time.Duration` field is nanoseconds, so it would render `max-age=0` beside `preload`.

`HSTS.Validate()` is the strict check for a caller that would rather refuse to start. It returns `ErrHSTSPreloadWithoutSubdomains` or `ErrHSTSPreloadMaxAgeTooShort`, and nil otherwise. Without `Preload`, any `MaxAge` is valid.

## Logging

`Logging(opts...)` is `RequestLogger` in a form `Chain` can compose, and it takes the same options. [Access logging and metrics](access-logging.md) covers them.

## RouteTimeout

`RouteTimeout(h, d, msg)` wraps `h` with `http.TimeoutHandler`. A handler that runs longer than `d` is cut off with a 503 and a JSON `ErrorResponse` whose code is `timeout`. An empty `msg` becomes `request timed out`. The body carries the request id when the context has one.

A non-positive `d` returns `h` unwrapped, so a configured zero means no timeout.

The JSON relabelling keys on the status alone. A downstream 503 that reaches the client without a `Content-Type` is also served as `application/json`, with its body unchanged. A handler that sends its own 503 sets an explicit `Content-Type`.

`http.TimeoutHandler` buffers the entire response. So `RouteTimeout` cannot wrap SSE, WebSocket upgrades or any other streaming or hijacking handler. For those, set per-request deadlines with `http.ResponseController.SetWriteDeadline`.

That buffering writer cannot be unwrapped either. Under it, `LimitBody` still fails an over-limit read, but it cannot ask `net/http` to close the connection.

## RateLimiter

`RateLimiter(burst, interval, opts...)` throttles the wrapped handler through one token bucket for the whole process. The bucket holds `burst` tokens, gains one every `interval`, and each admitted request takes one.

An empty bucket answers 429 with the code `rate_limited`, the message `rate limit exceeded` and a `Retry-After` header. The hint is the whole seconds until the missing fraction of a token refills, rounded up and never below 1.

The bucket is shared by every client. It bounds the total rate of an expensive shared route, not fairness between clients. A caller that needs per-client limits keys its own buckets on `ClientIP`. The bucket lives in process memory, so replicas do not share it.

A non-positive `burst` or `interval` returns the handler unwrapped, so a configured zero means no limit.

| Option | Effect |
| --- | --- |
| `WithRateLimitWhen(pred)` | Throttle only the requests `pred` matches. The rest pass without taking a token |
| `WithRateLimitError(code, msg)` | Set the 429 envelope's code and message |
| `WithRateLimitResponder(fn)` | Render the 429 through your own `ErrorResponder`, the hook `Recoverer` takes for its 500 |

### Presets

`SessionCreateRateLimit(path)` is for an endpoint where each admitted request starts an expensive process. It throttles `POST` requests to `path`, an exact match, at a burst of 6 and one token per second. Its 429 says `session creation rate exceeded`. Other methods and paths pass through without taking a token.

`FailedAuthRateLimit(when, msg)` is for a route guarded by one static credential. It throttles the requests `when` reports as presenting a failed credential, at a burst of 10 and one token every 6 seconds. It answers 429 with the fixed code `too_many_auth_failures` and `msg`.

A valid credential never takes a token, so the tuning does not need to leave room for your own senders. `msg` is yours because the credential differs per service. An empty `msg` becomes `too many failed authentication attempts`. A nil `when` throttles every request the middleware sees, for a caller that has already filtered the failed requests itself.

The failed-auth preset caps how many failed attempts reach the guarded handler. The `when` predicate runs on every request before the bucket check, so a credential check inside it is not capped. Access lines are capped only when the limiter sits outside `Logging`. Without the preset, a guessing run at wire speed gets a 401 and an access line for every attempt.

For other numbers, compose `RateLimiter` directly.

## NoStore

`NoStore()` sets `Cache-Control: no-store` on every response that passes through it, before the next handler runs. The value is fixed. Per-asset cache policy belongs in `WithStaticCacheControl`.

The header is set, not locked. A handler or inner middleware that needs its own value sets `Cache-Control` and wins, which is why the usual place is innermost in the chain. Scope it by mounting it on the subtree that needs it.

`NoStore` is not for a conditional no-store, such as a response that is uncacheable only when it carries a `Set-Cookie`. That rule belongs with the code that sets the cookie.
