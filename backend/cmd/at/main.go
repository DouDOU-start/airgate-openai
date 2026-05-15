// at is a small AirGate terminal for OpenAI accounts.
//
// Default mode prints the OpenAI account usage windows from AirGate Core, then
// starts an interactive Responses API chat through the same Core instance.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DouDOU-start/airgate-openai/backend/internal/gateway"
)

const (
	defaultBaseURL = "http://127.0.0.1:8080"
	defaultModel   = "gpt-5.3-codex"
)

type config struct {
	baseURL    string
	adminToken string
	apiKey     string
	model      string
	reasoning  string
	system     string
	proxy      string
	timeout    time.Duration
	rawJSON    bool
	noUsage    bool
	noChat     bool
}

type coreResponse struct {
	Code    int             `json:"code"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
}

type accountMeta struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Platform        string `json:"platform"`
	Type            string `json:"type"`
	State           string `json:"state"`
	ErrorMsg        string `json:"error_msg"`
	TodayImageCount *int64 `json:"today_image_count"`
}

type accountListData struct {
	List     []accountMeta `json:"list"`
	Total    int64         `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
}

type usageData struct {
	Accounts map[string]accountUsage `json:"accounts"`
}

type accountUsage struct {
	UpdatedAt  string           `json:"updated_at"`
	Windows    []usageWindow    `json:"windows"`
	Credits    *usageCredits    `json:"credits"`
	TodayStats *usageTodayStats `json:"today_stats"`
}

type usageWindow struct {
	Key               string  `json:"key"`
	Label             string  `json:"label"`
	UsedPercent       float64 `json:"used_percent"`
	ResetAt           string  `json:"reset_at"`
	ResetAfterSeconds int     `json:"reset_after_seconds"`
	ResetSeconds      int     `json:"reset_seconds"`
}

type usageCredits struct {
	Balance   float64 `json:"balance"`
	Unlimited bool    `json:"unlimited"`
}

type usageTodayStats struct {
	Requests    int64   `json:"requests"`
	Tokens      int64   `json:"tokens"`
	AccountCost float64 `json:"account_cost"`
	UserCost    float64 `json:"user_cost"`
}

type airgateSession struct {
	client   *http.Client
	cfg      *config
	history  []any
	cacheKey string
}

