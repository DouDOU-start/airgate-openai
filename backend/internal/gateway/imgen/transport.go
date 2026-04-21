// Package imgen 封装 chatgpt.com 网页端图片生成的逆向调用链路。
//
// 能力定位：
//
//	用 OAuth access_token（网页 OAuth 授权得到）直接打 chatgpt.com 的内部
//	/backend-api/f/conversation SSE 流，不走 OpenAI Images REST，也不走 ChatGPT
//	Responses API 的 image_generation tool。输出的图像质量对齐 "gpt-image-2"
//	灰度桶，比 image_generation tool 默认输出稍好，但每次生成消耗一次订阅额度。
//
// 本包从 image2gen/（独立 CLI 工具）迁移而来，去掉 CLI 入口，改为库形式：
//
//	client := imgen.NewClient(accessToken, proxyURL)
//	result, err := client.GenerateImage(ctx, "prompt 文本")
//	// result.Images[i].Data 就是 PNG 二进制
//
// image2gen 目录保留作为独立调试工具，两边代码在功能层保持一致但独立演进。
package imgen

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
)

// ---------- 常量：模拟 Edge 浏览器指纹 ----------

const (
	BaseURL        = "https://chatgpt.com"
	DefaultUA      = `Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0`
	clientVersion  = "prod-be885abbfcfe7b1f511e88b3003d9ee44757fbad"
	clientBuildNum = "5955942"
)

// ---------- uTLS Transport ----------
//
// chatgpt.com 前置了 Cloudflare，标准 Go net/http 的 TLS 指纹（JA3）会被识别
// 为脚本。这里用 utls 把 ClientHello 指纹化成 Chrome 131，并强制 ALPN 为
// http/1.1（chatgpt.com 网页端实际走 HTTP/1.1；上 HTTP/2 会被上游判为异常）。

type utlsRoundTripper struct {
	proxyURL *url.URL
	dialer   *net.Dialer
	h1       *http.Transport
}

func newUTLSTransport(proxyURL *url.URL, timeout time.Duration) *utlsRoundTripper {
	rt := &utlsRoundTripper{
		proxyURL: proxyURL,
		dialer:   &net.Dialer{Timeout: timeout},
	}
	rt.h1 = &http.Transport{
		DialTLSContext:        rt.dialTLS,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     false,
	}
	return rt
}

func (rt *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return rt.h1.RoundTrip(req)
}

func (rt *utlsRoundTripper) dialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	rawConn, err := rt.dialRaw(ctx, addr)
	if err != nil {
		return nil, err
	}

	host, _, _ := net.SplitHostPort(addr)
	uconn := utls.UClient(rawConn, &utls.Config{ServerName: host}, utls.HelloChrome_131)

	// 强制 HTTP/1.1
	if err := uconn.BuildHandshakeState(); err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("utls build state: %w", err)
	}
	for _, ext := range uconn.Extensions {
		if alpnExt, ok := ext.(*utls.ALPNExtension); ok {
			alpnExt.AlpnProtocols = []string{"http/1.1"}
		}
	}

	if err := uconn.HandshakeContext(ctx); err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("TLS 握手失败: %w", err)
	}

	np := uconn.ConnectionState().NegotiatedProtocol
	if np != "" && np != "http/1.1" {
		_ = uconn.Close()
		return nil, fmt.Errorf("ALPN 协商了 %q，期望 http/1.1", np)
	}
	return uconn, nil
}

func (rt *utlsRoundTripper) dialRaw(ctx context.Context, addr string) (net.Conn, error) {
	if rt.proxyURL == nil {
		return rt.dialer.DialContext(ctx, "tcp", addr)
	}

	proxyAddr := rt.proxyURL.Host
	if !strings.Contains(proxyAddr, ":") {
		proxyAddr += ":80"
	}

	conn, err := rt.dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("连接代理 %s 失败: %w", proxyAddr, err)
	}

	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	connectReq.Header.Set("User-Agent", DefaultUA)
	if u := rt.proxyURL.User; u != nil {
		pw, _ := u.Password()
		connectReq.Header.Set("Proxy-Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(u.Username()+":"+pw)))
	}
	if err := connectReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("发送 CONNECT 失败: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("读取 CONNECT 响应失败: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("代理 CONNECT 返回 %s", resp.Status)
	}

	if n := br.Buffered(); n > 0 {
		peeked, _ := br.Peek(n)
		buf := make([]byte, n)
		copy(buf, peeked)
		return &bufConn{
			Conn:   conn,
			reader: bufio.NewReaderSize(io.MultiReader(bytes.NewReader(buf), conn), 4096),
		}, nil
	}
	return conn, nil
}

type bufConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}
