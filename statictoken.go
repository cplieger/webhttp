package webhttp

import (
	"crypto/sha256"
	"crypto/subtle"
)

// StaticTokenVerifier verifies presented credentials against the single
// operator-configured secret guarding an endpoint. It is the constant-time
// verification primitive for static machine credentials — an API key, a
// bearer token, a basic-auth user or password — where exactly one expected
// value comes from configuration. It verifies one shared secret, not user
// identities: per-user credential stores, password hashing, and session
// management belong to the auth library (github.com/cplieger/auth), not here.
//
// Construct it once with NewStaticTokenVerifier and reuse it for every
// request. The configured secret is hashed with SHA-256 at construction;
// Verify hashes only the presented value and compares the two fixed-length
// digests with subtle.ConstantTimeCompare. Comparing 32-byte digests rather
// than the raw strings removes ConstantTimeCompare's unequal-length
// short-circuit (CWE-208), and pre-hashing the configured secret means no
// per-call timing varies with the secret's length or content — hashing the
// presented value per call reveals only what the caller already knows: its
// own input.
//
// The zero value fails CLOSED: Verify returns false for every presented
// value until the verifier is constructed from a non-empty secret.
type StaticTokenVerifier struct {
	digest [sha256.Size]byte
	set    bool
}

// NewStaticTokenVerifier builds a verifier for the configured secret.
//
// An empty configured secret yields the zero verifier, which fails CLOSED:
// Verify returns false for every presented value, including the empty
// string. Without that guard the digest comparison would fail OPEN —
// sha256("") equals sha256(""), so an unset secret would grant access to any
// client that simply presents no credential. Treat an empty configured value
// as "auth not configured", never as "no credential required": a caller that
// wants an open endpoint should skip the gate explicitly rather than rely on
// an empty secret.
func NewStaticTokenVerifier(configured string) StaticTokenVerifier {
	if configured == "" {
		return StaticTokenVerifier{}
	}
	return StaticTokenVerifier{digest: sha256.Sum256([]byte(configured)), set: true}
}

// Verify reports whether presented matches the configured secret. It is safe
// for concurrent use.
func (v StaticTokenVerifier) Verify(presented string) bool {
	if !v.set {
		return false
	}
	p := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(v.digest[:], p[:]) == 1
}