func main() {
	mode, args := splitMode(os.Args[1:])
	if mode == "help" {
		printUsage(os.Stdout)
		return
	}

	cfg, err := parseFlags(mode, args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	client := buildClient(cfg.proxy, cfg.timeout)
	ctx := context.Background()

	switch mode {
	case "usage":
		if err := requireAdminToken(cfg); err != nil {
			fatal(err)
		}
		if err := printUsageWindows(ctx, client, cfg); err != nil {
			fatal(err)
		}
	case "chat":
		if err := requireAPIKey(cfg); err != nil {
			fatal(err)
		}
		if err := runChat(ctx, client, cfg); err != nil {
			fatal(err)
		}
	default:
		ran := false
		if !cfg.noUsage {
			if cfg.adminToken == "" {
				fmt.Fprintln(os.Stderr, "[跳过用量窗口: 未设置 AIRGATE_ADMIN_TOKEN 或 AG_ADMIN_TOKEN]")
			} else if err := printUsageWindows(ctx, client, cfg); err != nil {
				fmt.Fprintf(os.Stderr, "[用量窗口查询失败: %v]\n", err)
			} else {
				ran = true
			}
		}
		if !cfg.noChat {
			if cfg.apiKey == "" {
				if ran {
					return
				}
				fatal(requireAPIKey(cfg))
			}
			if err := runChat(ctx, client, cfg); err != nil {
				fatal(err)
			}
		}
	}
}

func splitMode(args []string) (string, []string) {
	if len(args) == 0 {
		return "default", args
	}
	switch args[0] {
	case "usage", "chat", "help":
		return args[0], args[1:]
	default:
		return "default", args
	}
}

func parseFlags(mode string, args []string) (*config, error) {
	fs := flag.NewFlagSet("at", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	cfg := &config{}
	fs.StringVar(&cfg.baseURL, "base", firstEnv(defaultBaseURL, "AIRGATE_BASE_URL", "AG_BASE_URL"), "AirGate Core 根地址")
	fs.StringVar(&cfg.adminToken, "admin-token", firstEnv("", "AIRGATE_ADMIN_TOKEN", "AG_ADMIN_TOKEN", "AIRGATE_TOKEN", "AG_TOKEN"), "管理员 JWT 或 admin-xxx 管理员 API Key")
	fs.StringVar(&cfg.apiKey, "api-key", firstEnv("", "AIRGATE_API_KEY", "AG_API_KEY", "OPENAI_API_KEY"), "AirGate 用户 API Key，用于 /v1/responses 对话")
	fs.StringVar(&cfg.model, "model", firstEnv(defaultModel, "AIRGATE_MODEL", "AG_MODEL"), "对话模型")
	fs.StringVar(&cfg.reasoning, "reasoning", firstEnv("medium", "AIRGATE_REASONING", "AG_REASONING"), "Responses reasoning effort，空值表示不发送")
	fs.StringVar(&cfg.system, "system", firstEnv("", "AIRGATE_SYSTEM_PROMPT", "AG_SYSTEM_PROMPT"), "Responses instructions/system prompt")
	fs.StringVar(&cfg.proxy, "proxy", firstEnv("", "AIRGATE_PROXY", "AG_PROXY"), "HTTP/SOCKS 代理 URL")
	fs.DurationVar(&cfg.timeout, "timeout", 10*time.Minute, "单次 HTTP 请求超时")
	fs.BoolVar(&cfg.rawJSON, "json", false, "usage 模式输出原始 JSON")
	fs.BoolVar(&cfg.noUsage, "no-usage", false, "默认模式跳过用量窗口查询")
	fs.BoolVar(&cfg.noChat, "no-chat", false, "默认模式跳过交互式对话")
	fs.Usage = func() { printUsage(os.Stderr) }

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(os.Stdout)
			os.Exit(0)
		}
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("未知参数: %s", strings.Join(fs.Args(), " "))
	}
	if cfg.noUsage && cfg.noChat && mode == "default" {
		return nil, fmt.Errorf("默认模式不能同时指定 -no-usage 和 -no-chat")
	}
	cfg.baseURL = strings.TrimRight(strings.TrimSpace(cfg.baseURL), "/")
	if cfg.baseURL == "" {
		cfg.baseURL = defaultBaseURL
	}
	cfg.adminToken = strings.TrimSpace(cfg.adminToken)
	cfg.apiKey = strings.TrimSpace(cfg.apiKey)
	cfg.model = strings.TrimSpace(cfg.model)
	if cfg.model == "" {
		cfg.model = defaultModel
	}
	cfg.reasoning = strings.TrimSpace(cfg.reasoning)
	cfg.system = strings.TrimSpace(cfg.system)
	return cfg, nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "用法:")
	fmt.Fprintln(w, "  at [flags]          查询 OpenAI 账号用量窗口，然后进入交互式对话")
	fmt.Fprintln(w, "  at usage [flags]    只查询账号用量窗口")
	fmt.Fprintln(w, "  at chat [flags]     只启动交互式对话")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "常用环境变量:")
	fmt.Fprintln(w, "  AIRGATE_BASE_URL=http://127.0.0.1:8080")
	fmt.Fprintln(w, "  AIRGATE_ADMIN_TOKEN=admin-... 或管理员 JWT")
	fmt.Fprintln(w, "  AIRGATE_API_KEY=sk-...")
}

func requireAdminToken(cfg *config) error {
	if cfg.adminToken == "" {
		return fmt.Errorf("缺少管理员 Token：设置 AIRGATE_ADMIN_TOKEN/AG_ADMIN_TOKEN，或传 -admin-token")
	}
	return nil
}

func requireAPIKey(cfg *config) error {
	if cfg.apiKey == "" {
		return fmt.Errorf("缺少 AirGate API Key：设置 AIRGATE_API_KEY/AG_API_KEY，或传 -api-key")
	}
	return nil
}

func printUsageWindows(ctx context.Context, client *http.Client, cfg *config) error {
	usage, err := fetchUsage(ctx, client, cfg)
	if err != nil {
		return err
	}
	if cfg.rawJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(usage)
	}

	metas, metaErr := fetchAccountMeta(ctx, client, cfg)
	if metaErr != nil {
		fmt.Fprintf(os.Stderr, "[账号列表查询失败，仅显示 ID: %v]\n", metaErr)
	}
	printUsageTable(usage, metas)
	return nil
}

