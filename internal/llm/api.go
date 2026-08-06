package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
	"worldsim/internal/config"
	"worldsim/internal/logx"
	"worldsim/internal/sse"
)

// newHTTPClient 构建 HTTP 客户端。
// 关键：禁用 HTTP/2 和连接复用——Android/proot 环境的网络栈对 HTTP/2 长连接支持有 bug
// （表现为 read tcp ... software caused connection abort），HTTP/1.1 + 每次新建连接最稳。
// 另：必须显式设置 Dial/TLS/ResponseHeader 各阶段超时——Go 默认无这些超时，挂起时会无限等
// （网络栈丢包时 TCP 连接可能卡几分钟，直到上层 context 超时）。各阶段短超时让失败快速暴露。
func newHTTPClient(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		// 禁用 HTTP/2（ALPN 不协商 h2），走 HTTP/1.1——Android proot 下 h2 长连接会被 abort
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		// 禁用连接池：每次请求新建 TCP 连接（中转站响应快，连接开销可忽略；避免复用挂掉的连接）
		DisableKeepAlives:   true,
		MaxIdleConns:        0,
		MaxIdleConnsPerHost: 0,
		ForceAttemptHTTP2:   false,
		// 各阶段硬超时：TCP 连接 10s、TLS 握手 10s（连接阶段快速失败，防 proot 网络栈挂起）
		// 等响应头 300s：推理模型（deepseek-v4-flash-0731）处理大 prompt（事件生成 30KB+）时
		// 思考 6000+ tokens 需要 150-250s 才输出第一个字节，超时太短会误杀正常请求。
		// 网络层挂起由上层 context（CompleteTierTimeout）控制总时长，这里给生成留足空间。
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

type ChatRequest struct {
	Model         string         `json:"model"`
	Messages      []Message      `json:"messages"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	// ReasoningEffort 推理深度控制：""（默认）| "low"（低，省 reasoning token/提速）。
	// 事件生成这类"需要输出稳定 JSON、但思考太重"的场景用 low——completion 可省 50%+，
	// 且输出更干净（默认档常带 ```json 包裹，需额外剥离）。复杂多目标规划保持默认。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// llmSem 全局 LLM 并发闸门：所有 HTTP 调用（流式/同步）共用，
// 限制同时打到中转站的请求数，防止并行 Agent（角色档案/事件生成等）把上游并发打爆（429）。
var llmSem = make(chan struct{}, 3)

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type tokenUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens,omitempty"`
}

// ---------- 前缀缓存统计（DeepSeek 自动缓存：相同前缀命中，成本约 1/10） ----------
var (
	cacheCalls atomic.Int64 // 总调用次数
	cacheHits  atomic.Int64 // 命中缓存token数
	cacheMiss  atomic.Int64 // 未命中token数
)

// RecordCacheUsage 记录一次调用的前缀缓存命中情况
func RecordCacheUsage(cached, miss int) {
	if cached <= 0 && miss <= 0 {
		return
	}
	cacheCalls.Add(1)
	cacheHits.Add(int64(cached))
	cacheMiss.Add(int64(miss))
}

// CacheStats 返回缓存命中统计（用于日志/状态接口）
func CacheStats() string {
	calls := cacheCalls.Load()
	if calls == 0 {
		return "前缀缓存：尚无数据"
	}
	h := cacheHits.Load()
	m := cacheMiss.Load()
	total := h + m
	rate := 0.0
	if total > 0 {
		rate = float64(h) / float64(total) * 100
	}
	return fmt.Sprintf("前缀缓存：%d次调用 | 命中 %d token (%.1f%%) | 未命中 %d token", calls, h, rate, m)
}

type Message struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"` // 推理模型思考过程（正文为空时兜底）
}

type ChatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage *tokenUsage `json:"usage,omitempty"`
}

// CompletionResult is the normalized result of a chat completion call.
type CompletionResult struct {
	Content          string
	ReasoningContent string // 推理模型的思考过程（若有，正文之外另存，不混入正文）
	FinishReason     string // e.g. "stop", "length"
}

