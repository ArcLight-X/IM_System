package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"server-demo/internal/llm"
)

const (
	// 默认系统提示词
	defaultSystemPrompt = `你是 IM 聊天室里的 AI 助手。回答要简洁、直接，默认使用中文。`
	// 摘要任务的系统提示词
	summarySystemPrompt = `你是一个群聊摘要助手。请用不超过 5 条要点总结给定的聊天记录，` +
		`突出结论、待办与未解决的问题；只依据记录内容，不要编造不存在的信息。`
	polishSystemPrompt = `请润色用户的聊天草稿，保持原意和语气，语言自然简洁。只输出润色后的文本，不要解释。`

	defaultMaxContextTokens = 2000
	// 无论预算多紧，都给最后一条消息留出空间
	minMessageTokens = 32
	// 每条消息的固定开销（role 等）
	messageOverhead = 4
)

var (
	// ErrRateLimited 表示该用户请求过于频繁。
	ErrRateLimited = errors.New("ai: rate limited")
	// ErrClientNotConfigured 表示未配置模型客户端。
	ErrClientNotConfigured = errors.New("ai: client not configured")
	// ErrEmptyQuestion 表示问题为空。
	ErrEmptyQuestion = errors.New("ai: empty question")
)

// Stats 记录 AI 调用统计，用于观察缓存命中率、降级次数与 token 消耗。
type Stats struct {
	Requests     int
	CacheHits    int
	Fallbacks    int
	Errors       int
	Retries      int
	RateLimited  int
	PromptTokens int
	Latency      time.Duration
}

type cacheEntry struct {
	value string
	at    time.Time
}

// AIHandler 负责 prompt 组装、上下文裁剪、超时重试、结果缓存与限流。
// 它不认识 TCP 连接，因此可以脱离网络单独测试。
type AIHandler struct {
	client           llm.Client
	systemPrompt     string
	timeout          time.Duration
	retries          int
	maxContextTokens int
	ratePerMinute    int
	cacheTTL         time.Duration
	cacheSize        int
	fallback         string
	now              func() time.Time

	sem chan struct{}

	mu     sync.Mutex
	cache  map[string]cacheEntry
	recent map[string][]time.Time
	stats  Stats
}

// AIOption 用于配置 AIHandler。
type AIOption func(*AIHandler)

func WithSystemPrompt(s string) AIOption {
	return func(h *AIHandler) {
		if strings.TrimSpace(s) != "" {
			h.systemPrompt = s
		}
	}
}

func WithTimeout(d time.Duration) AIOption {
	return func(h *AIHandler) {
		if d > 0 {
			h.timeout = d
		}
	}
}

func WithRetries(n int) AIOption {
	return func(h *AIHandler) {
		if n >= 0 {
			h.retries = n
		}
	}
}

func WithMaxContextTokens(n int) AIOption {
	return func(h *AIHandler) {
		if n > 0 {
			h.maxContextTokens = n
		}
	}
}

func WithConcurrency(n int) AIOption {
	return func(h *AIHandler) {
		if n > 0 {
			h.sem = make(chan struct{}, n)
		}
	}
}

func WithRateLimit(perMinute int) AIOption {
	return func(h *AIHandler) { h.ratePerMinute = perMinute }
}

func WithCache(ttl time.Duration, size int) AIOption {
	return func(h *AIHandler) {
		h.cacheTTL = ttl
		h.cacheSize = size
	}
}

func WithFallback(s string) AIOption {
	return func(h *AIHandler) {
		if s != "" {
			h.fallback = s
		}
	}
}

// WithClock 便于在测试中控制时间流逝。
func WithClock(f func() time.Time) AIOption {
	return func(h *AIHandler) {
		if f != nil {
			h.now = f
		}
	}
}

