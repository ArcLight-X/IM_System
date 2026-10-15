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

func TestAISeesOnlyParticipantsPrivateMessages(t *testing.T) {
	requests := make(chan []llm.Message, 3)
	client := &fakeClient{fn: func(_ context.Context, msgs []llm.Message) (string, error) {
		requests <- msgs
		return "ok", nil
	}}
	srv, addr := startTestServer(t, WithAIHandler(NewAIHandler(client, WithRetries(0), WithCache(0, 0))))
	alice, aliceReader := dial(t, addr)
	bob, bobReader := dial(t, addr)
	charlie, charlieReader := dial(t, addr)
	for _, tc := range []struct {
		conn   net.Conn
		reader *bufio.Reader
		name   string
	}{{alice, aliceReader, "alice"}, {bob, bobReader, "bob"}, {charlie, charlieReader, "charlie"}} {
		if _, err := fmt.Fprintf(tc.conn, "rename|%s\n", tc.name); err != nil {
			t.Fatal(err)
		}
		readUntil(t, tc.reader, tc.conn, "成功更改用户名为 "+tc.name, 3*time.Second)
	}
	if _, err := fmt.Fprint(alice, "to|bob|今晚七点在门口见，记得带材料\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, bobReader, bob, "记得带材料", 3*time.Second)
	readUntil(t, aliceReader, alice, "记得带材料", 3*time.Second)
	if srv.History.Len() != 0 {
		t.Fatal("私聊不应进入公聊摘要历史")
	}
	for _, tc := range []struct {
		conn        net.Conn
		reader      *bufio.Reader
		name        string
		wantPrivate bool
	}{{bob, bobReader, "bob", true}, {charlie, charlieReader, "charlie", false}, {alice, aliceReader, "alice", true}} {
		if _, err := fmt.Fprint(tc.conn, "/ai 怎么回复？\n"); err != nil {
			t.Fatal(err)
		}
		readUntil(t, tc.reader, tc.conn, "[AI] ok", 3*time.Second)
		select {
		case msgs := <-requests:
			content := ""
			for _, m := range msgs {
				content += m.Content + "\n"
			}
			if got := strings.Contains(content, "记得带材料"); got != tc.wantPrivate {
				t.Fatalf("%s 的 AI 上下文私聊可见性错误: %q", tc.name, content)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("未收到 AI 请求")
		}
	}
	if _, err := fmt.Fprint(bob, "/summary\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, bobReader, bob, "暂无聊天记录", 3*time.Second)
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

func TestTCPMessageBoundaries(t *testing.T) {
	srv, addr := startTestServer(t)
	conn, reader := dial(t, addr)
	// 一次写入两条完整消息，不得当成一条。
	if _, err := fmt.Fprint(conn, "first\nsecond\r\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, reader, conn, "]:first", 3*time.Second)
	readUntil(t, reader, conn, "]:second", 3*time.Second)

	// 消息没有结束符时不得提前处理；内容可跨多次 TCP 写入。
	if _, err := fmt.Fprint(conn, "split-"); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if line, err := reader.ReadString('\n'); err == nil {
		t.Fatalf("未完成的消息被提前处理: %q", line)
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("等待未完成消息时得到意外错误: %v", err)
	}
	if _, err := fmt.Fprint(conn, "message\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, reader, conn, "]:split-message", 3*time.Second)
	if got := srv.History.Recent(3); len(got) != 3 || !strings.Contains(got[0], "first") || !strings.Contains(got[1], "second") || !strings.Contains(got[2], "split-message") {
		t.Fatalf("消息未按行分别记录: %q", got)
	}
}

func TestTCPRejectsOversizedMessage(t *testing.T) {
	srv, addr := startTestServer(t)
	conn, reader := dial(t, addr)
	if _, err := fmt.Fprint(conn, strings.Repeat("x", maxMessageBytes+1)+"\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, reader, conn, "消息过长", 3*time.Second)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("超长消息后连接未关闭")
			}
			break
		}
		if strings.Contains(line, strings.Repeat("x", 32)) {
			t.Fatal("超长消息被当作聊天内容广播")
		}
	}
	if got := srv.History.Len(); got != 0 {
		t.Fatalf("超长消息被记录进历史: %d", got)
	}
}

func TestTCPAcceptsMaximumSizeMessage(t *testing.T) {
	srv, addr := startTestServer(t)
	conn, reader := dial(t, addr)
	content := strings.Repeat("x", maxMessageBytes)
	if _, err := fmt.Fprint(conn, content+"\r\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, reader, conn, "]:"+content, 3*time.Second)
	if got := srv.History.Recent(1); len(got) != 1 || !strings.HasSuffix(got[0], content) {
		t.Fatal("达到上限的消息没有被完整处理")
	}
}

func TestTCPDoesNotProcessIncompleteMessageOnEOF(t *testing.T) {
	srv, addr := startTestServer(t)
	conn, _ := dial(t, addr)
	if _, err := fmt.Fprint(conn, "incomplete"); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.mapLock.RLock()
		count := len(srv.OnlineMap)
		srv.mapLock.RUnlock()
		if count == 0 {
			if srv.History.Len() != 0 {
				t.Fatal("断连时把残缺消息当作完整消息处理")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("连接断开后用户未下线")
}

func TestPrivateChatSlowRecipientDoesNotBlockSender(t *testing.T) {
	srv := NewServer("127.0.0.1", 0, WithWriteTimeout(50*time.Millisecond))
	senderConn, senderPeer := net.Pipe()
	defer senderPeer.Close()
	recipientConn, recipientPeer := net.Pipe()
	defer recipientPeer.Close()
	sender := NewUser(senderConn, srv)
	recipient := NewUser(recipientConn, srv)
	sender.Name = "sender"
	recipient.Name = "recipient"
	sender.Online()
	recipient.Online()
	defer sender.Close()
	defer recipient.Close()

	finished := make(chan struct{})
	go func() {
		sender.DoMessage("to|recipient|hello")
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("慢读私聊接收方阻塞了发件人")
	}
	select {
	case <-recipient.done:
	case <-time.After(time.Second):
		t.Fatal("写超时后未清理慢读用户")
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
	readUntil(t, bobReader, bob, "["+bob.LocalAddr().String()+"对alice说]：hello", 3*time.Second)
}

func TestPrivateChatEchoAfterRenameAndNoEchoOnFailure(t *testing.T) {
	_, addr := startTestServer(t)
	alice, aliceReader := dial(t, addr)
	bob, bobReader := dial(t, addr)
	if _, err := fmt.Fprint(alice, "rename|alice\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, aliceReader, alice, "成功更改用户名为 alice", 3*time.Second)
	if _, err := fmt.Fprint(bob, "rename|bob\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, bobReader, bob, "成功更改用户名为 bob", 3*time.Second)
	if _, err := fmt.Fprint(alice, "to|bob|你好\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, bobReader, bob, "alice对你说：你好", 3*time.Second)
	readUntil(t, aliceReader, alice, "[alice对bob说]：你好", 3*time.Second)
	if _, err := fmt.Fprint(alice, "to|nobody|失败\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, aliceReader, alice, "该用户名不存在", 3*time.Second)
}

func TestDefaultIdleTimeout(t *testing.T) {
	if got := NewServer("127.0.0.1", 0).IdleTimeout; got != 1000*time.Second {
		t.Fatalf("默认空闲超时 = %v，期望 1000 秒", got)
	}
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

func TestSummaryMultilineKeepsAIPrefix(t *testing.T) {
	_, addr := startTestServer(t)
	conn, reader := dial(t, addr)
	if _, err := fmt.Fprint(conn, "first-context\nsecond-context\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, reader, conn, "]:second-context", 3*time.Second)
	if _, err := fmt.Fprint(conn, "/summary 2\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, reader, conn, "[AI] 群聊摘要：", 3*time.Second)
	if line := readUntil(t, reader, conn, "first-context", 3*time.Second); !strings.HasPrefix(line, "[AI] ") {
		t.Fatalf("摘要首行缺少 AI 标记: %q", line)
	}
	if line := readUntil(t, reader, conn, "second-context", 3*time.Second); !strings.HasPrefix(line, "[AI] ") {
		t.Fatalf("摘要后续行缺少 AI 标记: %q", line)
	}
}

func TestPolishOnlyRepliesToRequester(t *testing.T) {
	srv, addr := startTestServer(t)
	alice, aliceReader := dial(t, addr)
	bob, bobReader := dial(t, addr)
	if _, err := fmt.Fprint(alice, "/polish hello-draft\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, aliceReader, alice, "润色建议：reply:hello-draft", 3*time.Second)
	if srv.History.Len() != 0 {
		t.Fatal("润色草稿不应进入公聊历史")
	}
	_ = bob.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	for {
		line, err := bobReader.ReadString('\n')
		if strings.Contains(line, "hello-draft") {
			t.Fatalf("润色草稿泄漏给其他用户: %q", line)
		}
		if err != nil {
			if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
				t.Fatalf("等待公聊时出错: %v", err)
			}
			break
		}
	}
	if _, err := fmt.Fprint(alice, "/summary\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, aliceReader, alice, "暂无聊天记录", 3*time.Second)
}

func TestPolishWithoutConfigurationAndEmptyDraft(t *testing.T) {
	_, addr := startTestServer(t, WithAIHandler(nil))
	conn, reader := dial(t, addr)
	if _, err := fmt.Fprint(conn, "/polish\n/polish test\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, reader, conn, "用法：/polish", 3*time.Second)
	readUntil(t, reader, conn, "未配置模型服务", 3*time.Second)
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