func hasAPIVersionSegment(u string) bool {
	for _, seg := range strings.Split(u, "/") {
		if len(seg) >= 2 && seg[0] == 'v' && seg[1] >= '0' && seg[1] <= '9' {
			return true
		}
	}
	return false
}

// resolveChatCompletionsURL builds the POST endpoint from base_url and url_strict.
// Must stay in sync with frontend/src/lib/apiUrl.js.
func resolveChatCompletionsURL(base string, strict bool) string {
	base = strings.TrimSpace(base)
	base = strings.TrimSuffix(base, "/")
	if base == "" {
		return ""
	}
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	if strict {
		return base + "/chat/completions"
	}
	if hasAPIVersionSegment(base) {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

func resolveAPIBase(base string, strict bool) string {
	u := resolveChatCompletionsURL(base, strict)
	return strings.TrimSuffix(u, "/chat/completions")
}

func normalizeURL(apiCfg *config.APIConfig) string {
	if apiCfg == nil {
		return ""
	}
	return resolveChatCompletionsURL(apiCfg.BaseURL, apiCfg.URLStrict)
}

// EnsureContextBudget fills ContextBudgetTokens when unset: it tries the
// model's real context window first, then falls back to the default.
func EnsureContextBudget(apiCfg *config.APIConfig) {
	if apiCfg == nil || apiCfg.ContextBudgetTokens > 0 {
		return
	}
	if window := FetchModelContextWindow(apiCfg); window > 0 {
		apiCfg.ContextBudgetTokens = window
	} else {
		apiCfg.ContextBudgetTokens = config.DefaultContextBudgetTokens
	}
}

// FetchModelContextWindow 从 API 的 /models 端点获取指定模型的上下文窗口大小。
// 成功返回 context_window > 0，失败返回 0（调用方应使用默认值）。
func FetchModelContextWindow(apiCfg *config.APIConfig) int {
	if apiCfg == nil || strings.TrimSpace(apiCfg.BaseURL) == "" || strings.TrimSpace(apiCfg.Model) == "" {
		return 0
	}
	modelsURL := resolveAPIBase(apiCfg.BaseURL, apiCfg.URLStrict) + "/models/" + apiCfg.Model

	req, err := http.NewRequest("GET", modelsURL, nil)
	if err != nil {
		return 0
	}
	if apiCfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiCfg.APIKey)
	}

	client := newHTTPClient(10 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0
	}

	var result struct {
		ContextWindow int `json:"context_window"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.ContextWindow <= 0 {
		return 0
	}
	return result.ContextWindow
}

func ValidateConfig(apiCfg *config.APIConfig) error {
	if strings.TrimSpace(apiCfg.BaseURL) == "" {
		return fmt.Errorf("API Base URL 未配置")
	}
	if strings.TrimSpace(apiCfg.Model) == "" {
		return fmt.Errorf("Model 未配置")
	}
	return nil
}

func IsFatalAPIError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// 注意：不要把所有 "dial tcp" 都当作致命错误——
	// "dial tcp ... i/o timeout" 等临时网络故障应当重试。
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") {
		return true
	}
	if strings.Contains(msg, "状态码: 401") ||
		strings.Contains(msg, "状态码: 403") ||
		strings.Contains(msg, "状态码: 404") {
		return true
	}
	if strings.Contains(msg, "context canceled") {
		return true
	}
	return false
}

func CallAPI(ctx context.Context, apiCfg *config.APIConfig, system, user string) (string, error) {
	return CallAPIMessages(ctx, apiCfg, []Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	})
}

// CallAPITier 按模型分层档位调用（fast/normal/premium），未配置档位则用默认 Model
func CallAPITier(ctx context.Context, apiCfg *config.APIConfig, tier, system, user string) (string, error) {
	cfg := apiCfg
	if tier != "" && apiCfg != nil && apiCfg.TierModel(tier) != apiCfg.Model {
		cfg = apiCfg.TieredConfig(tier)
	}
	return CallAPIMessages(ctx, cfg, []Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	})
}

// CallAPITierSync 同步版分层调用（长文/推理模型用，避免流式截断）
func CallAPITierSync(ctx context.Context, apiCfg *config.APIConfig, tier, system, user string) (string, error) {
	res, err := CallAPITierSyncResult(ctx, apiCfg, tier, system, user)
	if err != nil {
		return "", err
	}
	return res.Content, nil
}

// CallAPITierSyncResult 同步版分层调用，返回完整结果（含 ReasoningContent 思考通道，正文之外另存）
func CallAPITierSyncResult(ctx context.Context, apiCfg *config.APIConfig, tier, system, user string) (CompletionResult, error) {
	cfg := apiCfg
	if tier != "" && apiCfg != nil && apiCfg.TierModel(tier) != apiCfg.Model {
		cfg = apiCfg.TieredConfig(tier)
	}
	start := time.Now()
	res, err := CallAPIMessagesSync(ctx, cfg, []Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	})
	// 健康指标采集（运行检测 / 自动修复用）
	ms := time.Since(start).Milliseconds()
	if logx.M() != nil {
		if err != nil {
			logx.M().RecordLLM(false, ms, 0, logx.Trunc(err.Error(), 120))
		} else {
			logx.M().RecordLLM(true, ms, 0, "")
		}
	}
	if err != nil {
		return CompletionResult{}, err
	}
	return res, nil
}

// CallAPIMessages 以完整的多轮消息数组调用 API。
// 内部优先走流式并缓冲全文，使 token 计数在等待期间也能更新；流式不可用时回退同步请求。
func CallAPIMessages(ctx context.Context, apiCfg *config.APIConfig, messages []Message) (string, error) {
	result, err := CallAPIStreamMessages(ctx, apiCfg, messages, nil)
	if err == nil && result.Content != "" {
		return result.Content, nil
	}
	if ctx.Err() != nil {
		if result.Content != "" {
			return result.Content, ctx.Err()
		}
		return "", ctx.Err()
	}
	if result.Content != "" {
		return result.Content, err
	}
	if err != nil && IsFatalAPIError(err) {
		return "", err
	}
	// ponytail: fallback for providers with broken stream; loses finish_reason + stream estimate.
	syncResult, syncErr := CallAPIMessagesSync(ctx, apiCfg, messages)
	return syncResult.Content, syncErr
}

// CallAPIMessagesSync 同步 HTTP 调用（仅作流式失败时的回退）。
func CallAPIMessagesSync(ctx context.Context, apiCfg *config.APIConfig, messages []Message) (res CompletionResult, err error) {
	llmSem <- struct{}{} // 全局并发闸门（与流式共用）
	defer func() { <-llmSem }()
	fullURL := normalizeURL(apiCfg)
	tracker := TaskTokensFromContext(ctx)
	tracker.beginCall(messages)
	var lastUsage *tokenUsage
	defer func() {
		// 环节级用量记录（span 从 ctx 取，未标注则不统计；失败也计 Failures）
		RecordSpan(ctx, apiCfg.Model, lastUsage, countMessageRunes(messages), utf8.RuneCountInString(res.Content), err)
	}()

	reqBody := ChatRequest{
		Model:           apiCfg.Model,
		Messages:        messages,
		MaxTokens:       apiCfg.MaxTokens,
		ReasoningEffort: apiCfg.ReasoningEffort,
	}

	bts, err := json.Marshal(reqBody)
	if err != nil {
		return CompletionResult{}, err
	}
	// 诊断：确认 reasoning_effort 是否生效（事件生成 low / 其他默认空）
	if apiCfg.ReasoningEffort != "" {
		fmt.Printf(" [LLM] reasoning_effort=%s model=%s prompt=%d字符\n", apiCfg.ReasoningEffort, apiCfg.Model, len(bts))
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fullURL, bytes.NewBuffer(bts))
	if err != nil {
		return CompletionResult{}, err
	}

	req.Header.Set("Content-Type", "application/json")
	if apiCfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiCfg.APIKey)
	}

	timeout := time.Duration(apiCfg.HTTPTimeoutSeconds) * time.Second
	client := newHTTPClient(timeout)
	// Do 用 goroutine + select 双保险：proot 环境下 Go net/http 的 writeLoop 可能在
	// TCP 写死锁时卡死，context.WithTimeout 无法打断 syscall.Write（不可中断阻塞）。
	// 这里在 timeout 到点后强制返回错误并关闭连接，避免请求无限卡死。
	type doResult struct {
		resp *http.Response
		err  error
	}
	doCh := make(chan doResult, 1)
	go func() {
		resp, err := client.Do(req)
		doCh <- doResult{resp: resp, err: err}
	}()
	var resp *http.Response
	select {
	case r := <-doCh:
		resp, err = r.resp, r.err
	case <-time.After(timeout + 5*time.Second):
		return CompletionResult{}, fmt.Errorf("API 请求超时（client.Do 卡死 %ds 强制中断）：写循环死锁", int(timeout.Seconds()))
	case <-ctx.Done():
		return CompletionResult{}, fmt.Errorf("API 请求上下文取消：%v", ctx.Err())
	}
	if err != nil {
		return CompletionResult{}, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return CompletionResult{}, err
	}

	if resp.StatusCode != http.StatusOK {
		return CompletionResult{}, fmt.Errorf("API 响应错误，状态码: %d, 返回内容: %s", resp.StatusCode, string(bodyBytes))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(bodyBytes, &chatResp); err != nil {
		return CompletionResult{}, err
	}

	if len(chatResp.Choices) > 0 {
		content := chatResp.Choices[0].Message.Content
		reasoning := chatResp.Choices[0].Message.ReasoningContent
		// 推理模型兜底：正文为空但思考内容存在时，用思考内容回退（避免空手）
		if strings.TrimSpace(content) == "" && reasoning != "" {
			content = reasoning
			reasoning = ""
		}
		if chatResp.Usage != nil {
			lastUsage = chatResp.Usage // 供 defer 环节级记录
			if tracker != nil {
				tracker.finishCall(chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens, true, messages, content)
			}
			// 前缀缓存统计（独立于 tracker：有 usage 就记录）
			cached := 0
			if chatResp.Usage.PromptTokensDetails != nil {
				cached = chatResp.Usage.PromptTokensDetails.CachedTokens
			}
			if chatResp.Usage.PromptCacheHitTokens > cached {
				cached = chatResp.Usage.PromptCacheHitTokens
			}
			RecordCacheUsage(cached, chatResp.Usage.PromptTokens-cached)
		} else if tracker != nil {
			tracker.finishCall(0, 0, false, messages, content)
		}
		return CompletionResult{Content: content, ReasoningContent: reasoning, FinishReason: chatResp.Choices[0].FinishReason}, nil
	}
	return CompletionResult{}, fmt.Errorf("接口未响应有效 Choices 文本")
}

func CallAPIWithRetry(ctx context.Context, apiCfg *config.APIConfig, system, user string) string {
	retryCount := 0
	for {
		if ctx.Err() != nil {
			return ""
		}
		result, err := CallAPI(ctx, apiCfg, system, user)
		if err == nil && result != "" {
			return result
		}
		if IsFatalAPIError(err) {
			fmt.Printf(" ❌ [致命错误] %v，不再重试\n", err)
			return ""
		}

		retryCount++
		waitTime := RetryWaitTime(retryCount)
		fmt.Printf(" ⚠️ [错误] API调用失败: %v。第 %d 次重试，等待 %ds 后重试...\n", err, retryCount, waitTime)
		select {
		case <-time.After(time.Duration(waitTime) * time.Second):
		case <-ctx.Done():
			return ""
		}
	}
}

func CallAPIWithRetryLog(ctx context.Context, apiCfg *config.APIConfig, system, user string, logger *sse.LogBroadcaster) string {
	retryCount := 0
	for {
		if ctx.Err() != nil {
			return ""
		}
		result, err := CallAPI(ctx, apiCfg, system, user)
		if err == nil && result != "" {
			return result
		}
		if IsFatalAPIError(err) {
			logger.ErrorKey("log.fatal_no_retry", err)
			return ""
		}

		retryCount++
		waitTime := RetryWaitTime(retryCount)
		logger.WarnKey("log.api_retry", err, retryCount, waitTime)
		select {
		case <-time.After(time.Duration(waitTime) * time.Second):
		case <-ctx.Done():
			return ""
		}
	}
}

func RetryWaitTime(retry int) int {
	if retry > 6 {
		return 30
	}
	return retry * 5
}

type streamDelta struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *tokenUsage `json:"usage,omitempty"`
}

func CallAPIStream(ctx context.Context, apiCfg *config.APIConfig, system, user string, onChunk func(string)) (string, error) {
	result, err := CallAPIStreamMessages(ctx, apiCfg, []Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}, onChunk)
	return result.Content, err
}

// CallAPIStreamMessages 以完整的多轮消息数组调用 API（流式）。
func CallAPIStreamMessages(ctx context.Context, apiCfg *config.APIConfig, messages []Message, onChunk func(string)) (res CompletionResult, err error) {
	llmSem <- struct{}{} // 全局并发闸门：限制同时打到中转站的请求数，防止并行 Agent 调用打爆上游
	defer func() { <-llmSem }()
	fullURL := normalizeURL(apiCfg)
	tracker := TaskTokensFromContext(ctx)
	tracker.beginCall(messages)
	var streamUsage *tokenUsage
	defer func() {
		// 环节级用量记录（span 从 ctx 取，未标注则不统计；失败也计 Failures）
		RecordSpan(ctx, apiCfg.Model, streamUsage, countMessageRunes(messages), utf8.RuneCountInString(res.Content), err)
	}()

	reqBody := ChatRequest{
		Model:           apiCfg.Model,
		Messages:        messages,
		Stream:          true,
		StreamOptions:   &streamOptions{IncludeUsage: true},
		MaxTokens:       apiCfg.MaxTokens,
		ReasoningEffort: apiCfg.ReasoningEffort,
	}

	bts, err := json.Marshal(reqBody)
	if err != nil {
		return CompletionResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fullURL, bytes.NewBuffer(bts))
	if err != nil {
		return CompletionResult{}, err
	}

	req.Header.Set("Content-Type", "application/json")
	if apiCfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiCfg.APIKey)
	}

	timeout := time.Duration(apiCfg.HTTPTimeoutSeconds) * time.Second
	client := newHTTPClient(timeout)
	resp, err := client.Do(req)
	if err != nil {
		return CompletionResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return CompletionResult{}, fmt.Errorf("API 响应错误，状态码: %d, 返回内容: %s", resp.StatusCode, string(bodyBytes))
	}

	var fullContent strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	var finishReason string

	for scanner.Scan() {
		if ctx.Err() != nil {
			return CompletionResult{Content: fullContent.String(), FinishReason: finishReason}, ctx.Err()
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var delta streamDelta
		if err := json.Unmarshal([]byte(data), &delta); err != nil {
			continue
		}
		if delta.Usage != nil {
			streamUsage = delta.Usage
		}
		if len(delta.Choices) > 0 {
			if delta.Choices[0].FinishReason != "" {
				finishReason = delta.Choices[0].FinishReason
			}
			if delta.Choices[0].Delta.Content != "" {
				chunk := delta.Choices[0].Delta.Content
				fullContent.WriteString(chunk)
				if tracker != nil {
					tracker.updateStreamContent(fullContent.String())
				}
				if onChunk != nil {
					onChunk(chunk)
				}
			}
		}
	}

	result := fullContent.String()
	if result == "" {
		return CompletionResult{}, fmt.Errorf("流式响应为空")
	}
	if streamUsage != nil {
		if tracker != nil {
			tracker.finishCall(streamUsage.PromptTokens, streamUsage.CompletionTokens, true, messages, result)
		}
		// 前缀缓存统计（流式，独立于 tracker）
		cached := 0
		if streamUsage.PromptTokensDetails != nil {
			cached = streamUsage.PromptTokensDetails.CachedTokens
		}
		if streamUsage.PromptCacheHitTokens > cached {
			cached = streamUsage.PromptCacheHitTokens
		}
		RecordCacheUsage(cached, streamUsage.PromptTokens-cached)
	} else if tracker != nil {
		tracker.finishCall(0, 0, false, messages, result)
	}
	return CompletionResult{Content: result, FinishReason: finishReason}, nil
}
