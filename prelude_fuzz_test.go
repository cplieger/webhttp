package webhttp_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/webhttp/v2"
)

// decodeSeeds is the shared adversarial corpus for the two prelude fuzz
// targets: the untrusted-body shapes that DecodeBody / DecodeBodyOptional
// receive off the network. It covers empty, whitespace-only, valid
// scalar/object/array values, a value with accepted trailing whitespace,
// truncated JSON, two back-to-back values, trailing junk, deep nesting,
// invalid UTF-8 inside a string, and a large-but-under-cap value.
var decodeSeeds = [][]byte{
	[]byte(""),
	[]byte("   "),
	[]byte("\n\t "),
	[]byte(`{"name":"neo"}`),
	[]byte(`{"a":1}   `), // one value + accepted trailing whitespace
	[]byte(`[1,2,3]`),
	[]byte("123"),
	[]byte("true"),
	[]byte("null"),
	[]byte(`"str"`),
	[]byte(`{"a":`), // truncated object
	[]byte("{not json"),
	[]byte(`{"a":1}{"b":2}`), // two values back-to-back
	[]byte("1 2"),            // two scalars
	[]byte(`{"a":1} junk`),   // trailing junk after a complete value
	[]byte("[[[[[[[[[[]]]]]]]]]]"),
	{0x7b, 0x22, 0x6b, 0x22, 0x3a, 0x22, 0xff, 0xfe, 0x22, 0x7d}, // {"k":"\xff\xfe"}
	append(append([]byte(`{"a":"`), bytes.Repeat([]byte("x"), 4096)...), []byte(`"}`)...),
}

// countingBody is an io.ReadCloser wrapper that records how many bytes were
// actually pulled from the underlying reader. DecodeBody/DecodeBodyOptional
// wrap the request body in an http.MaxBytesReader (via LimitBody), which caps
// underlying reads at MaxJSONBody (+1 to detect overflow); counting the pulled
// bytes proves the cap bounds reads for ANY input rather than only checking the
// helpers reject one hand-built oversized payload.
type countingBody struct {
	r *bytes.Reader
	n int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingBody) Close() error { return nil }

// FuzzDecodeBody drives the REAL DecodeBody over arbitrary request-body bytes
// and asserts, for every input:
//
//  1. It never panics (fuzz-harness invariant).
//  2. Reads stay bounded by the MaxJSONBody cap: the underlying body is never
//     read past MaxJSONBody+1 bytes, so a huge/streaming body cannot cause an
//     unbounded read (the security half of "cap enforced"). The concrete
//     oversized-body -> 400 rejection is pinned by TestDecodeBody_tooLarge;
//     this target proves the read bound holds for arbitrary content.
//  3. Accept/reject matches the standard-library whole-input decode: DecodeBody
//     returns true iff json.Unmarshal accepts the same bytes as exactly one
//     JSON value plus optional trailing whitespace (gated to inputs within the
//     cap, where DecodeBody's MaxBytesReader never fires and its
//     Decode-then-trailing-EOF sequence is exactly json.Unmarshal's contract).
//     json.Unmarshal is a DIFFERENT encoding/json entry point, so this is an
//     oracle cross-check, not a reimplementation of DecodeBody's own logic.
//  4. Success writes no response body; failure writes a 400 "bad_request"
//     ErrorResponse carrying the exact errMsg the caller passed.
func FuzzDecodeBody(f *testing.F) {
	for _, s := range decodeSeeds {
		f.Add(s)
	}
	const wantMsg = "decode failed"
	f.Fuzz(func(t *testing.T, body []byte) {
		cb := &countingBody{r: bytes.NewReader(body)}
		req := httptest.NewRequest(http.MethodPost, "/", cb)
		rr := httptest.NewRecorder()

		var into any
		ok := webhttp.DecodeBody(rr, req, &into, wantMsg)

		if cb.n > webhttp.MaxJSONBody+1 {
			t.Fatalf("DecodeBody read %d bytes, exceeds MaxJSONBody+1 (%d)", cb.n, webhttp.MaxJSONBody+1)
		}

		// Oracle: within the cap, DecodeBody's accept decision must equal the
		// stdlib whole-input decode (one value + optional trailing whitespace).
		if int64(len(body)) <= webhttp.MaxJSONBody {
			oracleOK := json.Unmarshal(body, new(any)) == nil
			if ok != oracleOK {
				t.Fatalf("DecodeBody=%v but json.Unmarshal(one-value)=%v (body=%q)", ok, oracleOK, body)
			}
		}

		if ok {
			if rr.Body.Len() != 0 {
				t.Fatalf("DecodeBody succeeded but wrote a response body: %q (body=%q)", rr.Body.String(), body)
			}
			return
		}
		// Failure path: exactly a 400 bad_request envelope carrying errMsg.
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("DecodeBody failure status = %d, want 400 (body=%q)", rr.Code, body)
		}
		var er webhttp.ErrorResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &er); err != nil {
			t.Fatalf("DecodeBody failure envelope is not JSON: %v (raw=%q)", err, rr.Body.String())
		}
		if er.Code != "bad_request" || er.Error != wantMsg {
			t.Fatalf("DecodeBody failure envelope = %+v, want code=bad_request error=%q (body=%q)", er, wantMsg, body)
		}
	})
}

