package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"server-demo/internal/llm"
)

// fakeClient 是可编程的假模型客户端，记录调用次数与并发峰值。
type fakeClient struct {
	mu     sync.Mutex
	calls  int
	inFly  int
	peak   int
	fn     func(ctx context.Context, msgs []llm.Message) (string, error)
	errSeq []error // 按调用顺序返回的错误，用完后走 fn
}

func (f *fakeClient) Complete(ctx context.Context, msgs []llm.Message) (string, error) {
	f.mu.Lock()
	f.calls++
	f.inFly++
	if f.inFly > f.peak {
		f.peak = f.inFly
	}
	var seqErr error
	if len(f.errSeq) > 0 {
		seqErr = f.errSeq[0]
		f.errSeq = f.errSeq[1:]
	}
	fn := f.fn
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		f.inFly--
		f.mu.Unlock()
	}()

	if seqErr != nil {
		return "", seqErr
	}
	if fn != nil {
		return fn(ctx, msgs)
	}
	return "fake answer", nil
}

func (f *fakeClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeClient) peakConcurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

func lastUserContent(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleUser {
			return msgs[i].Content
		}
	}
	return ""
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"空字符串", "", 0},
		{"纯英文", "hello", 2},          // 5 字符 → ceil(5/4)
		{"纯中文", "你好世界", 4},           // 4 字 → 4
		{"中英混合", "你好abc", 3},         // 2 + ceil(3/4)
		{"英文疑问句", "how are you?", 3}, // 12 字符 → 3
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := estimateTokens(tt.in); got != tt.want {
				t.Errorf("estimateTokens(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestTrimHistory(t *testing.T) {
	history := []llm.Message{
		{Role: llm.RoleUser, Content: "第一条：昨天讲了并发"},
		{Role: llm.RoleAssistant, Content: "第二条：读写锁保护共享状态"},
		{Role: llm.RoleUser, Content: "第三条：今天的任务是补测试"},
	}

	t.Run("空历史返回 nil", func(t *testing.T) {
		if got := TrimHistory("system", nil, 100); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("预算充足时全部保留且保持时间顺序", func(t *testing.T) {
		got := TrimHistory("system", history, 1000)
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}
		if !strings.HasPrefix(got[0].Content, "第一条") || !strings.HasPrefix(got[2].Content, "第三条") {
			t.Errorf("顺序错误: %v", contents(got))
		}
	})

	t.Run("预算不足时丢弃最旧的消息", func(t *testing.T) {
		got := TrimHistory("", history, 30)
		if len(got) == 0 || len(got) == 3 {
			t.Fatalf("应保留部分消息，实际 %d 条", len(got))
		}
		if !strings.HasPrefix(got[len(got)-1].Content, "第三条") {
			t.Errorf("最新一条必须保留，实际最后一条为 %q", got[len(got)-1].Content)
		}
	})

	t.Run("预算极小时至少保留一条并截断内容", func(t *testing.T) {
		got := TrimHistory("", history, 1)
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		if got[0].Content == "" {
			t.Error("截断后内容不应为空")
		}
	})

	t.Run("system prompt 占用预算", func(t *testing.T) {
		long := strings.Repeat("系", 200)
		got := TrimHistory(long, history, 50)
		if len(got) > 1 {
			t.Errorf("system 已占满预算时应只保留最新一条，实际 %d 条", len(got))
		}
	})
}

func TestBuildMessages(t *testing.T) {
	msgs := buildMessages("你是助手", []llm.Message{{Role: llm.RoleUser, Content: "历史消息"}}, "当前问题", 500)
	if len(msgs) < 3 {
		t.Fatalf("len = %d, want >= 3", len(msgs))
	}
	if msgs[0].Role != llm.RoleSystem || msgs[0].Content != "你是助手" {
		t.Errorf("第一条应为 system prompt，实际 %+v", msgs[0])
	}
	last := msgs[len(msgs)-1]
	if last.Role != llm.RoleUser || last.Content != "当前问题" {
		t.Errorf("最后一条应为当前问题，实际 %+v", last)
	}
}

func TestAskReturnsAnswer(t *testing.T) {
	client := &fakeClient{fn: func(_ context.Context, msgs []llm.Message) (string, error) {
		return "回答：" + lastUserContent(msgs), nil
	}}
	h := NewAIHandler(client, WithRetries(0))

	got, err := h.Ask(context.Background(), "user-1", "什么是读写锁", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "回答：什么是读写锁" {
		t.Errorf("got %q", got)
	}
	if h.Stats().Requests != 1 {
		t.Errorf("Requests = %d, want 1", h.Stats().Requests)
	}
}

func TestAskEmptyQuestion(t *testing.T) {
	h := NewAIHandler(&fakeClient{}, WithRetries(0))
	if _, err := h.Ask(context.Background(), "u", "   ", nil); !errors.Is(err, ErrEmptyQuestion) {
		t.Errorf("err = %v, want ErrEmptyQuestion", err)
	}
}

func TestAskCacheAvoidsDuplicateCalls(t *testing.T) {
	client := &fakeClient{}
	h := NewAIHandler(client, WithRetries(0), WithCache(time.Minute, 8))

	for i := 0; i < 3; i++ {
		if _, err := h.Ask(context.Background(), "u", "重复的问题", nil); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if client.callCount() != 1 {
		t.Errorf("模型调用次数 = %d, want 1（后两次应命中缓存）", client.callCount())
	}
	stats := h.Stats()
	if stats.CacheHits != 2 {
		t.Errorf("CacheHits = %d, want 2", stats.CacheHits)
	}
	if stats.Requests != 1 {
		t.Errorf("Requests = %d, want 1", stats.Requests)
	}
}

func TestAskCacheSeparatesUsersAndContext(t *testing.T) {
	client := &fakeClient{}
	h := NewAIHandler(client, WithRetries(0), WithCache(time.Minute, 8))
	ctx := context.Background()
	a := []llm.Message{{Role: llm.RoleUser, Content: "[私聊] alice对bob说：第一条"}}
	b := []llm.Message{{Role: llm.RoleUser, Content: "[私聊] alice对bob说：第二条"}}
	for _, tc := range []struct {
		user    string
		history []llm.Message
	}{{"bob", a}, {"bob", a}, {"bob", b}, {"alice", a}} {
		if _, err := h.Ask(ctx, tc.user, "该怎么回答？", tc.history); err != nil {
			t.Fatal(err)
		}
	}
	if got := client.callCount(); got != 3 {
		t.Fatalf("不同用户或上下文应分别调用模型，实际调用 %d 次", got)
	}
}

func TestAskRetriesRetryableError(t *testing.T) {
	retryableErr := &llm.StatusError{Status: 503, Body: "unavailable"}
	client := &fakeClient{
		errSeq: []error{retryableErr, retryableErr},
		fn:     func(context.Context, []llm.Message) (string, error) { return "第三次成功", nil },
	}
	h := NewAIHandler(client, WithRetries(2))

	got, err := h.Ask(context.Background(), "u", "重试测试", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "第三次成功" {
		t.Errorf("got %q", got)
	}
	if client.callCount() != 3 {
		t.Errorf("调用次数 = %d, want 3", client.callCount())
	}
	if h.Stats().Retries != 2 {
		t.Errorf("Retries = %d, want 2", h.Stats().Retries)
	}
}

func TestAskDoesNotRetryConfigError(t *testing.T) {
	client := &fakeClient{errSeq: []error{&llm.StatusError{Status: 401, Body: "bad key"}}}
	h := NewAIHandler(client, WithRetries(3), WithFallback("AI 暂时不可用"))

	got, err := h.Ask(context.Background(), "u", "认证失败测试", nil)
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if client.callCount() != 1 {
		t.Errorf("401 不应重试，调用次数 = %d, want 1", client.callCount())
	}
	if got != "AI 暂时不可用" {
		t.Errorf("失败时应返回降级文案，实际 %q", got)
	}
	if h.Stats().Fallbacks != 1 {
		t.Errorf("Fallbacks = %d, want 1", h.Stats().Fallbacks)
	}
}

func TestAskTimeoutFallsBack(t *testing.T) {
	client := &fakeClient{fn: func(ctx context.Context, _ []llm.Message) (string, error) {
		// 模拟慢模型：一直等到上层超时
		<-ctx.Done()
		return "", ctx.Err()
	}}
	h := NewAIHandler(client, WithTimeout(60*time.Millisecond), WithRetries(0), WithFallback("模型超时"))

	start := time.Now()
	got, err := h.Ask(context.Background(), "u", "超时测试", nil)
	if err == nil {
		t.Fatal("期望返回超时错误")
	}
	if got != "模型超时" {
		t.Errorf("got %q, want 降级文案", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("超时未生效，耗时 %v", elapsed)
	}
}

func TestAskRateLimited(t *testing.T) {
	client := &fakeClient{}
	h := NewAIHandler(client, WithRetries(0), WithRateLimit(2), WithCache(0, 0))

	for i := 0; i < 2; i++ {
		if _, err := h.Ask(context.Background(), "user-1", fmt.Sprintf("问题%d", i), nil); err != nil {
			t.Fatalf("第 %d 次请求不应被限流: %v", i+1, err)
		}
	}
	if _, err := h.Ask(context.Background(), "user-1", "问题3", nil); !errors.Is(err, ErrRateLimited) {
		t.Errorf("第 3 次请求应被限流，实际 err = %v", err)
	}
	// 其他用户不受影响
	if _, err := h.Ask(context.Background(), "user-2", "问题3", nil); err != nil {
		t.Errorf("其他用户不应被限流: %v", err)
	}
	if h.Stats().RateLimited != 1 {
		t.Errorf("RateLimited = %d, want 1", h.Stats().RateLimited)
	}
}

func TestAskLimitsConcurrency(t *testing.T) {
	const limit = 3
	client := &fakeClient{fn: func(ctx context.Context, _ []llm.Message) (string, error) {
		time.Sleep(30 * time.Millisecond)
		return "ok", nil
	}}
	h := NewAIHandler(client, WithRetries(0), WithConcurrency(limit), WithCache(0, 0))

	var wg sync.WaitGroup
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := fmt.Sprintf("user-%d", i)
			_, _ = h.Ask(context.Background(), user, fmt.Sprintf("并发问题 %d", i), nil)
		}(i)
	}
	wg.Wait()

	if peak := client.peakConcurrent(); peak > limit {
		t.Errorf("并发峰值 = %d, 超过上限 %d", peak, limit)
	}
}

func TestAskWithoutClientFallsBack(t *testing.T) {
	h := NewAIHandler(nil, WithRetries(0))
	got, err := h.Ask(context.Background(), "u", "未配置测试", nil)
	if !errors.Is(err, ErrClientNotConfigured) {
		t.Errorf("err = %v, want ErrClientNotConfigured", err)
	}
	if got == "" {
		t.Error("未配置时也应返回可展示的降级文案")
	}
}

func TestSummarizeBuildsSummaryPrompt(t *testing.T) {
	var captured []llm.Message
	client := &fakeClient{fn: func(_ context.Context, msgs []llm.Message) (string, error) {
		captured = msgs
		return "- 要点一\n- 要点二", nil
	}}
	h := NewAIHandler(client, WithRetries(0))

	got, err := h.Summarize(context.Background(), "u", []string{"[Alice]: 明天上线", "[Bob]: 我需要先跑回归"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "要点一") {
		t.Errorf("got %q", got)
	}
	if len(captured) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(captured))
	}
	if captured[0].Role != llm.RoleSystem || !strings.Contains(captured[0].Content, "摘要") {
		t.Errorf("system prompt 不正确: %+v", captured[0])
	}
	if !strings.Contains(captured[1].Content, "明天上线") || !strings.Contains(captured[1].Content, "跑回归") {
		t.Errorf("摘要请求应包含原始聊天记录: %s", captured[1].Content)
	}
}

func TestSummarizeEmptyHistory(t *testing.T) {
	h := NewAIHandler(&fakeClient{}, WithRetries(0))
	if _, err := h.Summarize(context.Background(), "u", nil); err == nil {
		t.Error("空记录应返回错误")
	}
}

func TestPolishUsesDraftOnly(t *testing.T) {
	var captured []llm.Message
	client := &fakeClient{fn: func(_ context.Context, msgs []llm.Message) (string, error) {
		captured = msgs
		return "大家好！", nil
	}}
	h := NewAIHandler(client, WithRetries(0))
	got, err := h.Polish(context.Background(), "alice", " 大家好 ")
	if err != nil || got != "大家好！" {
		t.Fatalf("Polish = %q, %v", got, err)
	}
	if len(captured) != 2 || captured[0].Role != llm.RoleSystem || !strings.Contains(captured[0].Content, "润色") || captured[1].Content != "大家好" {
		t.Fatalf("润色请求应只包含指令和草稿: %+v", captured)
	}
	if _, err := h.Polish(context.Background(), "alice", "  "); !errors.Is(err, ErrEmptyQuestion) {
		t.Fatalf("空草稿应拒绝，实际: %v", err)
	}
}

func contents(msgs []llm.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Content)
	}
	return out
}
