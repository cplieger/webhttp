package webhttp

import "path"

// CanonicalRequestPath returns the request path http.ServeMux will route p as,
// and reports whether p already IS that path.
//
// clean is the same computation net/http performs on every non-CONNECT request
// before it selects a pattern: path.Clean, with a non-root trailing slash put
// back (see cleanPath in net/http's server.go). canonical is clean == p, so it
// answers "will the mux route this spelling, or rewrite it first?".
//
// # Why a library surface exists for this
//
// When the cleaned path differs, ServeMux answers 307 with a Location instead of
// calling any handler, and no registered pattern can intercept it — the
// canonicalization runs BEFORE pattern selection. For a browser that is
// invisible and harmless. For the machine senders these services actually have
// it is neither: a 307 is a SUCCESS status to a client that does not follow
// redirects (`curl -fsS` without -L is the documented sender for several of
// them), so such a caller exits 0 having never reached the handler. Nothing was
// recorded, no job ran, and nothing anywhere says the URL was malformed —
// the failure surfaces a deadline later as silence, if at all. A route whose
// whole purpose is a side effect wants to refuse the non-canonical spelling
// itself rather than let the redirect answer for it.
//
// # Which value to pass, and what that means
//
// ServeMux cleans r.URL.EscapedPath(), so passing that is what reproduces its
// cleaning decision exactly. Passing the DECODED r.URL.Path instead makes the
// verdict strictly WIDER: %2e%2e decodes to .., so an encoded dot segment
// reports non-canonical here while ServeMux draws no redirect for it. That is a
// deliberate choice available to the caller, not a defect — the decoded path is
// the one the sender believed it was addressing — but it is a choice, so pick
// the value on purpose.
//
// # What canonical does NOT claim
//
// It is the verdict of the cleaning step alone, not "ServeMux will not redirect
// this request". Two things are outside it, both by construction:
//
//   - The trailing-slash redirect. When a subtree pattern "/tree/" is
//     registered and the request is the already-canonical "/tree", ServeMux
//     redirects to "/tree/". That depends on the route table, not on the
//     path's spelling, so a pure function over p cannot see it.
//   - CONNECT. net/http does not canonicalize a CONNECT request's path at all,
//     so the cleaning redirect never applies to one.
//
// # Edge inputs
//
// An empty p returns "/" and false, and a p with no leading slash is rooted
// before cleaning ("beat/api" cleans to "/beat/api"), so it can never equal p
// and always reports false. Both mirror net/http's own rooting, and neither
// reaches a handler from net/http on a normal request (an origin-form request
// target starts with "/", and an empty one is rejected before routing), so they
// are the answers for a caller passing a value from somewhere else.
//
// # What stays the caller's
//
// Everything but the verdict: which routes the check applies to, whether a
// non-canonical spelling is refused or merely logged, the refusal's status and
// body, and any metric that counts the class. This is a pure function over a
// string and nothing else.
func CanonicalRequestPath(p string) (clean string, canonical bool) {
	if p == "" {
		return "/", false
	}
	rooted := p
	if rooted[0] != '/' {
		rooted = "/" + rooted
	}
	clean = path.Clean(rooted)
	// path.Clean drops a trailing slash (except at the root) and net/http puts
	// it back. That is load-bearing rather than cosmetic: ServeMux matches a
	// subtree pattern on the trailing-slash form, so folding it away here would
	// report a path the mux routes as one it rewrites, and a caller refusing on
	// that verdict would answer its own routes with a refusal.
	if rooted[len(rooted)-1] == '/' && clean != "/" {
		clean += "/"
	}
	return clean, clean == p
}
