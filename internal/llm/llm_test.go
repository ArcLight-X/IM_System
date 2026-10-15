package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient 启动一个假的模型服务端，返回按 handler 响应的客户端。
func newTestClient(t *testing.T, handler http.HandlerFunc, timeout time.Duration) *HTTPClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(Config{
		BaseURL: srv.URL,
		APIKey:  "test-key",
		Model:   "test-model",
		Timeout: timeout,
	})
}

func jsonResponse(content string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": content}},
			},
		})
	}
}

func TestCompleteSuccess(t *testing.T) {
	var gotAuth, gotCT, gotBody string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		jsonResponse("  你好，世界  ")(w, r)
	}, time.Second)

	got, err := client.Complete(context.Background(), []Message{{Role: RoleUser, Content: "hello"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "你好，世界" {
		t.Errorf("content = %q, want 你好，世界（应去除首尾空白）", got)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if !strings.Contains(gotBody, `"test-model"`) || !strings.Contains(gotBody, "hello") {
		t.Errorf("request body 缺少 model/messages: %s", gotBody)
	}
}

func TestCompleteErrorCases(t *testing.T) {
	tests := []struct {
		name          string
		handler       http.HandlerFunc
		timeout       time.Duration
		wantStatus    int
		wantRetryable bool
		wantErrEmpty  bool
		wantTimeout   bool
	}{
		{
			name:          "429 限流可重试",
			handler:       func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
			timeout:       time.Second,
			wantStatus:    429,
			wantRetryable: true,
		},
		{
			name:          "500 服务端错误可重试",
			handler:       func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
			timeout:       time.Second,
			wantStatus:    500,
			wantRetryable: true,
		},
		{
			name:          "400 参数错误不可重试",
			handler:       func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadRequest) },
			timeout:       time.Second,
			wantStatus:    400,
			wantRetryable: false,
		},
		{
			name: "响应体不是合法 JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "{not-json")
			},
			timeout: time.Second,
		},
		{
			name: "choices 为空数组",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"choices":[]}`)
			},
			timeout:      time.Second,
			wantErrEmpty: true,
		},
		{
			name: "content 为空字符串",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"   "}}]}`)
			},
			timeout:      time.Second,
			wantErrEmpty: true,
		},
		{
			name: "服务端不响应触发超时",
			handler: func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(300 * time.Millisecond)
				jsonResponse("too late")(w, r)
			},
			timeout:     50 * time.Millisecond,
			wantTimeout: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClient(t, tt.handler, tt.timeout)
			_, err := client.Complete(context.Background(), []Message{{Role: RoleUser, Content: "hi"}})
			if err == nil {
				t.Fatal("期望返回错误，实际为 nil")
			}

			if tt.wantStatus != 0 {
				var statusErr *StatusError
				if !errors.As(err, &statusErr) {
					t.Fatalf("错误类型应为 *StatusError，实际 %T: %v", err, err)
				}
				if statusErr.Status != tt.wantStatus {
					t.Errorf("status = %d, want %d", statusErr.Status, tt.wantStatus)
				}
				if statusErr.Retryable() != tt.wantRetryable {
					t.Errorf("Retryable() = %v, want %v", statusErr.Retryable(), tt.wantRetryable)
				}
			}
			if tt.wantErrEmpty && !errors.Is(err, ErrEmptyResponse) {
				t.Errorf("期望 ErrEmptyResponse，实际 %v", err)
			}
			if tt.wantTimeout && !strings.Contains(err.Error(), "context deadline exceeded") {
				t.Errorf("期望超时错误，实际 %v", err)
			}
		})
	}
}

func TestCompleteEmptyConversation(t *testing.T) {
	client := NewClient(Config{BaseURL: "http://127.0.0.1:1", APIKey: "k"})
	if _, err := client.Complete(context.Background(), nil); err == nil {
		t.Fatal("空对话应直接返回错误，不发起请求")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("LLM_API_KEY", "")
	if _, err := FromEnv(); err == nil {
		t.Error("缺少 API Key 时应返回错误")
	}

	t.Setenv("LLM_API_KEY", "k")
	t.Setenv("LLM_MODEL", "m")
	t.Setenv("LLM_TIMEOUT", "3s")
	client, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("client 不应为 nil")
	}
}

func TestConfigDefaults(t *testing.T) {
	c := NewClient(Config{}).cfg
	if c.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	if c.Model != "gpt-4o-mini" {
		t.Errorf("Model = %q", c.Model)
	}
	if c.Timeout != 20*time.Second {
		t.Errorf("Timeout = %v", c.Timeout)
	}
}
