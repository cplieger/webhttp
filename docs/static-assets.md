# Static assets and CSP hashes

This page is for a developer serving an embedded web UI from a Go binary. It covers `StaticHandler`, its cache policy, and the helpers that hash inline scripts and styles for a Content-Security-Policy.

## StaticHandler

`StaticHandler(fsys, opts...)` serves a static tree from an `embed.FS` or any other `fs.FS`. An `embed.FS` reports a zero modification time, so a bare `http.FileServer` never revalidates its files.

The handler walks the tree once, when you build it. For each file it computes a SHA-256 content-hash ETag and a gzip copy at the best compression level, and it keeps the gzip copy only when it is smaller. Already-compressed formats such as woff2 and png, and tiny files, therefore stay uncompressed.

It serves a known asset with its ETag, the cache policy's `Cache-Control` and `Vary: Accept-Encoding`. A matching `If-None-Match` gets a 304. A client that accepts gzip on a `GET` or `HEAD` without a `Range` header gets the gzip copy. That copy has its own ETag, the identity tag plus `-gz`, and its own 304 handling. Anything else, including `Range` requests, directories, unknown paths and 404s, falls through to the plain `http.FileServer`.

The error is non-nil only when walking `fsys` fails. A malformed embed is a build problem, so abort startup on it.

## Cache policy

`WithStaticCacheControl(fn)` sets the `Cache-Control` value per asset. `fn` receives the normalized asset path and returns the header value, or an empty string to leave the header out.

The default is `no-cache` for every asset. With a content-hash ETag, revalidation is a cheap 304.

The ETag, gzip and revalidation behaviour is not configurable. Only the cache policy is.

## Inline script hashes

`InlineScriptHashes(html)` returns one CSP source token, `'sha256-<base64>'`, for each inline `<script>` element in a page. It hashes exactly the bytes a browser hashes. A `<script>` with a `src` attribute is skipped, because `'self'` covers it.

It extracts hashes from a page your app controls. It is not an HTML sanitizer.

Put the tokens into your own policy string and pass it with `WithCSP`:

```go
hashes := webhttp.InlineScriptHashes(indexHTML)
if len(hashes) == 0 {
	return errors.New("index.html has no inline script")
}
csp := "default-src 'self'; script-src 'self' " + strings.Join(hashes, " ")
mw := webhttp.SecurityHeaders(webhttp.WithCSP(csp))
```

If your page is known to carry inline scripts, treat an empty result as a malformed build and fail startup. Never fall back to `'unsafe-inline'`.

## Inline style hashes

`InlineStyleHashes(html)` does the same for inline `<style>` elements, so `style-src` can be hash-pinned instead of using `'unsafe-inline'`. The usual case is a loading overlay whose CSS must paint before the external stylesheet arrives.

It shares the script scanner's core, so the two handle byte boundaries and malformed tags the same way. It has no rule to skip external sources, because a `<style>` element always carries its content inline.

A `style-src` hash does not cover inline style attributes, `style="..."`. Those are governed by `style-src-attr` and need `'unsafe-hashes'`. An app whose markup or renderer sets style attributes cannot drop `'unsafe-inline'` on these tokens alone. A renderer that sets styles through CSSOM properties writes no attribute, so it is unaffected.
