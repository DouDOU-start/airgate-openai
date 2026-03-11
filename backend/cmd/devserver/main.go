// devserver 插件开发服务器
// 模拟 AirGate 核心的最小行为，用于插件端到端验证
// 用法: go run ./cmd/devserver [-addr :8080] [-data ./devdata]
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/DouDOU-start/airgate-openai/backend/internal/gateway"
)

//go:embed static
var staticFiles embed.FS

func main() {
	addr := flag.String("addr", ":18080", "监听地址")
	dataDir := flag.String("data", "./devdata", "数据目录")
	logFile := flag.String("log", "./devdata/debug.log", "日志文件路径")
	flag.Parse()

	// 初始化日志：控制台 INFO 级别，文件 DEBUG 级别
	if err := os.MkdirAll(filepath.Dir(*logFile), 0o755); err == nil {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			// 控制台只输出 INFO 及以上
			consoleHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
			// 文件输出 DEBUG 及以上
			fileHandler := slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})
			slog.SetDefault(slog.New(&multiHandler{handlers: []slog.Handler{consoleHandler, fileHandler}}))
			log.SetOutput(io.MultiWriter(os.Stderr, f))
			log.Printf("日志文件: %s", *logFile)
		}
	}

	// 初始化插件
	gw := &gateway.OpenAIGateway{}
	if err := gw.Init(nil); err != nil {
		log.Fatalf("插件初始化失败: %v", err)
	}

	// 初始化账号存储
	store := NewAccountStore(filepath.Join(*dataDir, "accounts.json"))

	// 路由
	mux := http.NewServeMux()

	// 插件信息 API
	mux.HandleFunc("/api/plugin/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		info := gw.Info()
		json.NewEncoder(w).Encode(info)
	})

	// 账号管理 API
	accountHandler := &AccountHandler{store: store}
	mux.Handle("/api/accounts/", accountHandler)
	mux.Handle("/api/accounts", accountHandler)

	// OAuth 授权（用户手动复制回调 URL 完成授权）
	oauthHandler := &OAuthDevHandler{gateway: gw, store: store}
	mux.HandleFunc("/api/oauth/start", oauthHandler.HandleStart)
	mux.HandleFunc("/api/oauth/callback", oauthHandler.HandleCallback)

	// 代理路由：/v1/* 请求转发给插件
	proxy := &ProxyHandler{gateway: gw, store: store}
	mux.Handle("/v1/", proxy)

	// 静态文件（管理页面）
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("加载静态文件失败: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	log.Printf("devserver 启动: http://localhost%s", *addr)
	log.Printf("管理页面: http://localhost%s", *addr)
	log.Printf("代理端点: http://localhost%s/v1/", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("服务器启动失败: %v", err)
	}
}
