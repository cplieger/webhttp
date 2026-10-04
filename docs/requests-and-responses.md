# Requests and responses

This page is for a developer writing handlers on webhttp. It covers the JSON writers, the error envelope, body limits and decoding, method checks and the path `http.ServeMux` will route.

## JSON responses

- `JSONHeaders(w)` sets `Content-Type: application/json` and `X-Content-Type-Options: nosniff`.
- `WriteJSON(w, v)` writes `v` with status 200.
- `WriteJSONStatus(w, code, v)` sets the headers, writes the status and encodes `v`. An encode failure is logged at `Warn` and not returned.
- `Ok(w)` writes 200 with `{"ok":true}`.

## Errors

`WriteError(w, r, status, code, msg)` writes an `ErrorResponse`. It is safe to call with a nil `r`.

`ErrorResponse` has three fields, `Error`, `Code` and `RequestID`, and the last two are left out when empty. The request id comes from the request context, so a client can match a failure to the access log. Every error body the library writes follows that rule.

```json
{"error":"invalid payload","code":"bad_request","request_id":"0f6c..."}
```

`ErrorCode` is the machine-readable token in the `code` field. It is a separate type from the human message, so the two cannot be passed in the wrong order. A code uses only lowercase letters, digits and `_`, as in `host_not_allowed`. The empty code leaves the field out.

A code that breaks that grammar is neither sent nor repaired, and it never panics. The encoder sends `InvalidErrorCode`, `invalid_error_code`, in its place and logs the bad code once per process, because this runs on an error path for every request.

`ErrorResponder` is the signature of `WriteError`: `func(w, r, status, code, msg)`. `WriteError` is its default. Middleware that writes an error body takes one, so an endpoint that does not speak JSON can render its error on its own content type.

## Request bodies

`MaxJSONBody` is the default body cap, 1 MiB.

`LimitBody(w, r, maxBytes)` wraps the body in `http.MaxBytesReader`. A read past the cap fails with a `*http.MaxBytesError`, which you test with `errors.AsType`, and nothing is written, so the status is yours. Detect an over-limit body on the read error, never on the close.

`LimitBody` also asks `net/http` to close the connection instead of reading the rest of an oversized body. It reaches the server's own writer through the `Unwrap` chain, so this is best effort. A middleware that does not unwrap blocks it, and so does `RouteTimeout`, whose buffering writer cannot be unwrapped.

| Function | Cap | On failure |
| --- | --- | --- |
| `DecodeBody(w, r, v, errMsg)` | `MaxJSONBody` | writes 400 with `errMsg` and returns false |
| `DecodeBodyOptional(w, r, v)` | `MaxJSONBody` | ignores the error |
| `DecodeJSONInto(w, r, v, maxBytes)` | `maxBytes` | writes nothing and returns the error |

Each decodes a single JSON value, and `DecodeBody` and `DecodeJSONInto` reject trailing data. Unknown fields are accepted.

`DecodeBodyOptional` ignores two errors a caller often wants. A body holding a value plus trailing data leaves the first value in `v`, where `DecodeBody` would reject it. An oversized body's `*http.MaxBytesError` is ignored too, so it looks the same as an absent body. To tell those apart, use `DecodeJSONInto` and check `errors.Is(err, io.EOF)` for the absent case.

`DecodeJSONInto` is the mechanism behind `DecodeBody`, for an app with its own error envelope or a cap per endpoint. It returns a `*http.MaxBytesError` for an oversized body, `ErrTrailingData` for a second JSON value, and another error for a malformed body. Map the result to your own status and envelope, for example 413 for the size error.

## Methods

- `RequireMethod(w, r, method)` writes a 405 and returns false when `r.Method` differs.
- `MethodNotAllowed(w, r, allowed...)` writes the 405 on its own, for a route that permits several methods. The `Allow` header names the whole set, such as `GET, POST`, and the body is the standard `method_not_allowed` envelope.
- `SetAllow(w, allowed...)` sets only the `Allow` header, for an `OPTIONS` responder or any other place outside a 405.

RFC 9110 makes `Allow` mandatory on a 405, as a comma-separated list. The value is your set joined with `", "`. Entries are kept as written, because a method token is case-sensitive. Blank entries are dropped and exact duplicates collapse. An empty set renders the empty value, which the RFC defines as "this resource allows no methods".

`HEAD` is never implied by `GET`. `http.ServeMux` serves `HEAD` from a `GET` pattern, so a route whose `GET` has a side effect registers `HEAD` separately to reject it and does not advertise it. Pass `http.MethodHead` when the route really serves it.

## Canonical request path

`CanonicalRequestPath(p)` returns the path `http.ServeMux` will route `p` as, and whether `p` already is that path. The cleaning is `path.Clean` with a non-root trailing slash put back, which is `net/http`'s own `cleanPath`.

`ServeMux` cleans the escaped path before it picks a pattern, and answers 307 when the result differs. No registered route can intercept that redirect. A browser follows it without the user noticing.

A machine sender that does not follow redirects sees it differently. To `curl -fsS` without `-L`, a 307 is success, so the caller exits 0 having never reached the handler. Nothing is recorded, no job runs, and nothing says the URL was malformed. A route whose only purpose is a side effect uses this function to refuse the non-canonical spelling itself.

The verdict decides whether to refuse. The cleaned path tells you whether the request cleans into the namespace you guard, as `//beat/api` does.

Pass `r.URL.EscapedPath()` to reproduce the mux's decision exactly. The decoded `r.URL.Path` gives a stricter verdict, because `%2e%2e` decodes to `..`. An encoded dot segment then reads as non-canonical, while the mux draws no redirect for it. Both are legitimate, so choose on purpose.

`canonical` is the verdict of the cleaning step alone. Two other redirects are outside it. The trailing-slash redirect, from `/tree` to `/tree/` when a `/tree/` subtree is registered, fires on an already-canonical path and depends on the route table. A `CONNECT` request is not cleaned at all.

An empty `p` returns `/` and false. A `p` with no leading slash is rooted before cleaning, so it can never be canonical. The route scope, the refusal's status and body, and any metric for the case stay with your app. The function only reads a string.
