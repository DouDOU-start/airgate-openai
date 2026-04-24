package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"

	"github.com/DouDOU-start/airgate-openai/backend/internal/gateway"
	sdk "github.com/DouDOU-start/airgate-sdk"
)

const generatedComment = "# 本文件由 backend/cmd/genmanifest 自动生成，请勿手工修改。\n\n"

type manifest struct {
	ID              string           `yaml:"id"`
	Name            string           `yaml:"name"`
	Version         string           `yaml:"version"`
	Description     string           `yaml:"description"`
	Author          string           `yaml:"author"`
	Type            string           `yaml:"type"`
	MinCoreVersion  string           `yaml:"min_core_version"`
	Dependencies    []string         `yaml:"dependencies"`
	Config          []configField    `yaml:"config,omitempty"`
	FrontendWidgets []frontendWidget `yaml:"frontend_widgets,omitempty"`
	Gateway         *gatewayManifest `yaml:"gateway,omitempty"`
}

type gatewayManifest struct {
	Platform     string        `yaml:"platform"`
	Mode         string        `yaml:"mode"`
	Routes       []routeDef    `yaml:"routes,omitempty"`
	Models       []modelInfo   `yaml:"models,omitempty"`
	AccountTypes []accountType `yaml:"account_types,omitempty"`
}

type routeDef struct {
	Method      string `yaml:"method"`
	Path        string `yaml:"path"`
	Description string `yaml:"description"`
}

type modelInfo struct {
	ID               string  `yaml:"id"`
	Name             string  `yaml:"name"`
	ContextWindow    int     `yaml:"context_window"`
	MaxOutputTokens  int     `yaml:"max_output_tokens"`
	InputPrice       float64 `yaml:"input_price"`
	OutputPrice      float64 `yaml:"output_price"`
	CachedInputPrice float64 `yaml:"cached_input_price,omitempty"`

	// Priority 档单价（$/1M tokens）
	InputPricePriority       float64 `yaml:"input_price_priority,omitempty"`
	OutputPricePriority      float64 `yaml:"output_price_priority,omitempty"`
	CachedInputPricePriority float64 `yaml:"cached_input_price_priority,omitempty"`

	// Fast 档单价（$/1M tokens）
	InputPriceFast       float64 `yaml:"input_price_fast,omitempty"`
	OutputPriceFast      float64 `yaml:"output_price_fast,omitempty"`
	CachedInputPriceFast float64 `yaml:"cached_input_price_fast,omitempty"`

	// Flex / Batch 档单价（$/1M tokens）
	InputPriceFlex       float64 `yaml:"input_price_flex,omitempty"`
	OutputPriceFlex      float64 `yaml:"output_price_flex,omitempty"`
	CachedInputPriceFlex float64 `yaml:"cached_input_price_flex,omitempty"`

	// 长上下文阶梯（仅 gpt-5.4 家族）
	LongContextThreshold        int     `yaml:"long_context_threshold,omitempty"`
	LongContextInputMultiplier  float64 `yaml:"long_context_input_multiplier,omitempty"`
	LongContextOutputMultiplier float64 `yaml:"long_context_output_multiplier,omitempty"`
	LongContextCachedMultiplier float64 `yaml:"long_context_cached_multiplier,omitempty"`
}

type accountType struct {
	Key         string            `yaml:"key"`
	Label       string            `yaml:"label"`
	Description string            `yaml:"description"`
	Fields      []credentialField `yaml:"fields,omitempty"`
}

type credentialField struct {
	Key         string `yaml:"key"`
	Label       string `yaml:"label"`
	Type        string `yaml:"type"`
	Required    bool   `yaml:"required"`
	Placeholder string `yaml:"placeholder,omitempty"`
}

type configField struct {
	Key         string `yaml:"key"`
	Type        string `yaml:"type"`
	Default     string `yaml:"default,omitempty"`
	Description string `yaml:"description,omitempty"`
	Required    bool   `yaml:"required"`
}

type frontendWidget struct {
	Slot      string `yaml:"slot"`
	EntryFile string `yaml:"entry_file"`
	Title     string `yaml:"title"`
}

