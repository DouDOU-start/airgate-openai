package imgen

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
)

// ImageInput 调用方传入的待上传图片（内存中的原始二进制）。
type ImageInput struct {
	Data     []byte // 图片原始字节
	MimeType string // 可选；为空时自动检测
	FileName string // 可选；为空时根据 MimeType 生成
}

// UploadedFile 上传到 chatgpt.com 后的元数据，用于构建 multimodal_text 消息。
type UploadedFile struct {
	FileID   string
	FileName string
	MimeType string
	Size     int64
	Width    int
	Height   int
}

func detectMimeType(data []byte) string {
	if len(data) >= 4 {
		if data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G' {
			return "image/png"
		}
		if data[0] == 0xFF && data[1] == 0xD8 {
			return "image/jpeg"
		}
		if string(data[:4]) == "GIF8" {
			return "image/gif"
		}
		if string(data[:4]) == "RIFF" && len(data) >= 12 && string(data[8:12]) == "WEBP" {
			return "image/webp"
		}
	}
	return "image/png"
}

func getImageDimensions(data []byte) (width, height int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err == nil {
		return cfg.Width, cfg.Height
	}
	if len(data) >= 24 && data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G' {
		w := binary.BigEndian.Uint32(data[16:20])
		h := binary.BigEndian.Uint32(data[20:24])
		return int(w), int(h)
	}
	return 0, 0
}

func mimeToExt(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

// uploadFile 执行完整的 3 步上传流程：fileCreate → 二进制上传 → fileConfirm。
func (c *Client) uploadFile(input ImageInput) (*UploadedFile, error) {
	if len(input.Data) == 0 {
		return nil, fmt.Errorf("图片数据为空")
	}

	mimeType := input.MimeType
	if mimeType == "" {
		mimeType = detectMimeType(input.Data)
	}
	fileName := input.FileName
	if fileName == "" {
		fileName = "image" + mimeToExt(mimeType)
	}
	fileSize := int64(len(input.Data))
	width, height := getImageDimensions(input.Data)

	log.Printf("[imgen] 上传文件: %s (%d bytes, %dx%d, %s)", fileName, fileSize, width, height, mimeType)

	fileID, uploadURL, err := c.fileCreate(fileName, fileSize, mimeType)
	if err != nil {
		return nil, fmt.Errorf("创建文件记录失败: %w", err)
	}
	log.Printf("[imgen] file_id: %s", fileID)

	if err := c.fileUploadData(fileID, uploadURL, input.Data, mimeType); err != nil {
		return nil, fmt.Errorf("上传文件失败: %w", err)
	}
	log.Println("[imgen] 二进制上传完成")

	if err := c.fileConfirm(fileID); err != nil {
		log.Printf("[imgen] 文件确认警告（非致命）: %v", err)
	}

	return &UploadedFile{
		FileID:   fileID,
		FileName: fileName,
		MimeType: mimeType,
		Size:     fileSize,
		Width:    width,
		Height:   height,
	}, nil
}

func (c *Client) fileCreate(fileName string, fileSize int64, mimeType string) (fileID, uploadURL string, err error) {
	body := map[string]any{
		"file_name": fileName,
		"file_size": fileSize,
		"use_case":  "multimodal",
	}
	data, _ := json.Marshal(body)
	req, err := c.newReq("POST", "/backend-api/files", bytes.NewReader(data))
	if err != nil {
		return "", "", err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, respBody)
	}

	var result struct {
		FileID    string `json:"file_id"`
		UploadURL string `json:"upload_url"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", "", fmt.Errorf("解析响应失败: %w, body=%s", err, respBody)
	}
	if result.FileID == "" {
		return "", "", fmt.Errorf("未获取到 file_id, body=%s", respBody)
	}
	return result.FileID, result.UploadURL, nil
}

// fileUploadData 上传内存中的二进制数据到 upload_url 或 fallback 到 stream 上传。
func (c *Client) fileUploadData(fileID, uploadURL string, data []byte, mimeType string) error {
	if uploadURL != "" {
		return c.fileUploadToURL(uploadURL, bytes.NewReader(data), mimeType, fileID)
	}
	return c.fileUploadStream(fileID, bytes.NewReader(data), mimeType)
}

func (c *Client) fileUploadToURL(uploadURL string, body io.Reader, mimeType, fileID string) error {
	isAzureBlob := strings.Contains(uploadURL, "oaiusercontent.com")

	method := "PUT"
	if strings.Contains(uploadURL, "process_upload_stream") {
		method = "POST"
	}

	if strings.HasPrefix(uploadURL, "/") {
		uploadURL = BaseURL + uploadURL
	}

	req, err := http.NewRequest(method, uploadURL, body)
	if err != nil {
		return err
	}

	if isAzureBlob {
		req.Header.Set("x-ms-blob-type", "BlockBlob")
		req.Header.Set("x-ms-version", "2023-11-03")
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Origin", BaseURL)
	} else if strings.HasPrefix(uploadURL, BaseURL) {
		c.setCommonHeaders(req)
		req.Header.Set("Content-Type", mimeType)
		req.Header.Set("X-File-Id", fileID)
	} else {
		req.Header.Set("User-Agent", DefaultUA)
		req.Header.Set("Content-Type", mimeType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return fmt.Errorf("upload HTTP %d: %s", resp.StatusCode, respBody)
	}

	if !strings.HasPrefix(uploadURL, BaseURL) {
		return c.fileFinalize(fileID)
	}
	return nil
}

func (c *Client) fileUploadStream(fileID string, body io.Reader, mimeType string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("file_id", fileID)
	part, err := w.CreateFormFile("file", fileID)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, body); err != nil {
		return err
	}
	_ = w.Close()

	req, err := c.newReq("POST", "/backend-api/files/process_upload_stream", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return fmt.Errorf("process_upload_stream HTTP %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

func (c *Client) fileFinalize(fileID string) error {
	req, err := c.newReq("POST", "/backend-api/files/"+fileID+"/uploaded", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("files/%s/uploaded HTTP %d", fileID, resp.StatusCode)
	}
	return nil
}

func (c *Client) fileConfirm(fileID string) error {
	req, err := c.newReq("GET", "/backend-api/files/download/"+fileID, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("files/download/%s HTTP %d", fileID, resp.StatusCode)
	}
	return nil
}