func fetchUsage(ctx context.Context, client *http.Client, cfg *config) (*usageData, error) {
	var out usageData
	if err := coreGet(ctx, client, cfg, "/api/v1/admin/accounts/usage?platform=openai", &out); err != nil {
		return nil, err
	}
	if out.Accounts == nil {
		out.Accounts = make(map[string]accountUsage)
	}
	return &out, nil
}

func fetchAccountMeta(ctx context.Context, client *http.Client, cfg *config) (map[int64]accountMeta, error) {
	result := make(map[int64]accountMeta)
	page := 1
	for {
		var data accountListData
		path := fmt.Sprintf("/api/v1/admin/accounts?platform=openai&page=%d&page_size=100", page)
		if err := coreGet(ctx, client, cfg, path, &data); err != nil {
			return result, err
		}
		for _, item := range data.List {
			result[item.ID] = item
		}
		if data.Total <= int64(len(result)) || len(data.List) == 0 {
			break
		}
		page++
	}
	return result, nil
}

func coreGet(ctx context.Context, client *http.Client, cfg *config, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(cfg.baseURL, path), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.adminToken)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var wrapped coreResponse
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Code == 0 && len(wrapped.Data) > 0 {
		return json.Unmarshal(wrapped.Data, out)
	}
	if wrapped.Code != 0 {
		msg := strings.TrimSpace(wrapped.Message)
		if msg == "" {
			msg = "core 返回错误"
		}
		return errors.New(msg)
	}
	return json.Unmarshal(body, out)
}

