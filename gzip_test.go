package webhttp_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/webhttp/v4"
)

// gzipServe runs one GET through mw in front of h, offering the given
// Accept-Encoding lines.
func gzipServe(t *testing.T, mw webhttp.Middleware, h http.HandlerFunc, accept ...string) *httptest.ResponseRecorder {
	t.Helper()
	return gzipServeRequest(mw, h, httptest.NewRequest(http.MethodGet, "/", nil), accept...)
}

func gzipServeRequest(mw webhttp.Middleware, h http.HandlerFunc, req *http.Request, accept ...string) *httptest.ResponseRecorder {
	for _, line := range accept {
		req.Header.Add("Accept-Encoding", line)
	}
	rec := httptest.NewRecorder()
	mw(h).ServeHTTP(rec, req)
	return rec
}

func writeBody(status int, contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("reading the gzip stream: %v", err)
	}
	return out
}

func varies(h http.Header) bool {
	for _, v := range h.Values("Vary") {
		for elem := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(elem), "Accept-Encoding") {
				return true
			}
		}
	}
	return false
}

func assertPlain(t *testing.T, rec *httptest.ResponseRecorder, body []byte) {
	t.Helper()
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want none", ce)
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Errorf("body = %d bytes, want the %d bytes written", rec.Body.Len(), len(body))
	}
}

func assertGzipped(t *testing.T, rec *httptest.ResponseRecorder, body []byte) {
	t.Helper()
	if ce := rec.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", ce)
	}
	if got := gunzip(t, rec.Body.Bytes()); !bytes.Equal(got, body) {
		t.Errorf("decompressed body = %d bytes, want the %d bytes written", len(got), len(body))
	}
}