// FuzzSetAllow drives the REAL SetAllow over arbitrary method sets (the input
// string is split on NUL into entries, so the corpus covers 1..n entries
// including empty ones) and asserts, for every input:
//
//  1. It never panics, and always sets exactly one Allow field — never two,
//     and never none, since RFC 9110 makes the header mandatory on the 405
//     MethodNotAllowed builds on it.
//  2. A single entry renders VERBATIM. This is the compatibility invariant for
//     every existing RequireMethod caller: whatever byte sequence a
//     single-method call passed before the list rendering existed, it still
//     emits.
//  3. Every non-empty entry survives into the field value.
//  4. For entries that are real method tokens (the documented contract, RFC
//     9110 9.1), a RECIPIENT's parse of the field — split on "," and strip OWS,
//     per 5.6.1.2 — recovers exactly the deduplicated entry list in order, with
//     no empty list element (5.6.1.1 forbids generating one). Parsing is a
//     different operation from rendering, so this is an oracle cross-check
//     rather than a restatement of the encoder.
//  5. Rendering is idempotent: feeding a parsed field value back through
//     SetAllow reproduces it byte for byte.
func FuzzSetAllow(f *testing.F) {
	seeds := []string{
		"",
		"GET",
		"POST",
		"get",
		"GET\x00POST",
		"POST\x00GET",
		"GET\x00HEAD\x00POST",
		"GET\x00GET",       // exact duplicate
		"GET\x00",          // trailing empty entry
		"\x00GET",          // leading empty entry
		"\x00\x00",         // only empty entries
		"GET, POST",        // one entry that already looks like a list
		"GET\x00 ",         // whitespace-only entry
		"\x00",             // two empty entries
		"OPTIONS\x00TRACE", // uncommon but valid tokens
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, joined string) {
		entries := strings.Split(joined, "\x00")

		rr := httptest.NewRecorder()
		webhttp.SetAllow(rr, entries...)
		values, ok := rr.Header()["Allow"]
		if !ok {
			t.Fatalf("SetAllow set no Allow header (entries=%q)", entries)
		}
		if len(values) != 1 {
			t.Fatalf("SetAllow set %d Allow fields, want exactly 1 (entries=%q)", len(values), entries)
		}
		got := values[0]

		if len(entries) == 1 && got != entries[0] {
			t.Fatalf("single-entry Allow = %q, want the entry verbatim %q", got, entries[0])
		}
		for _, e := range entries {
			if e != "" && !strings.Contains(got, e) {
				t.Fatalf("Allow = %q dropped non-empty entry %q (entries=%q)", got, e, entries)
			}
		}

		// The recipient-parse oracle only applies to inputs that are method
		// tokens; a non-token entry (one containing a comma or space) is
		// garbage in, emitted verbatim, and cannot survive a list parse.
		if !allTokensOrEmpty(entries) {
			return
		}
		want := dedupeNonEmpty(entries)
		parsed := parseAllow(got)
		if !slices.Equal(parsed, want) {
			t.Fatalf("parse(Allow %q) = %q, want %q (entries=%q)", got, parsed, want, entries)
		}
		for _, p := range parsed {
			if p == "" {
				t.Fatalf("Allow = %q contains an empty list element (entries=%q)", got, entries)
			}
		}

		// Re-rendering a parsed value must reproduce it exactly.
		again := httptest.NewRecorder()
		webhttp.SetAllow(again, parsed...)
		if reRendered := again.Header().Get("Allow"); reRendered != got {
			t.Fatalf("SetAllow(parse(%q)) = %q, want the same value", got, reRendered)
		}
	})
}