func printUsageTable(usage *usageData, metas map[int64]accountMeta) {
	ids := make([]int, 0, len(usage.Accounts)+len(metas))
	seen := make(map[int]bool, len(usage.Accounts)+len(metas))
	for key := range usage.Accounts {
		id, err := strconv.Atoi(key)
		if err != nil {
			continue
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	for id := range metas {
		i := int(id)
		if !seen[i] {
			ids = append(ids, i)
			seen[i] = true
		}
	}
	sort.Ints(ids)

	fmt.Println("OpenAI 账号用量窗口")
	if len(ids) == 0 {
		fmt.Println("  暂无账号")
		fmt.Println()
		return
	}
	for _, id := range ids {
		key := strconv.Itoa(id)
		item := usage.Accounts[key]
		meta := metas[int64(id)]
		name := meta.Name
		if name == "" {
			name = "#" + key
		}
		state := meta.State
		if state == "" {
			state = "-"
		}
		accountType := meta.Type
		if accountType == "" {
			accountType = "-"
		}

		fmt.Printf("- #%d %s [%s/%s]\n", id, name, state, accountType)
		if len(item.Windows) == 0 && item.Credits == nil {
			fmt.Println("  窗口: -")
		} else {
			for _, w := range item.Windows {
				fmt.Printf("  窗口: %s %s 重置 %s\n", compactLabel(w.Label), formatPercent(w.UsedPercent), formatReset(w))
			}
			if item.Credits != nil {
				if item.Credits.Unlimited {
					fmt.Println("  额度: ∞")
				} else {
					fmt.Printf("  额度: $%.2f\n", item.Credits.Balance)
				}
			}
		}
		if item.TodayStats != nil {
			fmt.Printf("  今日: %s 次 / %s tokens / 成本 $%.2f / 消费 $%.2f",
				formatCompact(item.TodayStats.Requests),
				formatCompact(item.TodayStats.Tokens),
				item.TodayStats.AccountCost,
				item.TodayStats.UserCost,
			)
			if meta.TodayImageCount != nil {
				fmt.Printf(" / 图 %s", formatCompact(*meta.TodayImageCount))
			}
			fmt.Println()
		}
		if meta.ErrorMsg != "" {
			fmt.Printf("  错误: %s\n", meta.ErrorMsg)
		}
	}
	fmt.Println()
}

func runChat(ctx context.Context, client *http.Client, cfg *config) error {
	session := &airgateSession{
		client:   client,
		cfg:      cfg,
		cacheKey: generateCacheKey(),
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	fmt.Printf("模型: %s | /usage 查询用量 | /clear 清空 | /model <name> 切换模型 | /exit 退出\n\n", cfg.model)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			fmt.Println()
			return scanner.Err()
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		switch {
		case input == "/exit" || input == "/quit":
			return nil
		case input == "/help":
			fmt.Println("/usage 查询用量 | /clear 清空 | /model <name> 切换模型 | /exit 退出")
		case input == "/clear":
			session.history = nil
			session.cacheKey = generateCacheKey()
			fmt.Println("对话已清空")
		case input == "/usage":
			if cfg.adminToken == "" {
				fmt.Println("未设置 AIRGATE_ADMIN_TOKEN/AG_ADMIN_TOKEN，无法查询账号用量窗口")
				continue
			}
			if err := printUsageWindows(ctx, client, cfg); err != nil {
				fmt.Fprintf(os.Stderr, "用量窗口查询失败: %v\n", err)
			}
		case strings.HasPrefix(input, "/model "):
			model := strings.TrimSpace(strings.TrimPrefix(input, "/model "))
			if model == "" {
				fmt.Println("用法: /model <name>")
				continue
			}
			cfg.model = model
			session.history = nil
			session.cacheKey = generateCacheKey()
			fmt.Printf("模型已切换为 %s，对话历史已清空\n", cfg.model)
		default:
			if err := session.chat(input); err != nil {
				fmt.Fprintf(os.Stderr, "\n错误: %v\n", err)
			}
			fmt.Println()
		}
	}
}

func (s *airgateSession) chat(input string) error {
	userMsg := buildUserMsg(input)
	allInput := make([]any, 0, len(s.history)+1)
	allInput = append(allInput, s.history...)
	allInput = append(allInput, userMsg)

	reqBody := map[string]any{
		"model":            s.cfg.model,
		"input":            allInput,
		"stream":           true,
		"store":            false,
		"prompt_cache_key": s.cacheKey,
	}
	if s.cfg.system != "" {
		reqBody["instructions"] = s.cfg.system
	}
	if s.cfg.reasoning != "" && s.cfg.reasoning != "none" {
		reqBody["reasoning"] = map[string]any{
			"effort":  s.cfg.reasoning,
			"summary": "auto",
		}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, joinURL(s.cfg.baseURL, "/v1/responses"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	handler := &terminalHandler{}
	result := gateway.ParseSSEStream(resp.Body, handler)

	s.history = append(s.history, userMsg)
	if result.Text != "" {
		s.history = append(s.history, buildAssistantMsg(result.Text))
	}
	printStats(result.Model, result.InputTokens, result.OutputTokens, result.CachedInputTokens, result.Duration)
	return result.Err
}

type terminalHandler struct{}

func (h *terminalHandler) OnTextDelta(delta string) {
	fmt.Print(delta)
}

func (h *terminalHandler) OnReasoningDelta(delta string) {
	fmt.Fprintf(os.Stderr, "\033[2m%s\033[0m", delta)
}

func (h *terminalHandler) OnRawEvent(string, []byte) {}

func (h *terminalHandler) OnRateLimits(used float64) {
	if used > 80 {
		fmt.Fprintf(os.Stderr, "\n[速率限制: %.1f%%]", used)
	}
}

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
		if model == "" {
			model = "-"
		}
		fmt.Fprintf(os.Stderr, "\n[%s | 输入: %d 输出: %d 缓存: %d | %s]",
			model, input, output, cache, duration.Round(time.Millisecond))
	}
}

func generateCacheKey() string {
	return "at-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func buildClient(proxy string, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
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
	return &http.Client{Transport: transport, Timeout: timeout}
}

func firstEnv(fallback string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return fallback
}

func joinURL(baseURL, path string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = defaultBaseURL
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

func compactLabel(label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return "-"
	}
	return label
}

func formatPercent(value float64) string {
	return fmt.Sprintf("%.0f%%", value)
}

func formatReset(w usageWindow) string {
	seconds := w.ResetAfterSeconds
	if seconds <= 0 {
		seconds = w.ResetSeconds
	}
	if seconds <= 0 && w.ResetAt != "" {
		if t, err := time.Parse(time.RFC3339, w.ResetAt); err == nil {
			seconds = int(time.Until(t).Seconds())
		}
	}
	if seconds <= 0 {
		return "-"
	}
	return formatDuration(time.Duration(seconds) * time.Second)
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	hours := int(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	minutes := int(d / time.Minute)
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0 && minutes > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

func formatCompact(n int64) string {
	abs := n
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	case abs >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case abs >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}
