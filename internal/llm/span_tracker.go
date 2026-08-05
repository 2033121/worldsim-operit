package llm

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ---------- 环节级 Token 追踪（仿 langfuse Trace/Span：按"环节"聚合调用次数与 token） ----------
// 与 TaskTokenUsage（任务级、流式推送前端）互补：SpanTracker 按环节聚合，用于观察"钱花在哪个 Agent 上"
// 用法：调用 LLM 前 ctx = llm.WithSpan(ctx, "事件生成")，底层 HTTP 完成/失败时自动 RecordSpan

type spanCtxKey struct{}

// WithSpan 给 context 打上"环节名"标签（如"事件生成""NPC对话""记忆巩固""小说写手"）
func WithSpan(ctx context.Context, span string) context.Context {
	if ctx == nil || span == "" {
		return ctx
	}
	return context.WithValue(ctx, spanCtxKey{}, span)
}

func spanFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(spanCtxKey{}).(string)
	return v
}

// SpanUsage 单个环节的聚合用量
type SpanUsage struct {
	Calls      int `json:"calls"`             // 调用次数
	Prompt     int `json:"prompt_tokens"`     // 输入 token
	Completion int `json:"completion_tokens"` // 输出 token
	Total      int `json:"total_tokens"`
	Cached     int `json:"cached_tokens"` // 前缀缓存命中（成本约 1/10）
	Failures   int `json:"failures"`      // 失败/超时次数
}

// TokenTracker 全局环节追踪器（单例）
type TokenTracker struct {
	mu    sync.Mutex
	spans map[string]*SpanUsage
	start time.Time
}

var spanTracker = newTokenTracker()

func newTokenTracker() *TokenTracker {
	return &TokenTracker{spans: map[string]*SpanUsage{}, start: time.Now()}
}

// RecordSpan 记录一次调用的用量（在底层 HTTP 调用完成/失败处 defer 调用）
func RecordSpan(ctx context.Context, model string, usage *tokenUsage, promptRunes, completionRunes int, err error) {
	span := spanFrom(ctx)
	if span == "" {
		return // 未标注环节的调用不统计（零侵入）
	}
	spanTracker.mu.Lock()
	defer spanTracker.mu.Unlock()
	s := spanTracker.spans[span]
	if s == nil {
		s = &SpanUsage{}
		spanTracker.spans[span] = s
	}
	s.Calls++
	if err != nil {
		s.Failures++
		return
	}
	if usage != nil {
		s.Prompt += usage.PromptTokens
		s.Completion += usage.CompletionTokens
		s.Total += usage.TotalTokens
		// 缓存命中只取一个字段（OpenAI 系: prompt_tokens_details.cached_tokens；
		// DeepSeek 系: prompt_cache_hit_tokens）。同一模型响应通常只返回其一，
		// 若两个都返回则取较大的那个，避免重复累计导致命中率 >100%。
		cached := 0
		if usage.PromptTokensDetails != nil && usage.PromptTokensDetails.CachedTokens > cached {
			cached = usage.PromptTokensDetails.CachedTokens
		}
		if usage.PromptCacheHitTokens > cached {
			cached = usage.PromptCacheHitTokens
		}
		s.Cached += cached
	} else {
		// 无 usage 时按字数估算（与 TaskTokenUsage 口径一致）
		s.Prompt += EstimateTokensFromRunes(promptRunes)
		s.Completion += EstimateTokensFromRunes(completionRunes)
		s.Total += s.Prompt + s.Completion
	}
}

// SpanSnapshot 返回各环节用量（深拷贝，供 API/落盘）
func SpanSnapshot() map[string]*SpanUsage {
	spanTracker.mu.Lock()
	defer spanTracker.mu.Unlock()
	out := make(map[string]*SpanUsage, len(spanTracker.spans))
	for k, v := range spanTracker.spans {
		c := *v
		out[k] = &c
	}
	return out
}

// SpanSummary 总览：总调用/总 token/缓存命中率/各环节按消耗排序
func SpanSummary() map[string]any {
	spans := SpanSnapshot()
	totalCalls, totalPrompt, totalComp, totalCached := 0, 0, 0, 0
	for _, s := range spans {
		totalCalls += s.Calls
		totalPrompt += s.Prompt
		totalComp += s.Completion
		totalCached += s.Cached
	}
	type row struct {
		name string
		u    *SpanUsage
	}
	var rows []row
	for k, v := range spans {
		rows = append(rows, row{k, v})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].u.Total > rows[j].u.Total })
	top := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		top = append(top, map[string]any{
			"span": r.name, "calls": r.u.Calls,
			"prompt_tokens": r.u.Prompt, "completion_tokens": r.u.Completion,
			"total_tokens": r.u.Total, "cached_tokens": r.u.Cached, "failures": r.u.Failures,
		})
	}
	return map[string]any{
		"total_calls": totalCalls, "total_prompt_tokens": totalPrompt,
		"total_completion_tokens": totalComp, "total_tokens": totalPrompt + totalComp,
		"total_cached_tokens": totalCached,
		"cache_hit_rate":      fmt.Sprintf("%.1f%%", pct(totalCached, totalPrompt)),
		"spans":               top,
	}
}

func pct(a, b int) float64 {
	if b <= 0 {
		return 0
	}
	r := float64(a) / float64(b) * 100
	if r > 100 {
		return 100 // 命中率不可能超过100%（防御：估算token或模型异常数据）
	}
	return r
}
