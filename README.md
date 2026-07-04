# webhttp

> Resilient server-side HTTP plumbing for Go

Request-id access logging, a flush/hijack-safe status recorder, JSON response and error helpers, request-prelude helpers, an HTTP readiness gate, and a graceful server bootstrap. The inbound-server counterpart to [httpx](https://github.com/cplieger/httpx). Standard-library only, no external runtime dependencies.

## Install

`go get github.com/cplieger/webhttp@latest`

## Usage

```go
package main

import (
	"context"
	"net"
	"net/http"
	"os/signal"
	"syscall"

	"github.com/cplieger/webhttp"
)

func main() {
	ready := &webhttp.Ready{}

	mux := http.NewServeMux()
	mux.Handle("GET /readyz", webhttp.ReadinessHandler(ready))
	mux.HandleFunc("GET /hello", func(w http.ResponseWriter, _ *http.Request) {
		webhttp.WriteJSON(w, map[string]string{"msg": "hello"})
	})

	// RequestLogger mints/echoes a request id and logs one line per request;
	// NewServer applies streaming-safe timeouts; Run serves until SIGINT/SIGTERM
	// then shuts down gracefully.
	srv := webhttp.NewServer(webhttp.RequestLogger(mux))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", ":8080")
	if err != nil {
		panic(err)
	}
	ready.Set(true)
	if err := webhttp.Run(ctx, srv, ln, nil); err != nil {
		panic(err)
	}
}
```
