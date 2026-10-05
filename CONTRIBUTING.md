# Contributing to webhttp

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, dependencies and checks apply here.

## Rules

- An admission check on an ASCII token, such as a `Host` authority, folds case with `equalASCIIFold` or `lowerASCIIString`. `strings.EqualFold` and `strings.ToLower` map U+212A, the Kelvin sign, to `k`, so a crafted name passes an allowlist.
- Every JSON error body the library writes goes through `errorEnvelope`, by way of `WriteError` or `errorBodyJSON`. Any other body skips the error-code check and drops the request id.
- Every option loop, and `Chain`, skips a nil entry, and a nil function handed to an option is never called. Callers build option lists conditionally, so a new option that panics on nil breaks them.
- A middleware set by a number or a list, such as `RateLimiter`, `RouteTimeout` or `HostPolicy`, is off at zero or less or an empty list. A logging bound such as `WithMaxLoggedPath` ignores zero or less instead, because a configured zero would silently remove it.
- A new parser or validator of request, configuration or HTML input gets a fuzz target in a `*_fuzz_test.go` file, as `CanonicalHost`, `ClassifyBind` and `InlineScriptHashes` have. Its output feeds a security decision, and unit cases miss the inputs nobody thought of.
- A test that calls `swapDefaultLogger` does not call `t.Parallel()`. The default logger is process-wide, so parallel tests capture each other's lines. Where the API takes a logger, such as `WithLogger`, pass one instead.
