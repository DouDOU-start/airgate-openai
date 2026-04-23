package gateway

import (
	"context"
	"net/http"
	"time"
)

// imageKeepAliveInterval controls how frequently whitespace is sent during
// long-running image generation requests to prevent Cloudflare 524 timeouts.
// Cloudflare's free-tier origin timeout is 100 s; 30 s gives ample margin.
const imageKeepAliveInterval = 30 * time.Second

// imageKeepAlive sends periodic whitespace bytes on an http.ResponseWriter
// to prevent Cloudflare 524 (origin timeout) errors during image generation.
//
// JSON parsers ignore leading whitespace, so the final response body
// "   {...}" remains valid and transparent to OpenAI SDK clients.
type imageKeepAlive struct {
	w      http.ResponseWriter
	active bool // HTTP 200 already committed by keep-alive
	cancel context.CancelFunc
	done   chan struct{}
}

// startImageKeepAlive begins periodic whitespace writes on w.
// Returns nil when w is nil (no writer → nothing to keep alive).
func startImageKeepAlive(w http.ResponseWriter) *imageKeepAlive {
	if w == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	ka := &imageKeepAlive{
		w:      w,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go ka.run(ctx)
	return ka
}

func (ka *imageKeepAlive) run(ctx context.Context) {
	defer close(ka.done)
	t := time.NewTicker(imageKeepAliveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !ka.active {
				ka.w.Header().Set("Content-Type", "application/json")
				ka.w.WriteHeader(http.StatusOK)
				ka.active = true
			}
			_, _ = ka.w.Write([]byte(" "))
			if f, ok := ka.w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
}

// Stop cancels the keep-alive goroutine and waits for it to exit.
func (ka *imageKeepAlive) Stop() {
	ka.cancel()
	<-ka.done
}

// Finish stops the keep-alive goroutine and writes the final response.
// If keep-alive already committed HTTP 200, statusCode is ignored and only
// body is appended. Otherwise the full response (headers + status + body)
// is written with the given statusCode.
func (ka *imageKeepAlive) Finish(statusCode int, body []byte) {
	ka.cancel()
	<-ka.done
	if !ka.active {
		ka.w.Header().Set("Content-Type", "application/json")
		ka.w.WriteHeader(statusCode)
	}
	_, _ = ka.w.Write(body)
	if f, ok := ka.w.(http.Flusher); ok {
		f.Flush()
	}
}