// parseAllow reads an Allow field value the way a recipient must (RFC 9110
// 5.6.1.2): comma-separated elements with optional surrounding whitespace. An
// entirely empty value means "no methods allowed" and yields no elements.
func parseAllow(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.Trim(p, " \t"))
	}
	return out
}

// dedupeNonEmpty is the expected element set of a rendered Allow field: the
// non-empty entries in caller order, exact duplicates collapsed.
func dedupeNonEmpty(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e != "" && !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// allTokensOrEmpty reports whether every entry is either empty or a valid HTTP
// method token (RFC 9110 5.6.2 tchar), the documented input contract under
// which the field value is parseable as a list.
func allTokensOrEmpty(entries []string) bool {
	const tchar = "!#$%&'*+-.^_`|~"
	for _, e := range entries {
		for _, c := range e {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			case strings.ContainsRune(tchar, c):
			default:
				return false
			}
		}
	}
	return true
}

// FuzzDecodeBodyOptional drives the REAL DecodeBodyOptional over arbitrary
// request-body bytes and asserts, for every input:
//
//  1. It never panics.
//  2. It NEVER writes a response and never sets a non-200 status, regardless of
//     how malformed the body is: an empty body, invalid JSON, trailing junk, or
//     two values are all swallowed silently (the optional-error-swallow
//     contract, and specifically the empty-body-swallow that DecodeBody instead
//     rejects with a 400).
//  3. Reads stay bounded by the MaxJSONBody cap (same MaxBytesReader guard as
//     DecodeBody), so an optional decode cannot cause an unbounded read either.
//  4. Cross-consistency with strict DecodeBody: whenever DecodeBody accepts the
//     same bytes as exactly one value, DecodeBodyOptional must have decoded that
//     identical value. (When DecodeBody rejects, Optional may still have decoded
//     a leading value and MUST NOT have written anything -- covered by 2.)
func FuzzDecodeBodyOptional(f *testing.F) {
	for _, s := range decodeSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		cbOpt := &countingBody{r: bytes.NewReader(body)}
		reqOpt := httptest.NewRequest(http.MethodPost, "/", cbOpt)
		rrOpt := httptest.NewRecorder()

		var optInto any
		webhttp.DecodeBodyOptional(rrOpt, reqOpt, &optInto)

		if rrOpt.Body.Len() != 0 {
			t.Fatalf("DecodeBodyOptional wrote a response body: %q (body=%q)", rrOpt.Body.String(), body)
		}
		if rrOpt.Code != http.StatusOK { // recorder default; a WriteError would move it to 400
			t.Fatalf("DecodeBodyOptional set status %d, must not write a response (body=%q)", rrOpt.Code, body)
		}
		if cbOpt.n > webhttp.MaxJSONBody+1 {
			t.Fatalf("DecodeBodyOptional read %d bytes, exceeds MaxJSONBody+1 (%d)", cbOpt.n, webhttp.MaxJSONBody+1)
		}

		// When strict DecodeBody accepts these exact bytes, Optional (which
		// decodes the same leading value) must have produced the same value.
		cbDec := &countingBody{r: bytes.NewReader(body)}
		reqDec := httptest.NewRequest(http.MethodPost, "/", cbDec)
		rrDec := httptest.NewRecorder()
		var decInto any
		if webhttp.DecodeBody(rrDec, reqDec, &decInto, "x") {
			if !reflect.DeepEqual(optInto, decInto) {
				t.Fatalf("DecodeBody accepted but Optional decoded a different value: opt=%#v dec=%#v (body=%q)", optInto, decInto, body)
			}
		}
	})
}

