package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"server-demo/internal/llm"
)

// echoClient 是确定性的假模型：把最后一条用户消息原样回显。
// 这样就能断言「回复回到了正确的连接」，而不依赖任何外部服务。
type echoClient struct{}

func (echoClient) Complete(_ context.Context, msgs []llm.Message) (string, error) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleUser {
			return "reply:" + msgs[i].Content, nil
		}
	}
	return "reply:empty", nil
}

// startTestServer 在随机端口启动服务器，返回地址。
func startTestServer(t *testing.T, opts ...ServerOption) (*Server, string) {
	t.Helper()
	base := []ServerOption{
		WithHistoryCapacity(50),
		WithIdleTimeout(10 * time.Second),
		WithAITimeout(3 * time.Second),
		WithAIHandler(NewAIHandler(echoClient{}, WithRetries(0), WithCache(0, 0))),
	}
	srv := NewServer("127.0.0.1", 0, append(base, opts...)...)

	ln, err := srv.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	t.Cleanup(srv.Shutdown)
	return srv, ln.Addr().String()
}

func dial(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, bufio.NewReader(conn)
}

// readUntil 读到包含 want 的一行为止，超时则失败。
func readUntil(t *testing.T, r *bufio.Reader, conn net.Conn, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	_ = conn.SetReadDeadline(deadline)
	var sb strings.Builder
	for {
		line, err := r.ReadString('\n')
		sb.WriteString(line)
		if strings.Contains(line, want) {
			return line
		}
		if err != nil {
			t.Fatalf("等待 %q 时出错: %v，已收到:\n%s", want, err, sb.String())
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %q 超时，已收到:\n%s", want, sb.String())
		}
	}
}

// 多个用户同时提问，每个用户必须收到属于自己的回答。
func TestAIConcurrentRouting(t *testing.T) {
	_, addr := startTestServer(t)

	const clients = 12
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Errorf("client %d dial: %v", i, err)
				return
			}
			defer conn.Close()

			question := fmt.Sprintf("question-%02d", i)
			if _, err := fmt.Fprintf(conn, "/ai %s\n", question); err != nil {
				t.Errorf("client %d write: %v", i, err)
				return
			}

			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			r := bufio.NewReader(conn)
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					t.Errorf("client %d 未收到自己的回答 %q: %v", i, question, err)
					return
				}
				if strings.Contains(line, "reply:"+question) {
					return
				}
				// 收到别人的回答说明路由串了
				if strings.Contains(line, "reply:question-") {
					t.Errorf("client %d 收到不属于自己的回答: %s", i, strings.TrimSpace(line))
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// 慢客户端（连上后不读）不应拖住其他人的广播。
func TestSlowClientDoesNotBlockBroadcast(t *testing.T) {
	_, addr := startTestServer(t)

	slow, _ := dial(t, addr) // 故意不读取
	defer slow.Close()

	fast, reader := dial(t, addr)
	received := make(chan int, 1)

	go func() {
		count := 0
		_ = fast.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				break
			}
			if strings.Contains(line, "msg-") {
				count++
			}
		}
		received <- count
	}()

	for i := 0; i < 20; i++ {
		if _, err := fmt.Fprintf(fast, "msg-%02d\n", i); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	select {
	case count := <-received:
		if count < 15 {
			t.Errorf("快速客户端只收到 %d/20 条广播，广播可能被慢客户端阻塞", count)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("等待广播超时")
	}
}

// 畸形命令不应导致服务器崩溃（历史上 to|用户名 会越界 panic）。
func TestMalformedCommandsDoNotCrash(t *testing.T) {
	_, addr := startTestServer(t)

	alice, _ := dial(t, addr)
	if _, err := fmt.Fprintf(alice, "rename|alice\n"); err != nil {
		t.Fatalf("write: %v", err)
	}

	bob, bobReader := dial(t, addr)
	// 在线用户 alice 存在，但消息缺少内容段
	if _, err := fmt.Fprintf(bob, "to|alice\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntil(t, bobReader, bob, "格式有误", 3*time.Second)

	if _, err := fmt.Fprintf(bob, "to|\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntil(t, bobReader, bob, "格式有误", 3*time.Second)

	if _, err := fmt.Fprintf(bob, "rename|\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntil(t, bobReader, bob, "格式有误", 3*time.Second)

	// 正常的私聊仍然可用
	if _, err := fmt.Fprintf(bob, "to|alice|hello\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	aliceReader := bufio.NewReader(alice)
	readUntil(t, aliceReader, alice, "对你说：hello", 3*time.Second)
}

// 未配置 AI 时给出明确提示，而不是静默失败。
func TestAIWithoutConfiguration(t *testing.T) {
	_, addr := startTestServer(t, WithAIHandler(nil))

	conn, reader := dial(t, addr)
	if _, err := fmt.Fprintf(conn, "/ai 测试\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntil(t, reader, conn, "未配置模型服务", 3*time.Second)
}

func TestAIUsageHint(t *testing.T) {
	_, addr := startTestServer(t)

	conn, reader := dial(t, addr)
	if _, err := fmt.Fprintf(conn, "/ai\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntil(t, reader, conn, "用法：/ai", 3*time.Second)
}

func TestSummaryCommandRecordsHistory(t *testing.T) {
	_, addr := startTestServer(t)

	conn, reader := dial(t, addr)
	if _, err := fmt.Fprintf(conn, "今天要把回归跑完\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntil(t, reader, conn, "今天要把回归跑完", 3*time.Second)

	if _, err := fmt.Fprintf(conn, "/summary 5\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 假模型会回显聊天记录内容
	readUntil(t, reader, conn, "今天要把回归跑完", 3*time.Second)
}

func TestIdleTimeoutKicksUser(t *testing.T) {
	_, addr := startTestServer(t, WithIdleTimeout(300*time.Millisecond))

	conn, reader := dial(t, addr)
	readUntil(t, reader, conn, "你被踢出", 3*time.Second)
}

func TestShutdownStopsListener(t *testing.T) {
	srv, addr := startTestServer(t)

	conn, _ := dial(t, addr)
	srv.Shutdown()
	_ = conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", addr); err != nil {
			return // 端口已关闭
		} else {
			_ = c.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("Shutdown 后监听端口仍在接受连接")
}
