package webhttp_test

import (
	"strings"
	"testing"

	"github.com/cplieger/webhttp"
)

func TestStaticTokenVerifier(t *testing.T) {
	long := strings.Repeat("k", 300) // spans several SHA-256 blocks
	cases := []struct {
		name       string
		configured string
		presented  string
		want       bool
	}{
		{"equal non-empty", "s3cr3t-token", "s3cr3t-token", true},
		{"single char equal", "x", "x", true},
		{"long equal", long, long, true},
		{"unicode equal", "pässwörd-日本語", "pässwörd-日本語", true},
		{"embedded NUL equal", "a\x00b", "a\x00b", true},
		{"unequal same length", "aaaaaaaa", "aaaaaaab", false},
		{"unequal different length", "short", "short-but-longer", false},
		{"presented is prefix of configured", "topsecret", "top", false},
		{"configured is prefix of presented", "top", "topsecret", false},
		{"case sensitive", "Secret", "secret", false},
		{"long vs one-byte flip", long, long[:len(long)-1] + "x", false},
		{"embedded NUL vs plain", "a\x00b", "ab", false},
		{"unicode vs ascii lookalike", "pässwörd", "password", false},
		{"empty configured, empty presented (sha256 empty-equality trap)", "", "", false},
		{"empty configured, non-empty presented", "", "anything", false},
		{"empty configured, whitespace presented", "", " ", false},
		{"empty presented, non-empty configured", "configured", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := webhttp.NewStaticTokenVerifier(tc.configured)
			if got := v.Verify(tc.presented); got != tc.want {
				t.Errorf("NewStaticTokenVerifier(%q).Verify(%q) = %v, want %v", tc.configured, tc.presented, got, tc.want)
			}
		})
	}

	t.Run("zero value fails closed", func(t *testing.T) {
		var zero webhttp.StaticTokenVerifier
		for _, presented := range []string{"", "anything", " "} {
			if zero.Verify(presented) {
				t.Errorf("zero StaticTokenVerifier.Verify(%q) = true, want false", presented)
			}
		}
	})

	t.Run("reusable across calls", func(t *testing.T) {
		v := webhttp.NewStaticTokenVerifier("s3cr3t")
		for range 3 {
			if !v.Verify("s3cr3t") {
				t.Error("repeated Verify of the correct secret = false, want true")
			}
			if v.Verify("wrong") {
				t.Error("repeated Verify of a wrong secret = true, want false")
			}
		}
	})
}
