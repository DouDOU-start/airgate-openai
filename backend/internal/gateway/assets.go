package gateway

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed webdist/*
var webDistFS embed.FS

// GetWebAssets 实现 sdk.WebAssetsProvider 接口
// 开发模式优先读取磁盘上的 web/dist，生产模式回退到嵌入资源。
// 这样开发时只需要重新构建插件前端并热加载插件后端，不需要手动复制 webdist。
func (g *OpenAIGateway) GetWebAssets() map[string][]byte {
	if assets := loadDevWebAssets(); len(assets) > 0 {
		return assets
	}

	assets := make(map[string][]byte)
	fs.WalkDir(webDistFS, "webdist", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		content, err := webDistFS.ReadFile(path)
		if err != nil {
			return nil
		}
		// 去掉 "webdist/" 前缀，保留相对路径
		relPath := strings.TrimPrefix(path, "webdist/")
		assets[relPath] = content
		return nil
	})
	return assets
}

func loadDevWebAssets() map[string][]byte {
	candidates := []string{
		filepath.Join("..", "web", "dist"),
		filepath.Join("web", "dist"),
	}

	for _, dir := range candidates {
		assets := loadAssetsFromDir(dir)
		if len(assets) > 0 {
			return assets
		}
	}

	return nil
}

func loadAssetsFromDir(root string) map[string][]byte {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}

	assets := make(map[string][]byte)
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}

		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}

		relPath, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}

		assets[filepath.ToSlash(relPath)] = content
		return nil
	})

	if len(assets) == 0 {
		return nil
	}
	return assets
}
