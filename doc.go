// Package webhttp provides resilient server-side HTTP plumbing for building
// small services on top of net/http.
//
// It bundles the pieces almost every server ends up hand-rolling:
//
//   - request-id injection plus one-line access logging (RequestLogger),
//   - a flush/hijack-safe status recorder that stays transparent to
//     http.ResponseController (StatusRecorder),
//   - JSON response and error helpers (WriteJSON, WriteJSONStatus, Ok,
//     WriteError),
//   - request-prelude helpers for body limiting, method gating, and JSON
//     decoding (LimitBody, RequireMethod, DecodeBody),
//   - an HTTP readiness gate for load balancers (Ready, ReadinessHandler),
//   - a graceful server bootstrap (NewServer, Run).
//
// webhttp is the inbound-server counterpart to httpx
// (github.com/cplieger/httpx), which is the outbound-client toolkit: httpx
// makes resilient requests going OUT, webhttp handles the requests coming IN.
// The two are complementary and share no code.
//
// The package has zero dependencies beyond the standard library. It ships the
// mechanism only; each consuming application layers its own route table, error
// taxonomy, and named helpers on top.
package webhttp