// NewAIHandler 创建 AI 处理器；client 允许为 nil，此时所有请求都会走降级回复。
func NewAIHandler(client llm.Client, opts ...AIOption) *AIHandler {
	h := &AIHandler{
		client:           client,
		systemPrompt:     defaultSystemPrompt,
		timeout:          20 * time.Second,
		retries:          1,
		maxContextTokens: defaultMaxContextTokens,
		ratePerMinute:    10,
		cacheTTL:         5 * time.Minute,
		cacheSize:        128,
		fallback:         "AI 服务暂时不可用，请稍后再试。",
		now:              time.Now,
		sem:              make(chan struct{}, 8),
		cache:            make(map[string]cacheEntry),
		recent:           make(map[string][]time.Time),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Ask 回答用户提问，history 是最近的聊天记录，用于提供上下文。
// 返回的字符串一定可以直接展示给用户：失败时返回降级文案，同时返回错误供日志记录。
func (h *AIHandler) Ask(ctx context.Context, userID, question string, history []llm.Message) (string, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return "", ErrEmptyQuestion
	}
	msgs := buildMessages(h.systemPrompt, history, question, h.maxContextTokens)
	// 相同问题在不同用户或不同聊天上下文中，不能复用彼此的回答。
	var key strings.Builder
	key.WriteString(userID)
	for _, msg := range msgs {
		key.WriteByte(0)
		key.WriteString(msg.Role)
		key.WriteByte(0)
		key.WriteString(msg.Content)
	}
	return h.complete(ctx, userID, cacheKey("ask", key.String()), msgs)
}

// Summarize 总结聊天记录。
func (h *AIHandler) Summarize(ctx context.Context, userID string, lines []string) (string, error) {
	if len(lines) == 0 {
		return "", errors.New("ai: empty history")
	}
	joined := truncateRunes(strings.Join(lines, "\n"), 4000)
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: summarySystemPrompt},
		{Role: llm.RoleUser, Content: "请总结以下聊天记录：\n" + joined},
	}
	return h.complete(ctx, userID, cacheKey("summary", joined), msgs)
}

// Polish 润色一条尚未发送的聊天草稿，不使用群聊历史。
func (h *AIHandler) Polish(ctx context.Context, userID, draft string) (string, error) {
	draft = strings.TrimSpace(draft)
	if draft == "" {
		return "", ErrEmptyQuestion
	}
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: polishSystemPrompt},
		{Role: llm.RoleUser, Content: draft},
	}
	return h.complete(ctx, userID, cacheKey("polish", draft), msgs)
}

// Stats 返回统计快照。
func (h *AIHandler) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stats
}

func (h *AIHandler) complete(ctx context.Context, userID, key string, msgs []llm.Message) (string, error) {
	if h.client == nil {
		return h.fallbackText(), ErrClientNotConfigured
	}

	// 命中缓存不消耗模型额度，因此在限流之前判断
	if key != "" {
		if v, ok := h.cacheGet(key); ok {
			h.bump(func(s *Stats) { s.CacheHits++ })
			return v, nil
		}
	}

	if err := h.allow(userID); err != nil {
		return h.fallbackText(), err
	}
	if err := h.acquire(ctx); err != nil {
		return h.fallbackText(), err
	}
	defer h.release()

	start := h.now()
	promptTokens := estimateMessages(msgs)
	h.bump(func(s *Stats) { s.Requests++ })

	attempts := h.retries + 1
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			h.bump(func(s *Stats) { s.Retries++ })
			select {
			case <-time.After(time.Duration(i) * 200 * time.Millisecond):
			case <-ctx.Done():
				return h.failed(ctx.Err())
			}
		}

		callCtx, cancel := context.WithTimeout(ctx, h.timeout)
		out, err := h.client.Complete(callCtx, msgs)
		cancel()
		if err == nil {
			h.bump(func(s *Stats) {
				s.PromptTokens += promptTokens
				s.Latency += h.now().Sub(start)
			})
			if key != "" {
				h.cachePut(key, out)
			}
			return out, nil
		}
		lastErr = err
		if !retryable(err) {
			break
		}
	}
	return h.failed(lastErr)
}

func (h *AIHandler) failed(err error) (string, error) {
	if err == nil {
		err = errors.New("ai: request failed")
	}
	h.bump(func(s *Stats) {
		s.Errors++
		s.Fallbacks++
	})
	return h.fallbackText(), err
}

func (h *AIHandler) fallbackText() string {
	if h.fallback != "" {
		return h.fallback
	}
	return "AI 服务暂时不可用。"
}

// allow 实现每用户滑动窗口限流。
func (h *AIHandler) allow(userID string) error {
	if h.ratePerMinute <= 0 || userID == "" {
		return nil
	}
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()

	cut := now.Add(-time.Minute)
	kept := h.recent[userID][:0]
	for _, t := range h.recent[userID] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= h.ratePerMinute {
		h.recent[userID] = kept
		h.stats.RateLimited++
		return ErrRateLimited
	}
	h.recent[userID] = append(kept, now)
	return nil
}

