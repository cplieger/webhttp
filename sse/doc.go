// Package sse provides a broadcast hub for Server-Sent Events: fan-out to
// connected clients, a replay ring with monotonic event IDs so a reconnecting
// client resumes via the standard Last-Event-ID header, proxy-defensive
// response headers, keepalive comments, an optional concurrent-client cap,
// per-subscriber topic filtering, and a shutdown drain gate.
//
// The Hub owns broadcast state; Serve adapts one HTTP request into a
// subscriber. Events carry pre-marshaled bytes (the hub does no JSON), an
// optional SSE event name (the `event:` field, for clients listening with
// addEventListener), and an optional topic that scopes delivery to
// subscribers that asked for it.
//
// # Delivery model
//
// Every published event is assigned a monotonically increasing ID, appended
// to a fixed-size replay ring, and fanned out to each subscriber's buffered
// channel. A subscriber that stops draining (a stalled client, a dead TCP
// peer behind a proxy) has its connection cancelled rather than the hub
// blocking: the standard EventSource client reconnects automatically,
// presents Last-Event-ID, and the ring replays what it missed. Replay and
// registration happen under one lock, so the replayed frames and the live
// channel are gap-free and overlap-free.
//
// A reconnect that presents an ID older than the ring's floor means events
// were lost; the OnConnect hook receives the current (floor, head) bounds so
// an application can hand the client that information and let it refetch
// authoritative state.
//
// The package has zero dependencies beyond the standard library.
package sse
