package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// imageKeepAliveInterval controls how frequently SSE ping events are sent during
// long-running image generation requests to prevent Cloudflare 524 timeouts.
// Cloudflare's free-tier origin timeout is 100 s; 30 s gives ample margin.
const imageKeepAliveInterval = 30 * time.Second

type ssePingKeepAlive struct {
	w      http.ResponseWriter
	cancel context.CancelFunc
	done   chan struct{}
}

func startSSEPingKeepAlive(w http.ResponseWriter) *ssePingKeepAlive {
	if w == nil {
		return nil
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	ctx, cancel := context.WithCancel(context.Background())
	ka := &ssePingKeepAlive{w: w, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(ka.done)
		t := time.NewTicker(imageKeepAliveInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				writeSSEPing(w)
			}
		}
	}()
	return ka
}

func (ka *ssePingKeepAlive) Stop() {
	if ka == nil {
		return
	}
	ka.cancel()
	<-ka.done
}

func writeSSEPing(w http.ResponseWriter) {
	_, _ = w.Write([]byte("event: ping\ndata: {}\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeSSEData(w http.ResponseWriter, data []byte) {
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeSSEDone(w http.ResponseWriter) {
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeSSEError(w http.ResponseWriter, message string) {
	if message != imageTooLargeSSEErrorMessage {
		message = sanitizedImageSSEErrorMessage
	}
	errEvent, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "server_error",
		},
	})
	writeSSEData(w, errEvent)
	writeSSEDone(w)
}

func writeImagesRESTSSE(w http.ResponseWriter, body []byte) {
	writeSSEData(w, body)
	writeSSEDone(w)
}
