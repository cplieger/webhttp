package webhttp_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/cplieger/webhttp"
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