func TestGzipDelayedCommit(t *testing.T) {
	const jsonType = "application/json"
	small := bytes.Repeat([]byte("s"), 1023)
	large := bytes.Repeat([]byte("l"), 4096)

	t.Run("a body under the threshold goes out as written", func(t *testing.T) {
		rec := gzipServe(t, webhttp.Gzip(), writeBody(http.StatusOK, jsonType, small), "gzip")
		assertPlain(t, rec, small)
		if !varies(rec.Header()) {
			t.Error("a candidate under the threshold carries no Vary: Accept-Encoding")
		}
	})

	t.Run("a body at the threshold is compressed", func(t *testing.T) {
		body := bytes.Repeat([]byte("t"), 1024)
		h := func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", jsonType)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body)
		}
		rec := gzipServe(t, webhttp.Gzip(), h, "gzip")
		assertGzipped(t, rec, body)
		if !varies(rec.Header()) {
			t.Error("a compressed response carries no Vary: Accept-Encoding")
		}
		if cl := rec.Header().Get("Content-Length"); cl != "" {
			t.Errorf("Content-Length = %q, want it dropped from a compressed response", cl)
		}
	})

	t.Run("a body crossing the threshold over many writes is compressed whole", func(t *testing.T) {
		h := func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", jsonType)
			for chunk := range slices.Chunk(large, 100) {
				_, _ = w.Write(chunk)
			}
		}
		assertGzipped(t, gzipServe(t, webhttp.Gzip(), h, "gzip"), large)
	})

	t.Run("a pre-encoded response passes through", func(t *testing.T) {
		h := func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", jsonType)
			w.Header().Set("Content-Encoding", "br")
			_, _ = w.Write(large)
		}
		rec := gzipServe(t, webhttp.Gzip(), h, "gzip")
		if ce := rec.Header().Get("Content-Encoding"); ce != "br" {
			t.Errorf("Content-Encoding = %q, want the handler's br", ce)
		}
		if !bytes.Equal(rec.Body.Bytes(), large) {
			t.Error("a pre-encoded body was changed")
		}
		if varies(rec.Header()) {
			t.Error("a pre-encoded response gained Vary: Accept-Encoding")
		}
	})

	t.Run("a q=0 refusal gets the body as written", func(t *testing.T) {
		rec := gzipServe(t, webhttp.Gzip(), writeBody(http.StatusOK, jsonType, large), "gzip;q=0")
		assertPlain(t, rec, large)
		if !varies(rec.Header()) {
			t.Error("a candidate refused by Accept-Encoding carries no Vary: Accept-Encoding")
		}
	})

	t.Run("a request without Accept-Encoding gets the body as written", func(t *testing.T) {
		assertPlain(t, gzipServe(t, webhttp.Gzip(), writeBody(http.StatusOK, jsonType, large)), large)
	})

	t.Run("an offer on a later field line is read", func(t *testing.T) {
		rec := gzipServe(t, webhttp.Gzip(), writeBody(http.StatusOK, jsonType, large), "identity", "gzip")
		assertGzipped(t, rec, large)
	})

	t.Run("an error status keeps its status and is compressed", func(t *testing.T) {
		rec := gzipServe(t, webhttp.Gzip(), writeBody(http.StatusServiceUnavailable, jsonType, large), "gzip")
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
		assertGzipped(t, rec, large)
	})

	t.Run("a handler that writes nothing gets the implicit 200 with Vary", func(t *testing.T) {
		h := func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("Content-Type", jsonType) }
		rec := gzipServe(t, webhttp.Gzip(), h, "gzip")
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		assertPlain(t, rec, nil)
		if got := rec.Header().Values("Vary"); !slices.Equal(got, []string{"Accept-Encoding"}) {
			t.Errorf("Vary = %q, want [Accept-Encoding]", got)
		}
	})

	statuses := []struct {
		code   int
		varies bool
	}{
		// RFC 9110 sections 15.3.7 and 15.4.5: a 206 and a 304 carry the
		// Vary a 200 to the same request would; a 204 has no representation.
		{http.StatusNoContent, false},
		{http.StatusNotModified, true},
	}
	for _, tc := range statuses {
		t.Run("status "+strconv.Itoa(tc.code)+" carries no Content-Encoding", func(t *testing.T) {
			h := func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", jsonType)
				w.WriteHeader(tc.code)
			}
			rec := gzipServe(t, webhttp.Gzip(), h, "gzip")
			if rec.Code != tc.code {
				t.Errorf("status = %d, want %d", rec.Code, tc.code)
			}
			if ce := rec.Header().Get("Content-Encoding"); ce != "" {
				t.Errorf("Content-Encoding = %q, want none", ce)
			}
			if got := varies(rec.Header()); got != tc.varies {
				t.Errorf("Vary: Accept-Encoding present = %v, want %v", got, tc.varies)
			}
		})
	}

	t.Run("a 206 range passes through with Vary", func(t *testing.T) {
		rec := gzipServe(t, webhttp.Gzip(), writeBody(http.StatusPartialContent, jsonType, large), "gzip")
		assertPlain(t, rec, large)
		if !varies(rec.Header()) {
			t.Error("a 206 of a listed type carries no Vary: Accept-Encoding")
		}
	})

	t.Run("a Vary of * is left alone", func(t *testing.T) {
		h := func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Vary", "*")
			writeBody(http.StatusOK, jsonType, large)(w, nil)
		}
		got := gzipServe(t, webhttp.Gzip(), h, "gzip").Header().Values("Vary")
		if !slices.Equal(got, []string{"*"}) {
			t.Errorf("Vary = %q, want the handler's * alone", got)
		}
	})

	for _, body := range [][]byte{small, large} {
		t.Run("a header set after WriteHeader is not sent, "+strconv.Itoa(len(body))+" bytes", func(t *testing.T) {
			h := func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", jsonType)
				w.Header().Set("X-Before", "kept")
				w.WriteHeader(http.StatusCreated)
				w.Header().Set("X-Late", "dropped")
				w.Header().Del("X-Before")
				_, _ = w.Write(body)
			}
			res := gzipServe(t, webhttp.Gzip(), h, "gzip").Result()
			if res.StatusCode != http.StatusCreated {
				t.Errorf("status = %d, want 201", res.StatusCode)
			}
			if got := res.Header.Get("X-Late"); got != "" {
				t.Errorf("X-Late = %q, want a header set after WriteHeader dropped", got)
			}
			if got := res.Header.Get("X-Before"); got != "kept" {
				t.Errorf("X-Before = %q, want the value it had at WriteHeader", got)
			}
		})
	}

	t.Run("a HEAD request is never compressed", func(t *testing.T) {
		rec := gzipServeRequest(webhttp.Gzip(), writeBody(http.StatusOK, jsonType, large),
			httptest.NewRequest(http.MethodHead, "/", nil), "gzip")
		if ce := rec.Header().Get("Content-Encoding"); ce != "" {
			t.Errorf("Content-Encoding = %q, want none", ce)
		}
		if !varies(rec.Header()) {
			t.Error("a HEAD candidate carries no Vary: Accept-Encoding")
		}
	})

	t.Run("a type outside the list passes through without Vary", func(t *testing.T) {
		rec := gzipServe(t, webhttp.Gzip(), writeBody(http.StatusOK, "text/html", large), "gzip")
		assertPlain(t, rec, large)
		if varies(rec.Header()) {
			t.Error("a response Gzip could never compress gained Vary: Accept-Encoding")
		}
	})

	t.Run("a response without Content-Type passes through", func(t *testing.T) {
		h := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(large) }
		assertPlain(t, gzipServe(t, webhttp.Gzip(), h, "gzip"), large)
	})

	t.Run("WithGzipSkip hands the request over unwrapped", func(t *testing.T) {
		skip := webhttp.Gzip(webhttp.WithGzipSkip(func(r *http.Request) bool { return r.URL.Path == "/" }))
		var unwrapped bool
		h := func(w http.ResponseWriter, r *http.Request) {
			_, unwrapped = w.(*httptest.ResponseRecorder)
			writeBody(http.StatusOK, jsonType, large)(w, r)
		}
		rec := gzipServe(t, skip, h, "gzip")
		if !unwrapped {
			t.Error("a skipped request reached the handler through a wrapper")
		}
		assertPlain(t, rec, large)
		if varies(rec.Header()) {
			t.Error("a skipped request gained Vary: Accept-Encoding")
		}
	})

	t.Run("a compressed response weakens a strong ETag and drops Accept-Ranges", func(t *testing.T) {
		h := func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Accept-Ranges", "bytes")
			writeBody(http.StatusOK, jsonType, large)(w, nil)
		}
		rec := gzipServe(t, webhttp.Gzip(), h, "gzip")
		if etag := rec.Header().Get("ETag"); etag != `W/"v1"` {
			t.Errorf("ETag = %q, want W/\"v1\"", etag)
		}
		if ar := rec.Header().Get("Accept-Ranges"); ar != "" {
			t.Errorf("Accept-Ranges = %q, want it dropped", ar)
		}
	})

	t.Run("a compressed response keeps a weak ETag", func(t *testing.T) {
		h := func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("ETag", `W/"v1"`)
			writeBody(http.StatusOK, jsonType, large)(w, nil)
		}
		if etag := gzipServe(t, webhttp.Gzip(), h, "gzip").Header().Get("ETag"); etag != `W/"v1"` {
			t.Errorf("ETag = %q, want W/\"v1\"", etag)
		}
	})

	t.Run("a response sent as written keeps its ETag and Content-Length", func(t *testing.T) {
		h := func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Length", strconv.Itoa(len(small)))
			writeBody(http.StatusOK, jsonType, small)(w, nil)
		}
		rec := gzipServe(t, webhttp.Gzip(), h, "gzip")
		if etag := rec.Header().Get("ETag"); etag != `"v1"` {
			t.Errorf("ETag = %q, want \"v1\"", etag)
		}
		if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(small)) {
			t.Errorf("Content-Length = %q, want %d", cl, len(small))
		}
	})

	t.Run("Vary is added once beside the handler's own", func(t *testing.T) {
		h := func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Add("Vary", "Origin, accept-encoding")
			writeBody(http.StatusOK, jsonType, large)(w, nil)
		}
		got := gzipServe(t, webhttp.Gzip(), h, "gzip").Header().Values("Vary")
		if !slices.Equal(got, []string{"Origin, accept-encoding"}) {
			t.Errorf("Vary = %q, want the handler's value alone", got)
		}
	})

	t.Run("the first status wins", func(t *testing.T) {
		h := func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", jsonType)
			w.WriteHeader(http.StatusCreated)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write(large)
		}
		if rec := gzipServe(t, webhttp.Gzip(), h, "gzip"); rec.Code != http.StatusCreated {
			t.Errorf("status = %d, want 201", rec.Code)
		}
	})

	t.Run("WithGzipMinBytes moves the threshold", func(t *testing.T) {
		mw := webhttp.Gzip(webhttp.WithGzipMinBytes(10))
		assertPlain(t, gzipServe(t, mw, writeBody(http.StatusOK, jsonType, large[:9]), "gzip"), large[:9])
		assertGzipped(t, gzipServe(t, mw, writeBody(http.StatusOK, jsonType, large[:10]), "gzip"), large[:10])
	})

	t.Run("a threshold below one compresses any non-empty body", func(t *testing.T) {
		mw := webhttp.Gzip(webhttp.WithGzipMinBytes(-5))
		assertPlain(t, gzipServe(t, mw, writeBody(http.StatusOK, jsonType, nil), "gzip"), nil)
		assertGzipped(t, gzipServe(t, mw, writeBody(http.StatusOK, jsonType, large[:1]), "gzip"), large[:1])
	})
}

