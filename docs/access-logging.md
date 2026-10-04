# Access logging and metrics

This page is for a developer wiring webhttp's access log and request metrics. It covers request ids, the one-line access record, the bounds on what it records, the level policy, the metric hooks and the status recorder.

## Request ids

- `HeaderRequestID` is the `X-Request-ID` header name.
- `ValidRequestID(s)` accepts 1 to 64 characters, each one of `A-Z`, `a-z`, `0-9`, `_` or `-`.
- `NewRequestID()` returns 16 random bytes from `crypto/rand`, hex-encoded.
- `WithRequestID(ctx, id)` and `RequestIDFromContext(ctx)` carry the id through a context.

`WriteError` and `RouteTimeout` put the context's id in their error bodies, and `Recoverer` puts it in the panic log. When the context carries no id, the field is left out.

## The access line

`RequestLogger(next, opts...)` reuses a valid inbound `X-Request-ID` or mints a new one. It echoes the id on the response, puts it in the request context and records the status through a `StatusRecorder`. Then it emits one `Info` access line per request. `Logging(opts...)` is the same thing as a `Middleware`.

An option that adds an attribute or a hook changes nothing until you pass it.

| Option | Effect |
| --- | --- |
| `WithLogger(l)` | Log to `l` instead of `slog.Default()` |
| `WithSkipPaths(paths...)` | Write no record for these exact paths |
| `WithSkipFunc(fn)` | Write no record for a request `fn` matches |
| `WithSkipUpgrades(true)` | Write no record for a request that switched protocols |
| `WithClientIP(trusted...)` | Add a `client_ip` attribute resolved by `ClientIP` |
| `WithClientIPFunc(fn)` | Add a `client_ip` attribute resolved by your own function |
| `WithTemplatePathsUnder(prefixes...)` | Record the route template instead of the path under these prefixes |
| `WithPathFunc(fn)` | Record your own value in place of the path |
| `WithMaxLoggedPath(n)` | Cap the recorded path at `n` bytes instead of 512 |
| `WithLogLevel(fn)` | Choose each line's level |
| `ProbeLogLevel(paths...)` | Log probe endpoints at Debug while they succeed |

A skipped request still gets an id minted, echoed and threaded. It gets no access line and no metric hook, because a stream's open-to-close duration beside a synthetic status would mislead.

`WithClientIP` and `WithClientIPFunc` exclude each other, and the last one applied wins. `WithClientIPFunc` suits a trusted proxy set that changes at run time.

### Upgraded connections

`WithSkipUpgrades(true)` suppresses the record of a request whose response actually switched protocols. That is a recorded status 101, or a hijack taken before any status was recorded, the two shapes a completed WebSocket handshake takes.

Use it instead of a skip rule over the upgrade route. A skip rule decides before the handler runs, so it also deletes the records of refused handshakes. With this option, refused handshakes on the same route keep their records. That covers the 400 for a malformed key, the 403 for a cross-origin or Host refusal, the 405 for a non-GET and the 426 for missing upgrade headers.

Suppression removes the whole record, with no line and no metric hook, while the request id is still minted and echoed. Two cases keep their record. A handler that never calls `WriteHeader` sends an implicit 200, so ordinary requests are untouched. A handler that writes an explicit status and then hijacks, such as a `CONNECT` tunnel answering 200, also keeps its record.

The skip options win over this one. `WithSkipPaths` and `WithSkipFunc` act before the handler runs, so this option can only remove a record, never restore one. It takes no status argument. To quiet a noisy route without losing records, lower its level with `WithLogLevel` or `ProbeLogLevel`.

## What the line records

The access line has two attributes an attacker controls, the path and the method. Both are bounded by default, so a megabyte URL cannot buy a megabyte log line.

The path is capped at 512 bytes. An over-cap value keeps at most that many bytes, cut on a UTF-8 rune boundary, and gains a `...(truncated)` marker. A value within the cap is recorded byte for byte.

`WithMaxLoggedPath(n)` replaces the 512-byte cap. A non-positive `n` is ignored, because a configured zero would silently remove the bound. The cap applies to whatever the path policy produced. That can be the raw path, a template, your `WithPathFunc` value or a placeholder. 128 bytes suits a service with a short route table, such as one serving `/healthz`, `/metrics` and one templated route.

The method is capped at 24 bytes, with no option. The longest method in IANA's registry is `UPDATEREDIRECTREF` at 17 characters. A longer method is recorded as the fixed `(overlong)` placeholder rather than a cut token.

Both caps bound the log only. Request size stays the job of `WithMaxHeaderBytes`, and neither cap touches routing, the status or the `Allow` header.

### Route templates for paths that carry a credential

`WithTemplatePathsUnder(prefixes...)` declares URL prefixes whose concrete paths carry a credential, such as a session token. Under those prefixes the access line records the matched route template, `/api/sessions/{id}`, instead of the path.

The template comes from `r.Pattern`, which `http.ServeMux` sets, with the method prefix removed. The router stays the source of truth, so a new subroute logs correctly with no change in your code.

A path under a declared prefix that matched no route records the prefix plus `(unmatched)`. It never records the raw path, because an unrouted request under that prefix still contains the credential. Paths outside every prefix are recorded unchanged. A static mount's pattern is `/`, so templating every path would collapse all assets onto one line.

