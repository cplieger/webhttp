# Contributing to webhttp

Notes on the public API, the invariants that make the plumbing safe, and the
test suite that guards them.

## What the library provides

`webhttp` is a standard-library-only Go package of server-side HTTP plumbing.
It has no external runtime dependencies, and it must stay that way: every
dependency would be inherited by every consuming service. The package groups a
set of small, independent pieces:

- `StatusRecorder`: a status-capturing `http.ResponseWriter` wrapper,
- `RequestLogger` / `Logging`: request-id middleware with one-line access
  logging,
- a composable middleware set: `Chain`, `Recoverer`, `SecurityHeaders`,
  `RouteTimeout`, the shared-bucket `RateLimiter` and its
  `SessionCreateRateLimit` preset,
- the `ClientIP` resolver plus `ParseCIDRs`,
- the exact-match Host allowlist: `ParseHostList`, `CanonicalHost`,
  `HostPolicy`,
- the bind-exposure classifier: `ClassifyBind`, `ClassifyBindHost`,
  `BindClass`,
- static assets and CSP: `StaticHandler`, `InlineScriptHashes`,
- JSON helpers: `WriteJSON`, `WriteJSONStatus`, `Ok`, `WriteError`,
- request-prelude helpers: `LimitBody`, `RequireMethod`, `DecodeBody`,
  `DecodeJSONInto`,
- the constant-time `StaticTokenVerifier`,
- a readiness gate: `Ready`, `ReadinessHandler`,
- a graceful server bootstrap: `NewServer`, `Run`,
- the `sse` subpackage: a Server-Sent-Events broadcast hub with replay and
  `Last-Event-ID` resume.

## Invariants to preserve

A few properties are load-bearing. Keep them when you change the code.

- **`StatusRecorder` stays transparent to `http.ResponseController`.** The
  whole reason the recorder is safe to wrap around streaming handlers is its
  `Unwrap` method: `http.NewResponseController` walks `Unwrap` to reach the
  `Flusher` / `Hijacker` on the underlying writer. Never add a method to
  `StatusRecorder` that shadows an optional `http.ResponseWriter` interface
  (such as `Flush` or `Hijack`) unless you forward it correctly; doing so would
  break SSE and WebSocket handlers running behind the middleware. Only the
  first explicit `WriteHeader` code is recorded, matching net/http semantics.
- **`ValidRequestID` is the trust boundary for the echoed id.** The id is
  written back on a response header and into log lines, so it must reject any
  byte outside `[A-Za-z0-9_-]` and anything longer than 64 chars. That is what
  stops log-forging newlines and header-splitting content. `NewRequestID`
  output (hex) must stay inside the same charset.
- **`NewServer` defaults are streaming-safe.** `ReadTimeout` and `WriteTimeout`
  are deliberately left unset (0) so SSE, WebSocket, and long responses work
  out of the box; a `WriteTimeout` would cut an in-progress stream. Keep the
  slowloris guard (`ReadHeaderTimeout`) and the header-size cap.
- **`Run`'s shutdown ordering.** On context cancellation `Run` computes one
  shutdown deadline (now + grace) and runs the whole sequence against it: the
  `WithPreDrain` hook first (if registered), then `srv.Shutdown`, then
  `onShutdown`; each later phase gets whatever budget remains, not a fresh
  window. `http.ErrServerClosed` is a clean stop, and a real serve error takes
  precedence over a shutdown error in the return value.
- **`WriteError` is nil-safe.** It must not panic when `r` is nil; the
  `RequestID` field simply stays empty.
- **`Chain` order and `Recoverer` placement.** `Chain` applies middleware so the
  first listed is the outermost wrapper (`Chain(h, A, B, C)` is `A(B(C(h)))`).
  `Recoverer` must re-panic `http.ErrAbortHandler` untouched (the net/http
  silent-abort contract) and is documented to sit inside `Logging` so a
  recovered request records its 500 before the deferred access line runs.