func (h *AIHandler) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *AIHandler) release() { <-h.sem }

func (h *AIHandler) cacheGet(key string) (string, bool) {
	if h.cacheTTL <= 0 || h.cacheSize <= 0 {
		return "", false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	entry, ok := h.cache[key]
	if !ok {
		return "", false
	}
	if h.now().Sub(entry.at) > h.cacheTTL {
		delete(h.cache, key)
		return "", false
	}
	return entry.value, true
}

func (h *AIHandler) cachePut(key, value string) {
	if h.cacheTTL <= 0 || h.cacheSize <= 0 || value == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.cache[key]; !exists && len(h.cache) >= h.cacheSize {
		var oldestKey string
		var oldest time.Time
		first := true
		for k, e := range h.cache {
			if first || e.at.Before(oldest) {
				oldestKey, oldest, first = k, e.at, false
			}
		}
		delete(h.cache, oldestKey)
	}
	h.cache[key] = cacheEntry{value: value, at: h.now()}
}

func (h *AIHandler) bump(f func(*Stats)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(&h.stats)
}

// retryable 判断错误是否值得重试。
func retryable(err error) bool {
	if err == nil {
		return false
	}
	var statusErr *llm.StatusError
	if errors.As(err, &statusErr) {
		return statusErr.Retryable()
	}
	if errors.Is(err, llm.ErrEmptyResponse) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	// 网络类错误通常可以重试
	return true
}

// buildMessages 组装最终请求：system + 裁剪后的历史 + 当前问题。
func buildMessages(system string, history []llm.Message, question string, maxTokens int) []llm.Message {
	budget := maxTokens - estimateTokens(question) - messageOverhead
	if budget < minMessageTokens {
		budget = minMessageTokens
	}
	out := make([]llm.Message, 0, len(history)+2)
	if system != "" {
		out = append(out, llm.Message{Role: llm.RoleSystem, Content: system})
	}
	out = append(out, TrimHistory(system, history, budget)...)
	out = append(out, llm.Message{Role: llm.RoleUser, Content: question})
	return out
}

// TrimHistory 在 token 预算内保留最近的对话消息：
// system prompt 永远保留，历史消息从最新往前取，超预算的最旧消息被丢弃。
// 无论如何至少保留一条历史消息，超出预算时截断其内容。
func TrimHistory(system string, history []llm.Message, maxTokens int) []llm.Message {
	if len(history) == 0 {
		return nil
	}
	remaining := maxTokens
	if system != "" {
		remaining -= estimateTokens(system) + messageOverhead
	}
	if remaining < minMessageTokens {
		remaining = minMessageTokens
	}

	kept := make([]llm.Message, 0, len(history))
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i]
		cost := estimateTokens(msg.Content) + messageOverhead
		if cost > remaining {
			if len(kept) == 0 {
				// 最新一条就超预算：截断内容后保留
				msg.Content = truncateRunes(msg.Content, remaining*4)
				if msg.Content == "" || estimateTokens(msg.Content)+messageOverhead > remaining {
					break
				}
			} else {
				break
			}
		}
		kept = append(kept, msg)
		remaining -= cost
	}
	if len(kept) == 0 {
		msg := history[len(history)-1]
		msg.Content = truncateRunes(msg.Content, 60)
		kept = append(kept, msg)
	}

	// 反转回时间顺序
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return kept
}

// estimateTokens 粗略估算 token 数：CJK 字符约 1 token/字，其余约 4 字符/token。
// 目的是控制上下文预算，不追求与具体分词器完全一致。
func estimateTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

func estimateMessages(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += estimateTokens(m.Content) + messageOverhead
	}
	return total
}

func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF, // 汉字
		r >= 0x3400 && r <= 0x4DBF, // 汉字扩展 A
		r >= 0x3000 && r <= 0x303F, // 中日韩标点
		r >= 0xFF00 && r <= 0xFFEF: // 全角字符
		return true
	}
	return false
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func cacheKey(kind, content string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + strings.ToLower(strings.TrimSpace(content))))
	return kind + ":" + hex.EncodeToString(sum[:8])
}