func main() {
	content, err := renderManifest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "生成 manifest 失败: %v\n", err)
		os.Exit(1)
	}

	targetPath, err := manifestFilePath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "定位 plugin.yaml 失败: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(targetPath, content, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写入 plugin.yaml 失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("已生成 %s\n", targetPath)
}

func renderManifest() ([]byte, error) {
	plugin := &gateway.OpenAIGateway{}
	info := plugin.Info()

	doc := manifest{
		ID:              info.ID,
		Name:            info.Name,
		Version:         info.Version,
		Description:     info.Description,
		Author:          info.Author,
		Type:            string(info.Type),
		MinCoreVersion:  gateway.PluginMinCoreVersion,
		Dependencies:    gateway.PluginDependencies(),
		FrontendWidgets: convertFrontendWidgets(info.FrontendWidgets),
		Gateway: &gatewayManifest{
			Platform:     plugin.Platform(),
			Mode:         gateway.PluginMode,
			Routes:       convertRoutes(plugin.Routes()),
			Models:       convertModels(plugin.Models()),
			AccountTypes: convertAccountTypes(info.AccountTypes),
		},
	}

	var body bytes.Buffer
	encoder := yaml.NewEncoder(&body)
	encoder.SetIndent(2)
	if err := encoder.Encode(doc); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}

	content := append([]byte(generatedComment), body.Bytes()...)
	return content, nil
}

func manifestFilePath() (string, error) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("无法定位 genmanifest 源文件")
	}

	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", ".."))
	return filepath.Join(repoRoot, "plugin.yaml"), nil
}

func convertRoutes(routes []sdk.RouteDefinition) []routeDef {
	items := make([]routeDef, 0, len(routes))
	for _, route := range routes {
		items = append(items, routeDef{
			Method:      route.Method,
			Path:        route.Path,
			Description: route.Description,
		})
	}
	return items
}

func convertModels(models []sdk.ModelInfo) []modelInfo {
	items := make([]modelInfo, 0, len(models))
	for _, m := range models {
		items = append(items, modelInfo{
			ID:                          m.ID,
			Name:                        m.Name,
			ContextWindow:               m.ContextWindow,
			MaxOutputTokens:             m.MaxOutputTokens,
			InputPrice:                  m.InputPrice,
			OutputPrice:                 m.OutputPrice,
			CachedInputPrice:            m.CachedInputPrice,
			InputPricePriority:          m.InputPricePriority,
			OutputPricePriority:         m.OutputPricePriority,
			CachedInputPricePriority:    m.CachedInputPricePriority,
			InputPriceFast:              m.InputPriceFast,
			OutputPriceFast:             m.OutputPriceFast,
			CachedInputPriceFast:        m.CachedInputPriceFast,
			InputPriceFlex:              m.InputPriceFlex,
			OutputPriceFlex:             m.OutputPriceFlex,
			CachedInputPriceFlex:        m.CachedInputPriceFlex,
			LongContextThreshold:        m.LongContextThreshold,
			LongContextInputMultiplier:  m.LongContextInputMultiplier,
			LongContextOutputMultiplier: m.LongContextOutputMultiplier,
			LongContextCachedMultiplier: m.LongContextCachedMultiplier,
		})
	}
	return items
}

func convertAccountTypes(types []sdk.AccountType) []accountType {
	items := make([]accountType, 0, len(types))
	for _, item := range types {
		items = append(items, accountType{
			Key:         item.Key,
			Label:       item.Label,
			Description: item.Description,
			Fields:      convertCredentialFields(item.Fields),
		})
	}
	return items
}

func convertFrontendWidgets(widgets []sdk.FrontendWidget) []frontendWidget {
	items := make([]frontendWidget, 0, len(widgets))
	for _, w := range widgets {
		items = append(items, frontendWidget{
			Slot:      w.Slot,
			EntryFile: w.EntryFile,
			Title:     w.Title,
		})
	}
	return items
}

func convertCredentialFields(fields []sdk.CredentialField) []credentialField {
	items := make([]credentialField, 0, len(fields))
	for _, field := range fields {
		items = append(items, credentialField{
			Key:         field.Key,
			Label:       field.Label,
			Type:        field.Type,
			Required:    field.Required,
			Placeholder: field.Placeholder,
		})
	}
	return items
}
