package webhttp_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/cplieger/webhttp"
)

func ExampleValidRequestID() {
	fmt.Println(webhttp.ValidRequestID("abc-123_XYZ"))
	fmt.Println(webhttp.ValidRequestID("bad id"))
	// Output:
	// true
	// false
}

func ExampleOk() {
	rr := httptest.NewRecorder()
	webhttp.Ok(rr)
	fmt.Print(rr.Body.String())
	// Output:
	// {"ok":true}
}

func ExampleWriteError() {
	rr := httptest.NewRecorder()
	// r may be nil; the request_id field is then omitted.
	webhttp.WriteError(rr, nil, http.StatusBadRequest, "bad_request", "invalid payload")
	fmt.Print(rr.Body.String())
	// Output:
	// {"error":"invalid payload","code":"bad_request"}
}

func ExampleNewStaticTokenVerifier() {
	v := webhttp.NewStaticTokenVerifier("s3cr3t")
	fmt.Println(v.Verify("s3cr3t"))
	fmt.Println(v.Verify("wrong"))
	// An unset secret fails closed: even an empty presented value is rejected.
	unset := webhttp.NewStaticTokenVerifier("")
	fmt.Println(unset.Verify(""))
	// Output:
	// true
	// false
	// false
}
