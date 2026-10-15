package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// openWebClient 打开一个网页客户端连接（模拟一个浏览器窗口），
// 返回事件行通道与会话号。
func openWebClient(t *testing.T, base, name string) (chan string, string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events?name="+name, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open sse: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sse status = %d, want 200", resp.StatusCode)
	}

	lines := make(chan string, 128)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	// 第一条 data 是 init，携带会话号
	data := waitForLine(t, lines, `"session"`)
	var init struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "data: ")), &init); err != nil {
		t.Fatalf("解析 init 失败: %v (%q)", err, data)
	}
	if init.Session == "" {
		t.Fatal("init 事件缺少 session")
	}
	return lines, init.Session
}

func waitForLine(t *testing.T, lines <-chan string, want string) string {
	t.Helper()
	deadline := time.After(4 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("SSE 流已关闭，仍未收到 %q", want)
			}
			if strings.Contains(line, want) {
				return line
			}
		case <-deadline:
			t.Fatalf("等待 %q 超时", want)
		}
	}
}

func postWeb(t *testing.T, base, session, text string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"session": session, "text": text})
	resp, err := http.Post(base+"/send", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post send: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// 两个浏览器窗口（alice / bob）作为两个客户端，消息必须互相可见。
func TestWebUIClientEndToEnd(t *testing.T) {
	_, tcpAddr := startTestServer(t)
	httpSrv := httptest.NewServer(NewWebUI(tcpAddr).Handler())
	t.Cleanup(httpSrv.Close)

	aliceLines, aliceSession := openWebClient(t, httpSrv.URL, "alice")
	waitForLine(t, aliceLines, "成功更改用户名为 alice")

	bobLines, _ := openWebClient(t, httpSrv.URL, "bob")
	waitForLine(t, bobLines, "成功更改用户名为 bob")

	if code := postWeb(t, httpSrv.URL, aliceSession, "hello-web"); code != http.StatusNoContent {
		t.Fatalf("send status = %d, want 204", code)
	}
	waitForLine(t, aliceLines, "hello-web")
	waitForLine(t, bobLines, "[alice]:hello-web")
	if code := postWeb(t, httpSrv.URL, aliceSession, "to|bob|private-web"); code != http.StatusNoContent {
		t.Fatalf("private send status = %d, want 204", code)
	}
	waitForLine(t, aliceLines, "[alice对bob说]：private-web")
	waitForLine(t, bobLines, "alice对你说：private-web")
}

func TestWebUIPolishAndSummary(t *testing.T) {
	_, tcpAddr := startTestServer(t)
	httpSrv := httptest.NewServer(NewWebUI(tcpAddr).Handler())
	t.Cleanup(httpSrv.Close)
	lines, session := openWebClient(t, httpSrv.URL, "alice")
	waitForLine(t, lines, "成功更改用户名为 alice")
	if code := postWeb(t, httpSrv.URL, session, "/polish draft-web"); code != http.StatusNoContent {
		t.Fatalf("polish status = %d", code)
	}
	waitForLine(t, lines, "润色建议：reply:draft-web")
	if code := postWeb(t, httpSrv.URL, session, "/summary 20"); code != http.StatusNoContent {
		t.Fatalf("summary status = %d", code)
	}
	waitForLine(t, lines, "暂无聊天记录")
}

func TestWebUISendValidation(t *testing.T) {
	_, tcpAddr := startTestServer(t)
	httpSrv := httptest.NewServer(NewWebUI(tcpAddr).Handler())
	t.Cleanup(httpSrv.Close)

	if code := postWeb(t, httpSrv.URL, "missing-session", "hi"); code != http.StatusNotFound {
		t.Errorf("未知会话应返回 404，实际 %d", code)
	}

	resp, err := http.Get(httpSrv.URL + "/send")
	if err != nil {
		t.Fatalf("get send: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /send 应返回 405，实际 %d", resp.StatusCode)
	}
}

func TestWebUIServesPage(t *testing.T) {
	_, tcpAddr := startTestServer(t)
	httpSrv := httptest.NewServer(NewWebUI(tcpAddr).Handler())
	t.Cleanup(httpSrv.Close)

	resp, err := http.Get(httpSrv.URL + "/")
	if err != nil {
		t.Fatalf("get index: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("Content-Type = %q, want text/html", resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(string(body), "IM 聊天室") {
		t.Error("首页内容不是网页客户端页面")
	}
}
