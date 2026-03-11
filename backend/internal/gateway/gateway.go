package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	sdk "github.com/DouDOU-start/airgate-sdk"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
)

// OpenAIGateway OpenAI 网关插件（SimpleGatewayPlugin 实现）
// 核心自动处理调度、计费、限流、并发控制，插件只负责转发
type OpenAIGateway struct {
	logger *slog.Logger
	ctx    sdk.PluginContext
}

func (g *OpenAIGateway) Info() sdk.PluginInfo {
	return BuildPluginInfo()
}

func (g *OpenAIGateway) Init(ctx sdk.PluginContext) error {
	g.ctx = ctx
	if ctx != nil {
		g.logger = ctx.Logger()
	}
	if g.logger == nil {
		g.logger = slog.Default()
	}
	g.logger.Info("OpenAI 网关插件初始化")
	return nil
}

func (g *OpenAIGateway) Start(_ context.Context) error {
	g.logger.Info("OpenAI 网关插件启动")
	return nil
}

func (g *OpenAIGateway) Stop(_ context.Context) error {
	g.logger.Info("OpenAI 网关插件停止")
	return nil
}

func (g *OpenAIGateway) Platform() string {
	return PluginPlatform
}

func (g *OpenAIGateway) Models() []sdk.ModelInfo {
	return model.AllSpecs()
}

func (g *OpenAIGateway) Routes() []sdk.RouteDefinition {
	return PluginRouteDefinitions()
}

func (g *OpenAIGateway) Forward(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	return g.forwardHTTP(ctx, req)
}

// ValidateAccount 验证凭证有效性
func (g *OpenAIGateway) ValidateAccount(ctx context.Context, credentials map[string]string) error {
	apiKey := credentials["api_key"]
	accessToken := credentials["access_token"]

	if apiKey == "" && accessToken == "" {
		return fmt.Errorf("缺少 api_key 或 access_token")
	}

	// API Key 模式：调用 /v1/models 验证
	if apiKey != "" {
		account := &sdk.Account{Credentials: credentials}
		targetURL := buildAPIKeyURL(account, "/v1/models")
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
		if err != nil {
			return fmt.Errorf("构建验证请求失败: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)

		client := g.buildHTTPClient(account)
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("验证请求失败: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == 401 {
			return fmt.Errorf("API Key 无效")
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("API Key 验证失败: HTTP %d", resp.StatusCode)
		}
		return nil
	}

	// OAuth 模式：access_token 非空即通过
	return nil
}

// QueryQuota 查询账号额度（OpenAI 无标准额度查询接口）
func (g *OpenAIGateway) QueryQuota(_ context.Context, _ map[string]string) (*sdk.QuotaInfo, error) {
	return nil, sdk.ErrNotSupported
}