func TestGzipTypes(t *testing.T) {
	body := bytes.Repeat([]byte("j"), 2048)
	cases := []struct {
		contentType string
		compressed  bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"APPLICATION/JSON", true},
		{"application/problem+json", true},
		{"application/vnd.example+v2+json", true},
		{"application/x-ndjson", false},
		{"application/jsonl", false},
		{"application/json+xml", false},
		{"text/json", false},
		{"text/html", false},
		{"", false},
		{"not a media type", false},
	}
	middlewares := []struct {
		name string
		mw   webhttp.Middleware
	}{
		{"explicit", webhttp.Gzip(webhttp.WithGzipTypes("application/json", "+json"))},
		{"default", webhttp.Gzip()},
	}
	for _, m := range middlewares {
		for _, tc := range cases {
			t.Run(m.name+" "+tc.contentType, func(t *testing.T) {
				rec := gzipServe(t, m.mw, writeBody(http.StatusOK, tc.contentType, body), "gzip")
				if tc.compressed {
					assertGzipped(t, rec, body)
				} else {
					assertPlain(t, rec, body)
				}
			})
		}
	}

	t.Run("entries fold case", func(t *testing.T) {
		mw := webhttp.Gzip(webhttp.WithGzipTypes("Text/CSV", "+XML"))
		assertGzipped(t, gzipServe(t, mw, writeBody(http.StatusOK, "text/csv", body), "gzip"), body)
		assertGzipped(t, gzipServe(t, mw, writeBody(http.StatusOK, "image/svg+xml", body), "gzip"), body)
	})

	t.Run("the list replaces the default", func(t *testing.T) {
		mw := webhttp.Gzip(webhttp.WithGzipTypes("text/csv"))
		assertPlain(t, gzipServe(t, mw, writeBody(http.StatusOK, "application/json", body), "gzip"), body)
	})

	t.Run("an empty list compresses nothing", func(t *testing.T) {
		mw := webhttp.Gzip(webhttp.WithGzipTypes())
		rec := gzipServe(t, mw, writeBody(http.StatusOK, "application/json", body), "gzip")
		assertPlain(t, rec, body)
		if varies(rec.Header()) {
			t.Error("a response no entry names gained Vary: Accept-Encoding")
		}
	})

	for _, entry := range []string{"", "json", "+", "text/*", "*/*", "+a+json", "+js*n", "application/json; charset=utf-8", "/json", "application/"} {
		t.Run("entry "+strconv.Quote(entry)+" panics", func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("WithGzipTypes(%q) did not panic", entry)
				}
			}()
			webhttp.WithGzipTypes(entry)
		})
	}
}

