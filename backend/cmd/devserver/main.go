// devserver 插件开发服务器
// 用法: go run ./cmd/devserver [-addr :8080] [-data ./devdata]
package main

import (
	"log"
	"net/http"

	"github.com/DouDOU-start/airgate-sdk/devserver"

	"github.com/DouDOU-start/airgate-openai/backend/internal/gateway"
)

func main() {
	gw := &gateway.OpenAIGateway{}
	if err := devserver.Run(devserver.Config{
		Plugin: gw,
		ExtraRoutes: func(mux *http.ServeMux, store *devserver.AccountStore) {
			h := &gateway.OAuthDevHandler{Gateway: gw, Store: store}
			h.RegisterRoutes(mux)
		},
	}); err != nil {
		log.Fatal(err)
	}
}