// FuzzLimitBodyWriterChain fuzzes the ResponseWriter chain LimitBody must walk
// (to reach net/http's own writer with the too-large signal) together with the
// cap and the body length. The chain shape is untrusted input in the sense that
// matters here: it is assembled from whatever middleware — first-party,
// third-party, or buggy — a consumer composed, and each byte of `shape`
// contributes one wrapper:
//
//	0: a wrapper exposing the inner writer via Unwrap (StatusRecorder's shape)
//	1: a wrapper with no Unwrap at all (opaque third-party middleware)
//	2: a wrapper whose Unwrap returns ITSELF (an unterminated chain)
//	3: a wrapper whose Unwrap returns nil
//
// The invariants hold for every chain, which is the point: the walk is a
// best-effort reach for an optional signal and must never change the guarantee
// callers actually rely on.
//
//  1. It terminates and never panics — no chain, however degenerate or deep,
//     can spin the walk or hand a nil writer onward.
//  2. Reads never exceed the cap: at most maxBytes bytes are delivered.
//  3. The error class is decided by size alone, never by the chain: a body over
//     the cap always fails with *http.MaxBytesError, one within it never does.
func FuzzLimitBodyWriterChain(f *testing.F) {
	f.Add([]byte{}, int64(8), 128)
	f.Add([]byte{0}, int64(8), 128)                     // one unwrappable wrapper
	f.Add([]byte{1}, int64(8), 128)                     // one opaque wrapper
	f.Add([]byte{2}, int64(8), 128)                     // self-referential Unwrap
	f.Add([]byte{3}, int64(8), 128)                     // nil Unwrap
	f.Add([]byte{0, 0, 1, 0, 2, 3}, int64(1), 4096)     // mixed chain
	f.Add(bytes.Repeat([]byte{0}, 64), int64(16), 4096) // deeper than the walk bound
	f.Add([]byte{0, 1}, int64(0), 1)                    // zero cap
	f.Add([]byte{0}, int64(-1), 1)                      // negative cap (treated as 0)
	f.Add([]byte{0}, int64(64), 64)                     // exactly at the cap
	f.Add(bytes.Repeat([]byte{2}, 32), int64(4), 1<<10) // nothing but cycles
	f.Fuzz(func(t *testing.T, shape []byte, maxBytes int64, bodyLen int) {
		// Keep the case cheap and meaningful: a body big enough to cross any cap
		// under test, a chain deep enough to exceed the walk bound.
		if bodyLen < 0 || bodyLen > 1<<16 || len(shape) > 128 {
			t.Skip()
		}
		var w http.ResponseWriter = httptest.NewRecorder()
		for _, kind := range shape {
			switch kind % 4 {
			case 0:
				w = &unwrappableWriter{ResponseWriter: w}
			case 1:
				w = &opaqueWriter{ResponseWriter: w}
			case 2:
				w = &selfUnwrappingWriter{ResponseWriter: w}
			default:
				w = &nilUnwrappingWriter{ResponseWriter: w}
			}
		}

		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(bytes.Repeat([]byte("z"), bodyLen)))
		webhttp.LimitBody(w, req, maxBytes) // must return for every chain
		n, err := io.Copy(io.Discard, req.Body)

		effectiveCap := max(maxBytes, 0) // MaxBytesReader treats a negative cap as 0
		if n > effectiveCap {
			t.Fatalf("read %d bytes past the %d-byte cap (shape=%v)", n, effectiveCap, shape)
		}
		tooLarge, gotTooLarge := errors.AsType[*http.MaxBytesError](err)
		wantTooLarge := int64(bodyLen) > effectiveCap
		if gotTooLarge != wantTooLarge {
			t.Fatalf("MaxBytesError=%v (err=%v) for a %d-byte body under a %d-byte cap, want %v (shape=%v)",
				gotTooLarge, err, bodyLen, effectiveCap, wantTooLarge, shape)
		}
		if gotTooLarge && tooLarge.Limit != effectiveCap {
			t.Fatalf("MaxBytesError.Limit = %d, want %d (shape=%v)", tooLarge.Limit, effectiveCap, shape)
		}
		if !gotTooLarge && err != nil {
			t.Fatalf("read err = %v (%T), want nil within the cap (shape=%v)", err, err, shape)
		}
		if !gotTooLarge && n != int64(bodyLen) {
			t.Fatalf("read %d of %d bytes within the cap (shape=%v)", n, bodyLen, shape)
		}
	})
}