// lockedBuffer collects a server's error log, which net/http writes from the
// connection's goroutine.
type lockedBuffer struct {
	b  bytes.Buffer
	mu sync.Mutex
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// gzipServer serves h on a loopback listener and returns its URL and its
// error log, read after the server has closed.
func gzipServer(t *testing.T, h http.Handler) (url string, errorLog func() string) {
	t.Helper()
	logs := &lockedBuffer{}
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = log.New(logs, "", 0)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL, func() string {
		srv.Close()
		return logs.String()
	}
}

// gzipGet sends a GET offering gzip; setting Accept-Encoding by hand keeps
// the transport from decompressing. The caller closes the body.
func gzipGet(t *testing.T, url string, trace *httptrace.ClientTrace) *http.Response {
	t.Helper()
	ctx := t.Context()
	if trace != nil {
		ctx = httptrace.WithClientTrace(ctx, trace)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	return resp
}

func TestGzipPanicOrdering(t *testing.T) {
	recoverer := webhttp.Recoverer(webhttp.WithRecoverLogger(discardLogger()))

	t.Run("a panic after a held-back write answers 500 with no gzip stream", func(t *testing.T) {
		h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write(bytes.Repeat([]byte("p"), 100))
			panic("handler failed")
		})
		url, _ := gzipServer(t, webhttp.Chain(h, recoverer, webhttp.Gzip()))
		resp := gzipGet(t, url, nil)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", resp.StatusCode)
		}
		if ce := resp.Header.Get("Content-Encoding"); ce != "" {
			t.Errorf("Content-Encoding = %q, want none", ce)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading the 500: %v", err)
		}
		if !bytes.Contains(body, []byte(`"code":"internal_error"`)) || bytes.Contains(body, []byte("ppp")) {
			t.Errorf("body = %q, want Recoverer's envelope alone", body)
		}
	})

	t.Run("a panic after compression began leaves the stream truncated", func(t *testing.T) {
		h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(bytes.Repeat([]byte("p"), 4096))
			panic("handler failed")
		})
		url, _ := gzipServer(t, webhttp.Chain(h, recoverer, webhttp.Gzip()))
		resp := gzipGet(t, url, nil)
		defer func() { _ = resp.Body.Close() }()
		if ce := resp.Header.Get("Content-Encoding"); ce != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", ce)
		}
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			t.Fatalf("gzip.NewReader: %v", err)
		}
		if _, err := io.ReadAll(zr); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("reading the stream: err = %v, want io.ErrUnexpectedEOF (a sealed stream hides the failure)", err)
		}
	})
}

