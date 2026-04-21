package imgen

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/publicsuffix"
)

// Client 一个 chatgpt.com 网页端图片生成的逆向客户端。
//
// 单个 Client 绑定一个 access_token（一个 OAuth 账号）。Bootstrap 成功后持久
// 的 cookie（oai-did / __cf_bm / _cfuvid）存在内嵌的 cookiejar 里，多次
// GenerateImage 复用同一 Client 能省掉重复的 bootstrap + Cloudflare 握手。
//
// **不** 并发安全：同一个 Client 同一时间只发起一次 GenerateImage 调用。
// 若上层要并发，给每个请求 / 账户实例化独立的 Client（NewClient 开销主要
// 在首次 Bootstrap，Cookie 可外部持久化后注入）。
type Client struct {
	http         *http.Client
	accessToken  string
	deviceID     string
	sessionID    string
	bootstrapped bool
}

// NewClient 构造一个 Client。proxyURL 可为 nil（直连）。
func NewClient(accessToken string, proxyURL *url.URL) *Client {
	transport := newUTLSTransport(proxyURL, 30*time.Second)
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})

	return &Client{
		http: &http.Client{
			Transport: transport,
			Timeout:   180 * time.Second,
			Jar:       jar,
		},
		accessToken: accessToken,
		deviceID:    uuid.New().String(),
		sessionID:   uuid.New().String(),
	}
}

// doNoRedirect 发一次请求但不自动跟随 302/301，返回原始响应（用于读 Location）。
//
// 注：修改了 c.http.CheckRedirect，非并发安全。调用方必须保证同一 Client 上
// 没有并发请求；本包内 pollForImages + 下载流程本身就是串行的，没问题。
func (c *Client) doNoRedirect(req *http.Request) (*http.Response, error) {
	orig := c.http.CheckRedirect
	c.http.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	defer func() { c.http.CheckRedirect = orig }()
	return c.http.Do(req)
}

// Bootstrap 访问首页拿 Cloudflare / OpenAI 的前置 cookie（oai-did / __cf_bm /
// _cfuvid / oai-chat-web-route）。不做这一步，后续 /backend-api/* 会被直接
// 302 到 Turnstile 挑战页。
func (c *Client) Bootstrap() error {
	req, err := http.NewRequest("GET", BaseURL+"/", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", DefaultUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("bootstrap 请求失败: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	u, _ := url.Parse(BaseURL)
	cookies := c.http.Jar.Cookies(u)
	names := make([]string, 0, len(cookies))
	for _, ck := range cookies {
		names = append(names, ck.Name)
	}
	log.Printf("[imgen] bootstrap 获取 %d 个 cookie", len(cookies))
	_ = names
	return nil
}

// setCommonHeaders 设置完整的浏览器伪装头。任一 sec-ch-ua-* / oai-* 头缺失
// 都会让 Sentinel 把请求判为脚本，触发 chat-requirements 的难度升级或 403。
func (c *Client) setCommonHeaders(req *http.Request) {
	req.Header.Set("Authority", "chatgpt.com")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6")
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Origin", BaseURL)
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Priority", "u=1, i")
	req.Header.Set("Referer", BaseURL+"/")
	req.Header.Set("User-Agent", DefaultUA)

	req.Header.Set("Sec-Ch-Ua", `"Chromium";v="131", "Microsoft Edge";v="131", "Not A(Brand";v="24"`)
	req.Header.Set("Sec-Ch-Ua-Arch", `"x86"`)
	req.Header.Set("Sec-Ch-Ua-Bitness", `"64"`)
	req.Header.Set("Sec-Ch-Ua-Full-Version", `"131.0.0.0"`)
	req.Header.Set("Sec-Ch-Ua-Full-Version-List", `"Chromium";v="131.0.0.0", "Microsoft Edge";v="131.0.0.0", "Not A(Brand";v="24.0.0.0"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Model", `""`)
	req.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	req.Header.Set("Sec-Ch-Ua-Platform-Version", `"19.0.0"`)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	req.Header.Set("Oai-Device-Id", c.deviceID)
	req.Header.Set("Oai-Session-Id", c.sessionID)
	req.Header.Set("Oai-Language", "zh-CN")
	req.Header.Set("Oai-Client-Version", clientVersion)
	req.Header.Set("Oai-Client-Build-Number", clientBuildNum)
}

func (c *Client) newReq(method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	c.setCommonHeaders(req)
	req.Header.Set("X-Openai-Target-Path", path)
	req.Header.Set("X-Openai-Target-Route", path)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}
