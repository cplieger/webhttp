package webhttp

import (
	"net/http"
	"net/netip"
)

// ForwardingFields projects parseForwarding's one parse of a request's
// forwarding fields, so black-box tests can derive ResolveClient's answer
// from it without parsing a header themselves.
type ForwardingFields struct {
	XFF, RealIP []ListEntry
	Forwarded   []ForwardedElement
}

type ListEntry struct {
	Addr netip.Addr
	Bare bool
}

type ForwardedElement struct {
	For   netip.Addr
	Valid bool
}

func ParseForwarding(h http.Header) ForwardingFields {
	f := parseForwarding(h)
	project := func(entries []listEntry) []ListEntry {
		var out []ListEntry
		for _, e := range entries {
			out = append(out, ListEntry{Addr: e.addr, Bare: e.bare})
		}
		return out
	}
	out := ForwardingFields{XFF: project(f.xff), RealIP: project(f.realIP)}
	for _, e := range f.forwarded {
		out.Forwarded = append(out.Forwarded, ForwardedElement{For: e.forNode, Valid: e.valid})
	}
	return out
}