func TestGzipStreaming(t *testing.T) {
	// waitFor bounds a step that must finish while the handler is blocked.
	waitFor := func(t *testing.T, what string, step func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- step() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: nothing reached the client while the handler was still running", what)
		}
	}

	t.Run("a flushed small body reaches the client before the handler returns", func(t *testing.T) {
		release := make(chan struct{})
		chunk := []byte(`{"progress":1}`)
		h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(chunk)
			w.(http.Flusher).Flush()
			<-release
		})
		url, _ := gzipServer(t, webhttp.Gzip()(h))
		resp := gzipGet(t, url, nil)
		defer func() { _ = resp.Body.Close() }()
		defer close(release)
		if ce := resp.Header.Get("Content-Encoding"); ce != "" {
			t.Errorf("Content-Encoding = %q, want none for a flushed small body", ce)
		}
		got := make([]byte, len(chunk))
		waitFor(t, "reading the flushed body", func() error {
			_, err := io.ReadFull(resp.Body, got)
			return err
		})
		if !bytes.Equal(got, chunk) {
			t.Errorf("body = %q, want %q", got, chunk)
		}
	})

	t.Run("a flushed compressed stream reaches the client before the handler returns", func(t *testing.T) {
		release := make(chan struct{})
		body := bytes.Repeat([]byte("f"), 4096)
		h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("ResponseController.Flush: %v", err)
			}
			<-release
		})
		url, _ := gzipServer(t, webhttp.Gzip()(h))
		resp := gzipGet(t, url, nil)
		defer func() { _ = resp.Body.Close() }()
		defer close(release)
		if ce := resp.Header.Get("Content-Encoding"); ce != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", ce)
		}
		got := make([]byte, len(body))
		waitFor(t, "reading the flushed stream", func() error {
			zr, err := gzip.NewReader(resp.Body)
			if err != nil {
				return err
			}
			_, err = io.ReadFull(zr, got)
			return err
		})
		if !bytes.Equal(got, body) {
			t.Error("the flushed stream does not decompress to the body written")
		}
	})

	t.Run("Hijack takes the connection over and drops the held-back body", func(t *testing.T) {
		h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"held":true}`))
			conn, brw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("Hijack: %v", err)
				return
			}
			defer conn.Close()
			_, _ = brw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi")
			_ = brw.Flush()
		})
		// The server does not wait for a hijacked connection's handler, so
		// the test does before reading the log.
		served := make(chan struct{})
		gz := webhttp.Gzip()(h)
		url, errorLog := gzipServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(served)
			gz.ServeHTTP(w, r)
		}))
		resp := gzipGet(t, url, nil)
		defer func() { _ = resp.Body.Close() }()
		got, err := io.ReadAll(resp.Body)
		if err != nil || string(got) != "hi" {
			t.Errorf("body = %q, %v; want the hijacker's \"hi\"", got, err)
		}
		waitFor(t, "the handler returning", func() error {
			<-served
			return nil
		})
		if logged := errorLog(); logged != "" {
			t.Errorf("the server logged %q, want nothing written after the hijack", logged)
		}
	})

	t.Run("ResponseController reaches the connection through Unwrap", func(t *testing.T) {
		h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			rc := http.NewResponseController(w)
			if err := rc.SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
				t.Errorf("SetWriteDeadline: %v", err)
			}
			if err := rc.EnableFullDuplex(); err != nil {
				t.Errorf("EnableFullDuplex: %v", err)
			}
		})
		url, _ := gzipServer(t, webhttp.Gzip()(h))
		resp := gzipGet(t, url, nil)
		_ = resp.Body.Close()
	})
}

func TestGzipTrailers(t *testing.T) {
	for _, size := range []int{10, 4096} {
		t.Run(strconv.Itoa(size)+" bytes", func(t *testing.T) {
			body := bytes.Repeat([]byte("t"), size)
			h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Trailer", "x-checksum")
				w.WriteHeader(http.StatusCreated)
				w.Header().Set("X-Checksum", "declared")
				w.Header().Set(http.TrailerPrefix+"X-Extra", "prefixed")
				w.Header().Set("X-Late", "dropped")
				_, _ = w.Write(body)
			})
			url, _ := gzipServer(t, webhttp.Gzip()(h))
			resp := gzipGet(t, url, nil)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusCreated {
				t.Errorf("status = %d, want 201", resp.StatusCode)
			}
			var r io.Reader = resp.Body
			if resp.Header.Get("Content-Encoding") == "gzip" {
				zr, err := gzip.NewReader(resp.Body)
				if err != nil {
					t.Fatalf("gzip.NewReader: %v", err)
				}
				r = zr
			}
			if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, body) {
				t.Fatalf("body = %d bytes, %v; want the %d written", len(got), err, len(body))
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			for name, want := range map[string]string{"X-Checksum": "declared", "X-Extra": "prefixed"} {
				if got := resp.Trailer.Get(name); got != want {
					t.Errorf("trailer %s = %q, want %q", name, got, want)
				}
			}
			for _, name := range []string{"X-Checksum", "X-Late"} {
				if got := resp.Header.Get(name); got != "" {
					t.Errorf("header %s = %q, want it absent from the header block", name, got)
				}
			}
		})
	}
}

func TestGzipPassesEarlyHints(t *testing.T) {
	body := bytes.Repeat([]byte("e"), 4096)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", "</app.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	url, _ := gzipServer(t, webhttp.Gzip()(h))
	var hints []int
	resp := gzipGet(t, url, &httptrace.ClientTrace{
		Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
			hints = append(hints, code)
			return nil
		},
	})
	defer func() { _ = resp.Body.Close() }()
	if !slices.Equal(hints, []int{http.StatusEarlyHints}) {
		t.Errorf("1xx responses = %v, want [103]", hints)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("final response = %d with Content-Encoding %q, want a compressed 200",
			resp.StatusCode, resp.Header.Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	if got, err := io.ReadAll(zr); err != nil || !bytes.Equal(got, body) {
		t.Errorf("decompressed body = %d bytes, %v; want the %d bytes written", len(got), err, len(body))
	}
}
