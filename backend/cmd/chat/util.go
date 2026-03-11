package main

import (
	"crypto/tls"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

func buildUserMsg(text string) map[string]any {
	return map[string]any{
		"type": "message",
		"role": "user",
		"content": []map[string]string{
			{"type": "input_text", "text": text},
		},
	}
}

func buildAssistantMsg(text string) map[string]any {
	return map[string]any{
		"type": "message",
		"role": "assistant",
		"content": []map[string]string{
			{"type": "output_text", "text": text},
		},
	}
}

func printStats(model string, input, output, cache int, duration time.Duration) {
	if input > 0 || output > 0 {
		fmt.Fprintf(os.Stderr, "\n[%s | 输入: %d 输出: %d 缓存: %d | %s]",
			model, input, output, cache, duration.Round(time.Millisecond))
	}
}

func buildReasoning(effort string) map[string]any {
	return map[string]any{
		"effort":  effort,
		"summary": "auto",
	}
}

func generateCacheKey() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 16)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return "chat-" + string(b)
}

func buildClient(proxy string) *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: transport, Timeout: 300 * time.Second}
}
