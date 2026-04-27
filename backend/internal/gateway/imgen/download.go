package imgen

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// ---------- Step 5: 下载图片二进制 ----------
//
// file-service 的下载路径有两代：
//
//	新版（2024-Q4 起真实 trace 里观察到）：
//	  GET /backend-api/files/download/{file_id}?conversation_id=<cid>&inline=false
//	  → 302 到 https://files.oaiusercontent.com/... 签名 URL
//
//	旧版：
//	  GET /backend-api/files/{file_id}/download
//	  → 200 { "download_url": "https://files.oaiusercontent.com/..." }
//
// 优先走新版；遇到非 302（服务端返回 JSON 或 200）时 fallback 到旧版。
//
// sediment 用会话内 attachment 路径，两代一致，保持不变。

func (c *Client) downloadImage(conversationID, ref string) ([]byte, error) {
	switch {
	case strings.HasPrefix(ref, "file-service://"):
		fileID := strings.TrimPrefix(ref, "file-service://")
		return c.downloadFileService(conversationID, fileID)
	case strings.HasPrefix(ref, "sediment://"):
		sedID := strings.TrimPrefix(ref, "sediment://")
		return c.downloadByJSONLink("/backend-api/conversation/" + conversationID + "/attachment/" + sedID + "/download")
	default:
		return nil, fmt.Errorf("未知引用格式: %s", ref)
	}
}

func (c *Client) downloadFileService(conversationID, fileID string) ([]byte, error) {
	newPath := fmt.Sprintf(
		"/backend-api/files/download/%s?conversation_id=%s&inline=false",
		url.PathEscape(fileID), url.QueryEscape(conversationID),
	)
	req, err := c.newReq("GET", newPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.doNoRedirect(req)
	if err != nil {
		return nil, err
	}

	// 1) 302/301/307 → 跟 Location
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := resp.Header.Get("Location")
		_ = resp.Body.Close()
		if loc == "" {
			return nil, fmt.Errorf("files/download 返回 %d 但 Location 为空", resp.StatusCode)
		}
		return c.fetchBinary(loc)
	}

	// 2) 200 JSON（兼容返回 { download_url } 或直接二进制的形式）
	if resp.StatusCode == 200 {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		var dl struct {
			DownloadURL string `json:"download_url"`
		}
		if json.Unmarshal(body, &dl) == nil && dl.DownloadURL != "" {
			return c.fetchBinary(dl.DownloadURL)
		}
		if len(body) > 0 {
			return body, nil
		}
	}

	// 3) 其它情况 → fallback 到旧版 JSON 链接
	_ = resp.Body.Close()
	slog.Default().Debug("imgen_download_fallback",
		"http_status", resp.StatusCode,
		"file_id", fileID,
	)
	return c.downloadByJSONLink("/backend-api/files/" + fileID + "/download")
}

// downloadByJSONLink 走旧式路径：先拿 JSON { download_url }，再 GET CDN 拿二进制。
func (c *Client) downloadByJSONLink(path string) ([]byte, error) {
	req, err := c.newReq("GET", path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("获取下载链接失败 HTTP %d: %s", resp.StatusCode, b)
	}
	var dl struct {
		DownloadURL string `json:"download_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dl); err != nil {
		return nil, err
	}
	if dl.DownloadURL == "" {
		return nil, fmt.Errorf("下载链接为空")
	}
	return c.fetchBinary(dl.DownloadURL)
}

// fetchBinary 拿二进制：chatgpt.com 内部 URL 用完整认证头，外部 CDN 仅用 UA。
func (c *Client) fetchBinary(targetURL string) ([]byte, error) {
	imgReq, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(targetURL, BaseURL+"/") {
		c.setCommonHeaders(imgReq)
		imgReq.Header.Set("Accept", "image/*,*/*;q=0.8")
	} else {
		imgReq.Header.Set("User-Agent", DefaultUA)
		imgReq.Header.Set("Accept", "image/*,*/*;q=0.8")
	}
	imgResp, err := c.http.Do(imgReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = imgResp.Body.Close() }()
	if imgResp.StatusCode != 200 {
		b, _ := io.ReadAll(imgResp.Body)
		return nil, fmt.Errorf("下载图片失败 HTTP %d: %s", imgResp.StatusCode, b)
	}
	return io.ReadAll(imgResp.Body)
}
