# Client IP, hosts and credentials

This page is for a developer deciding what a request may claim about itself. It covers the client IP behind a proxy, the Host allowlist, loopback-only endpoints, classifying a listen address and checking one static credential.

## Client IP

`ClientIP(r, trusted...)` returns the best-effort client IP.

With no trusted ranges, or when the direct peer is outside them, it ignores `X-Forwarded-For` and returns the host of `r.RemoteAddr`. That is the TCP peer, which a client cannot spoof at this layer.

Behind a trusted proxy it reads `X-Forwarded-For` from right to left, skipping each trusted hop, and returns the first untrusted entry. That is the correct reading when a proxy appends the peer it saw, as Caddy and most reverse proxies do. The leftmost entry is whatever the client sent.

`X-Real-IP` is never read, because a client can set it. The library hardcodes no trusted range. You supply them.

The trusted set must hold every proxy hop between the client and the server. If a hop is missing, the walk stops there and returns that hop's address.

`ParseCIDRs(entries)` turns an operator list of CIDRs or bare IPs into that trusted set. A bare IP becomes `/32` or `/128`, and blank entries are skipped. Malformed entries come back in a separate list, so a strict caller can reject them and a lenient one can log them and use the rest.

## Host allowlist

A DNS rebinding attack starts on a page the attacker controls. The page's own hostname is made to resolve to your service's address, so the victim's browser sends requests to your service with the attacker's name in `Host`. The allowlist refuses any request whose `Host` you did not list, which breaks the attack. It is listed as CWE-346.

- `ParseHostList(entries, opts...)` parses an operator allowlist, such as a config array or a comma-split environment variable, into an immutable `*HostPolicy`. Malformed entries come back separately, as with `ParseCIDRs`.
- `(*HostPolicy).Middleware()` answers 403 with the code `host_not_allowed` for a request whose `Host` is not listed. An inactive policy returns the handler unwrapped.
- `(*HostPolicy).Allows(r)` is the same decision for one request. `Active()` and `Size()` describe the policy, for startup logs.
- `WithHostAllowlistError(code, msg)` changes the 403's code and message, for example to name your configuration setting.
- `WithLoopbackExempt(true)` admits a request when both the socket peer and the `Host` are loopback, described below.

Place the middleware before any cross-origin or CSRF check. A rebinding request makes `Origin` and `Host` agree, so a same-origin check alone admits it.

### How matching works

`CanonicalHost(hostport)` is the strict canonicalizer that both the entries and each request's `Host` go through. It lowercases ASCII only, drops the port and the brackets, allows at most one trailing FQDN dot and normalizes IP literals so different spellings of one address compare equal. It returns `""` for anything malformed.

The lowercasing stops at ASCII on purpose. `strings.ToLower` maps U+212A and U+0130 to `k` and `i`, which would let a non-ASCII name canonicalize onto an allowlisted ASCII one. Configure a non-ASCII name as its Punycode A-label, because matching is byte-exact.

Matching is exact and purely textual. There is no name resolution, because resolving would reopen the race the check closes. `X-Forwarded-Host` is ignored, because a client controls it. A malformed `Host` is rejected rather than repaired, because a repair would map distinct wire values onto allowlisted entries.

Activation fails closed. A nil or all-blank entry list leaves the policy inactive, so requests pass through. Any non-blank entry turns the check on, so even a list where every entry is invalid denies every request rather than silently turning protection off.

### The loopback exemption

`WithLoopbackExempt(true)` keeps a container healthcheck or an in-container client working under an allowlist meant for browsers. It admits a request only when the socket peer and the `Host` are both loopback.

A rebinding attack cannot reach it, because the attack's `Host` is not loopback. A remote client cannot forge it, because a remote peer is not loopback. `false` means the same as leaving the option out, and options resolve last-wins, so you can pass a computed flag without branching.

## Loopback-only endpoints

`LoopbackOnly(refuse)` admits a request only when `LoopbackRequest` passes and `ProxiedRequest` finds no forwarding header. Every other request goes to `refuse`. A nil `refuse` answers 403 with the standard error envelope and the code `loopback_only`. Pass your own handler to keep an envelope your clients already depend on.

`LoopbackRequest(r)` reports whether a request is local. The socket peer must be loopback and the `Host` must name the local host, and either check failing refuses. It reads `r.RemoteAddr` and `r.Host` and nothing else, so forwarded headers never admit or refuse a request. The `Host` check accepts every spelling `CanonicalHost` collapses: `localhost` in any case, `127.0.0.0/8` and `::1`, with an optional port or trailing dot.

`ProxiedRequest(h)` reports whether the headers show that a proxy forwarded the request or a browser sent it. The set is `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Real-Ip`, `Sec-Fetch-Site` and `Origin`. A missing header proves nothing. Use it directly when your app has its own admission chain and cannot take the middleware whole.

### Why the middleware also checks forwarding headers

A normal in-container command-line client sends none of those headers. Their presence points to a browser or a proxy, so `LoopbackOnly` refuses the request. Host networking or a shared network namespace puts a reverse proxy on the server's own loopback interface. Both nginx and Apache then rewrite `Host` to their upstream address by default, which passes the `Host` check, and the proxy itself passes the peer check. Without the header check, a remote request would pass both.

The check has a limit. It admits a proxy on the same loopback interface that removes every forwarding header, because nothing then tells it apart from an in-container caller. Only authentication closes that gap.

This is a different question from `HostPolicy`. The allowlist asks whether the `Host` is one the operator named, so it admits a remote client sending `Host: localhost` when the operator lists that name. `LoopbackOnly` asks whether the request came from inside.

## Bind classification

`ClassifyBind(addr)` classifies a configured listen address, given as `host:port`, by exposure. Use it at startup to warn when an unauthenticated service listens beyond loopback.

| Class | Covers |
| --- | --- |
| `BindLoopback` | Loopback IP literals, and `localhost` under an ASCII case fold. `LOCALHOST` matches, a non-ASCII lookalike does not |
| `BindExposed` | Wildcard binds, routable IPs and every other hostname. No name is resolved, so an unresolvable name is exposed |
| `BindInvalid` | Not `host:port`. It is the zero value, so an uninitialized class never reads as safe |

`ClassifyBindHost(host)` classifies a bare host with no port and never returns `BindInvalid`. It is the fallback for a bind value that has no port. `BindClass.String()` returns `invalid`, `loopback` or `exposed` for a log attribute.

What to do with an invalid address is your app's decision. The godoc shows three ways to handle it.

## One static credential

`NewStaticTokenVerifier(configured)` builds a verifier once, at startup, from the single operator-configured secret that guards an endpoint. That can be an API key, a bearer token, or a basic-auth user or password. `Verify(presented)` reports a match in constant time and is safe for concurrent use.

The configured secret is hashed with SHA-256 once, at construction. `Verify` hashes the presented value and compares the two fixed-length digests with `subtle.ConstantTimeCompare`. So no call's timing varies with the secret's length or content.

An empty configured secret fails closed. `Verify` then returns false for every value, the empty string included. Otherwise `sha256("")` would equal `sha256("")`, and an unset secret would admit a client presenting nothing. Treat an empty configured value as "auth not configured", never as "no credential required".

This checks one shared secret, not user identities. Per-user credentials, password hashing and sessions belong to the [auth](https://github.com/cplieger/auth) library.