Pass the prefix the package that owns the routes exports, not a literal you copy.

### Your own path policy

`WithPathFunc(fn)` replaces `r.URL.Path` with `fn`'s return in the access line, in the `WithRecordMetric` path and in the hook-failure messages. Use it for a policy `WithTemplatePathsUnder` cannot express, such as a truncated form or a per-request decision.

`fn` runs when the line is written, after routing, so `r.Pattern` is set. It is empty on an unmatched request, so return your own placeholder for those. Skip rules test the raw path, and skipped requests never call `fn`.

A panicking or empty-returning `fn` records `(path-redaction-failed)`, never the raw path. The request-based metric hooks are not affected by the path policy.

## Log level

`WithLogLevel(fn)` chooses each access line's level from the request and the status. The default is `Info` for every line. A common policy for a polled service is `Debug` for 2xx and 3xx, `Warn` for 4xx and `Error` for 5xx. Skipped paths never call it, and a panicking policy falls back to `Info`.

`ProbeLogLevel(paths...)` is a preset over it for health, readiness and metrics endpoints. On those exact paths, a success logs at `Debug`, a 4xx at `Warn` and a 5xx at `Error`. Every other request stays at `Info`.

Prefer it over skipping probe paths, which also hides a failing probe. Keep skip rules for streams, where one open-to-close line misleads by its shape. The two level options exclude each other, and the last one applied wins.

## Metric hooks

Three hooks receive one call per logged request. They exclude each other, and the last one applied wins. Each fires from the access log's deferred call, so a panicking handler is still recorded. A panicking hook skips that request's metric and leaves the connection alone.

| Option | Receives | Labels |
| --- | --- | --- |
| `WithRecordRouteMetric(fn)` | a `RequestMetric` | bounded by construction. Recommended |
| `WithRecordMetric(fn)` | a `RequestMetric` | the access line's recorded values |
| `WithRecordMetricRequest(fn)` | the request, status and latency | yours to bound |

`RequestMetric` has named fields, `Method`, `Path`, `Status` and `Latency`, so a hook cannot read the method and path in the wrong order. Both struct hooks take the same `RequestMetric`, so moving from one to the other changes only the option name.

`WithRecordRouteMetric` hands the hook the label pair `RouteMetricLabels` derives. The app never sees the raw request through it, so it has no derivation to get wrong. Prefer it over calling `RouteMetricLabels` inside another hook. The path policy options do not reach these labels, because they bound a log line, not a set of label values. A nil `fn` is ignored.

`WithRecordMetric` receives what the access line recorded. Those values are bounded in length but not in count, because a raw path under the cap is still one label value per URL a scanner invents. Use it for metrics only when a path policy already collapses paths onto templates.

`WithRecordMetricRequest` receives the request itself, for a metric that needs something other than the standard pair, such as a per-tenant series keyed on an id the app validated. The app then owns the cardinality bound.

### How the route labels are derived

`RouteMetricLabels(r)` returns `(method, path)`.

The method label is `r.Method` when it is one of the nine standard methods: `GET`, `HEAD`, `POST`, `PUT`, `DELETE`, `CONNECT`, `OPTIONS` and `TRACE` from RFC 9110, plus `PATCH` from RFC 5789. Anything else becomes `other`. That makes ten values whatever arrives, and a lowercase `get` is `other`, because HTTP methods are case-sensitive.

The path label is the route the mux matched. It is `r.Pattern` with the method prefix removed, so `GET /beat/{id}` becomes `/beat/{id}` and an unknown id adds no series. A pattern with no method, such as `/beat/{id}` or a `/` catch-all, is used as it is. A request that matched nothing gets `unmatched`.

So the number of series is at most ten times one more than the number of routes, and no traffic can widen it. That matters because the hook runs outside every auth check in the app, and a series lasts for the life of the process.

The labels read `r.Pattern`, which `http.ServeMux` sets. A registered `/` catch-all matches every request, so in that case nothing is ever `unmatched`. A middleware between the logger and the mux that replaces the request, for example with `r.WithContext`, leaves `r.Pattern` empty for the logger, so every request reads as `unmatched`. Check for that before you trust a flat metric.

One divergence from the access line is deliberate. For a non-standard method, the line records the token, capped at 24 bytes, while the metric records `other`. Match the two by `request_id`.

## StatusRecorder

`StatusRecorder` wraps an `http.ResponseWriter` to capture the response status and stays transparent to streaming. `NewStatusRecorder(w)` starts with status 200.

- `WriteHeader(code)` and `Write(b)` record the first explicit code only. The first `Write` without one records 200.
- `Status()` returns the recorded status. `Wrote()` reports whether the response is committed, which `Recoverer` uses to avoid writing a 500 over a started response.
- `Unwrap()`, `Flush()`, `Hijack()` and `ReadFrom(src)` pass through to the underlying writer and return its results.

`Unwrap` lets `http.NewResponseController` reach the underlying writer's flusher, hijacker and deadline setters. The recorder also implements `http.Flusher`, `http.Hijacker` and `io.ReaderFrom` directly. So a handler that type-asserts those interfaces still works, and `io.Copy` and `http.ServeContent` keep the sendfile fast path.
