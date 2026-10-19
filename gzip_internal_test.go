package webhttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"no header", nil, false},
		{"empty header", []string{""}, false},
		{"plain gzip", []string{"gzip"}, true},
		{"coding names fold case", []string{"GZIP"}, true},
		{"gzip among others", []string{"gzip, deflate, br"}, true},
		{"gzip with fractional q after another coding", []string{"br, gzip;q=0.5"}, true},
		{"gzip with q=1", []string{"gzip;q=1.0"}, true},
		{"gzip with q=1.000", []string{"gzip;q=1.000"}, true},
		{"gzip with the smallest non-zero q", []string{"gzip;q=0.001"}, true},
		{"gzip explicitly disabled q=0", []string{"gzip;q=0"}, false},
		{"gzip disabled q=0.0", []string{"gzip;q=0.0"}, false},
		{"gzip disabled q=0.", []string{"gzip;q=0."}, false},
		{"gzip disabled q=0.000", []string{"gzip;q=0.000"}, false},
		{"gzip disabled with an upper-case Q", []string{"gzip;Q=0"}, false},
		{"gzip with fractional q", []string{"gzip;q=0.5"}, true},
		{"gzip with space before params", []string{"gzip ; q=0"}, false},
		{"gzip with space after the semicolon", []string{"gzip; q=0"}, false},
		{"a parameter ending in q is not the weight", []string{"gzip;fooq=0"}, true},
		{"second token offers gzip", []string{"deflate, gzip"}, true},
		{"second token gzip disabled", []string{"deflate, gzip;q=0"}, false},
		{"no gzip offered", []string{"br, deflate"}, false},
		{"a coding that only starts with gzip", []string{"gzipx"}, false},
		{"identity only", []string{"identity"}, false},
		{"x-gzip is gzip", []string{"x-gzip"}, true},
		{"x-gzip refused", []string{"x-gzip;q=0, *"}, false},
		{"wildcard offers gzip", []string{"*"}, true},
		{"wildcard with non-zero q offers gzip", []string{"*;q=0.5"}, true},
		{"wildcard refused with q=0", []string{"*;q=0"}, false},
		{"wildcard alongside other codings", []string{"identity, *"}, true},
		{"explicit gzip refusal beats a wildcard", []string{"gzip;q=0, *"}, false},
		{"explicit gzip q=0.0 refusal beats a wildcard", []string{"gzip;q=0.0, *"}, false},
		{"explicit gzip refusal beats a preceding wildcard", []string{"*, gzip;q=0"}, false},
		{"explicit gzip acceptance beats a refused wildcard", []string{"*;q=0, gzip"}, true},
		{"malformed q is permissive", []string{"gzip;q=bogus"}, true},
		{"q=0 with trailing parameter still refuses", []string{"gzip;q=0;dummy=x"}, false},
		{"q=0.0 with trailing parameter still refuses", []string{"gzip;q=0.0;dummy=x"}, false},
		{"non-zero q with trailing parameter accepts", []string{"gzip;q=0.5;dummy=x"}, true},
		{"malformed q with trailing parameter is permissive", []string{"gzip;q=bogus;dummy=x"}, true},
		{"empty list elements are skipped", []string{" , ,gzip"}, true},
		{"gzip on a later field line", []string{"identity", "gzip"}, true},
		{"a refusal on a later field line beats an earlier wildcard", []string{"*", "gzip;q=0"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			for _, line := range tc.lines {
				req.Header.Add("Accept-Encoding", line)
			}
			if got := acceptsGzip(req); got != tc.want {
				t.Errorf("acceptsGzip(Accept-Encoding=%q) = %v, want %v", tc.lines, got, tc.want)
			}
		})
	}
}
