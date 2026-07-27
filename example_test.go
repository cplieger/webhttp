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

func ExampleMethodNotAllowed() {
	rr := httptest.NewRecorder()
	// A route serving GET and POST refuses everything else, and RFC 9110
	// requires the 405 to advertise BOTH permitted methods, not just one.
	webhttp.MethodNotAllowed(rr, nil, http.MethodGet, http.MethodPost)
	fmt.Println(rr.Code)
	fmt.Println(rr.Header().Get("Allow"))
	fmt.Print(rr.Body.String())
	// Output:
	// 405
	// GET, POST
	// {"error":"method not allowed","code":"method_not_allowed"}
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
