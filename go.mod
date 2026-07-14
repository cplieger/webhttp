module github.com/cplieger/webhttp

go 1.26.5

// v1.5.0 shipped same-day sse API that v1.6.0 reshapes: sse.Replay(sinceID,
// topic) was replaced by the parameterless snapshot sse.Buffered(), and the
// sse refusal responses moved onto the standard ErrorResponse envelope.
// Nothing external consumed v1.5.0; use v1.6.0 or later.
retract v1.5.0