- **`ClientIP` trusts `X-Forwarded-For` only from a trusted peer, and walks it
  right-to-left.** With no trusted ranges (or an untrusted direct peer) it
  returns the `RemoteAddr` host and ignores `X-Forwarded-For`. Only when the
  direct peer is inside a caller-supplied trusted range is the header
  consulted, and then it is walked from the right, skipping trusted-proxy
  hops, down to the first untrusted entry (the client). That is the correct
  reading when a proxy appends the peer it saw, which makes the leftmost entry
  the attacker-controlled value the client sent; the trusted set must
  therefore contain every proxy hop. `X-Real-IP` is deliberately not consulted
  (client-settable, and not overwritten by common proxies, so it would be a
  spoof vector). The library hardcodes no CIDR.
- **`CanonicalHost` rejects malformed authorities, never repairs them.**
  Repair (stripping stray brackets, truncating at a bad port) would collapse
  distinct wire values onto allowlisted keys and silently widen an exact-match
  gate; anything that does not parse strictly returns `""` and is rejected.
  Matching stays purely textual: no name resolution, no IDN mapping.
- **`SecurityHeaders` never builds a CSP.** A Content-Security-Policy is
  application-owned; the middleware only sets what `WithCSP` is given. HSTS
  stays off unless `WithHSTS` is passed.
- **Functional options skip nil.** Every `...Option` loop, and `Chain` itself,
  ignores a nil entry so callers can pass conditionally-built values.
- **`RateLimiter`'s non-positive contract is "off", not "unlimited".** A `burst`
  or `interval` `<= 0` returns the next handler unwrapped (no bucket
  allocated), so a config-driven zero means "no limit" without the caller
  special-casing it: the same off contract as `RouteTimeout` and an inactive
  `HostPolicy`. The bucket is a single process-wide instance shared across all
  clients (it bounds the aggregate rate of the wrapped route, not per-client
  fairness), and the empty-bucket 429 flows through `WriteError` so the
  throttled response stays the standard JSON envelope. Keep all three
  properties if you touch it.

## Local development

The module targets the Go version pinned in `go.mod`.

```sh
go build ./...
go test ./...
go test -race ./...
go test -cover ./...
```

### Linting and formatting

Lint config lives in `.golangci.yaml` (golangci-lint v2, synced from
`cplieger/ci`). Formatting is `gofumpt` with `extra-rules` plus `gci` import
grouping; `golangci-lint run` reports unformatted files as issues, so format
before pushing.

```sh
golangci-lint run
golangci-lint fmt
```

### Mutation testing

`.gremlins.yaml` configures [Gremlins](https://gremlins.dev) mutation testing
(synced from `cplieger/ci`; change it upstream). Run it locally to check that
new tests actually kill mutants:

```sh
gremlins unleash .
```

## Test suite conventions

Tests are **standard library only**: `testing` plus `net/http/httptest`. Do
not add a third-party test dependency (no `testify`, no `rapid`); it would show
up in `go.sum` and, for a zero-dependency library, that is a regression. Use
plain `if got != want { t.Errorf(...) }`, table-driven subtests, and
`httptest` throughout.

Tests live beside the code, one `_test.go` file per source unit, in both the
root package and `sse/`. Parser, validator, and encoder surfaces additionally
carry fuzz targets in `*_fuzz_test.go` files; add one when you introduce a new
input-parsing surface. `helpers_test.go` holds the shared handlers and the
capturing `slog.Handler`; `example_test.go` keeps runnable `Example` functions
compiling.

Tests that capture `slog` output by swapping `slog.Default()` mutate
process-global state, so they must run serially (no `t.Parallel()`); prefer
injecting a logger with `WithLogger` where the API allows it.

## Commits and PRs

Branch from `main`, keep changes focused with tests, and open a PR. This
account uses [Conventional Commits](https://www.conventionalcommits.org/)
parsed by git-cliff (`cliff.toml`) to build release notes, so the commit type
drives the version bump: `feat:`, `fix:`, `sec:`, and
`chore:`/`docs:`/`refactor:`/`test:` (no release). Write the subject as the
changelog line a consumer would read.

## Conduct & security

By participating you agree to the org-wide
[Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md).
Report security issues through the
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md),
never in a public issue.
