package main

import (
	"context"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"

	"server-demo/internal/llm"
)

// 每个用户的待发消息缓冲；缓冲满说明客户端读得太慢，直接丢弃。
const userChanBuffer = 32

type User struct {
	Name string
	Addr string
	C    chan string //channel

	conn   net.Conn
	server *Server

	// 所有写连接的操作都经过这把锁：
	// 广播、命令回复与 AI 异步回复可能来自不同 goroutine
	writeMu sync.Mutex

	// 关闭信号，用于结束 ListenMessage 与 Handler 循环
	done        chan struct{}
	closeOnce   sync.Once
	offlineOnce sync.Once

	aiMu sync.Mutex // 保证同一用户同时只有一个 AI 请求在途
}

// 创建一个用户的 api
func NewUser(conn net.Conn, server *Server) *User {
	userAddr := conn.RemoteAddr().String()

	user := &User{
		Name:   userAddr,
		Addr:   userAddr,
		C:      make(chan string, userChanBuffer),
		conn:   conn,
		server: server,
		done:   make(chan struct{}),
	}
	// 监听 user channel 的协程
	go user.ListenMessage()

	return user
}

func (this *User) Online() { //上线业务
	// 用户上线，加入到 map
	this.server.mapLock.Lock()
	this.server.OnlineMap[this.Name] = this
	this.server.mapLock.Unlock()
	// 广播用户上线消息
	this.server.BroadCast(this, "已上线")
}

func (this *User) Offline() { //下线
	this.offlineOnce.Do(func() {
		this.server.mapLock.Lock()
		delete(this.server.OnlineMap, this.Name)
		this.server.mapLock.Unlock()
		// 广播用户下线消息
		this.server.BroadCast(this, "已下线")
	})
}

// Close 关闭用户连接：清理在线列表、关闭写协程与底层连接。
// 可重复调用，只有第一次生效。
func (this *User) Close() {
	this.closeOnce.Do(func() {
		this.Offline()
		close(this.done)
		this.conn.Close()
	})
}

// SendMsg 给当前 user 对应的客户端发信息。
// 加锁保证多个 goroutine 不会同时写同一个连接。
func (this *User) SendMsg(msg string) {
	this.writeMu.Lock()
	defer this.writeMu.Unlock()
	if _, err := this.conn.Write([]byte(msg)); err != nil {
		// 写失败说明连接已不可用，清理交给读侧，这里不打扰用户
		log.Printf("write to %s failed: %v", this.Addr, err)
	}
}

// TrySend 非阻塞投递，用于广播：缓冲满时丢弃而不是阻塞广播协程。
func (this *User) TrySend(msg string) bool {
	select {
	case this.C <- msg:
		return true
	case <-this.done:
		return false
	default:
		log.Printf("drop message for slow client %s", this.Addr)
		return false
	}
}

// 监听当前 user channel 的方法
func (this *User) ListenMessage() {
	for {
		select {
		case msg := <-this.C:
			this.SendMsg(msg + "\n")
		case <-this.done:
			// 关闭前把缓冲里剩下的消息尽量发完
			for {
				select {
				case msg := <-this.C:
					this.SendMsg(msg + "\n")
				default:
					return
				}
			}
		}
	}
}

// 处理消息
func (this *User) DoMessage(msg string) {
	msg = strings.TrimSpace(msg)

	switch {
	case msg == "":
		return
	case msg == "who":
		this.handleWho()
	case strings.HasPrefix(msg, "rename|"):
		this.handleRename(msg)
	case strings.HasPrefix(msg, "to|"):
		this.handlePrivate(msg)
	case msg == "/help":
		this.sendHelp()
	case msg == "/ai" || strings.HasPrefix(msg, "/ai "):
		this.handleAI(strings.TrimSpace(strings.TrimPrefix(msg, "/ai")))
	case msg == "/summary" || strings.HasPrefix(msg, "/summary "):
		this.handleSummary(strings.TrimSpace(strings.TrimPrefix(msg, "/summary")))
	default:
		this.server.History.Add("[" + this.Name + "]: " + msg)
		this.server.BroadCast(this, msg)
	}
}

// 查询当前在线用户
func (this *User) handleWho() {
	this.server.mapLock.RLock()
	names := make([]string, 0, len(this.server.OnlineMap))
	for _, user := range this.server.OnlineMap {
		names = append(names, user.Name)
	}
	this.server.mapLock.RUnlock()

	for _, name := range names {
		this.SendMsg("[" + name + "]:" + "在线..\n")
	}
}

// 改名
func (this *User) handleRename(msg string) {
	parts := strings.SplitN(msg, "|", 2)
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		this.SendMsg("格式有误，用法：rename|新用户名\n")
		return
	}
	newName := strings.TrimSpace(parts[1])

	if newName == this.Name {
		this.SendMsg("用户名未变化\n")
		return
	}

	this.server.mapLock.Lock()
	if _, exists := this.server.OnlineMap[newName]; exists {
		this.server.mapLock.Unlock()
		this.SendMsg("当前用户名已被使用\n")
		return
	}
	delete(this.server.OnlineMap, this.Name)
	this.server.OnlineMap[newName] = this
	// 在锁内更新，避免与其他协程读取 Name 形成数据竞争
	this.Name = newName
	this.server.mapLock.Unlock()

	this.SendMsg("成功更改用户名为 " + newName + "\n")
}

// 私聊
func (this *User) handlePrivate(msg string) {
	parts := strings.SplitN(msg, "|", 3)
	if len(parts) < 3 || strings.TrimSpace(parts[1]) == "" {
		this.SendMsg("格式有误，用法：to|用户名|消息内容\n")
		return
	}
	remoteName := strings.TrimSpace(parts[1])
	content := strings.TrimSpace(parts[2])
	if content == "" {
		this.SendMsg("消息内容不能为空\n")
		return
	}

	this.server.mapLock.RLock()
	remoteUser, ok := this.server.OnlineMap[remoteName]
	this.server.mapLock.RUnlock()
	if !ok {
		this.SendMsg("该用户名不存在\n")
		return
	}
	remoteUser.SendMsg(this.Name + "对你说：" + content + "\n")
}

// AI 问答：/ai 你的问题
func (this *User) handleAI(question string) {
	if question == "" {
		this.SendMsg("用法：/ai 你的问题（示例：/ai 用一句话解释 TCP 三次握手）\n")
		return
	}
	if this.server.ai == nil {
		this.SendMsg("[AI] 未配置模型服务：请设置环境变量 LLM_API_KEY 后重启服务器\n")
		return
	}
	if !this.aiMu.TryLock() {
		this.SendMsg("[AI] 上一个问题还在处理中，请稍候\n")
		return
	}

	this.SendMsg("[AI] 正在思考…\n")

	// 取最近的聊天记录作为上下文，由 AIHandler 按 token 预算裁剪
	lines := this.server.History.Recent(12)
	history := make([]llm.Message, 0, len(lines))
	for _, line := range lines {
		history = append(history, llm.Message{Role: llm.RoleUser, Content: line})
	}

	go func() {
		defer this.aiMu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), this.server.AITimeout)
		defer cancel()

		reply, err := this.server.ai.Ask(ctx, this.Addr, question, history)
		if err != nil {
			log.Printf("ai ask failed for %s: %v", this.Addr, err)
		}
		this.SendMsg("[AI] " + reply + "\n")
	}()
}

// 群聊摘要：/summary [条数]
func (this *User) handleSummary(arg string) {
	if this.server.ai == nil {
		this.SendMsg("[AI] 未配置模型服务：请设置环境变量 LLM_API_KEY 后重启服务器\n")
		return
	}
	n := 20
	if arg != "" {
		if v, err := strconv.Atoi(arg); err == nil && v > 0 && v <= 200 {
			n = v
		} else {
			this.SendMsg("用法：/summary [1-200]，默认最近 20 条\n")
			return
		}
	}
	lines := this.server.History.Recent(n)
	if len(lines) == 0 {
		this.SendMsg("[AI] 暂无聊天记录可总结\n")
		return
	}
	if !this.aiMu.TryLock() {
		this.SendMsg("[AI] 上一个请求还在处理中，请稍候\n")
		return
	}

	this.SendMsg("[AI] 正在生成摘要…\n")

	go func() {
		defer this.aiMu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), this.server.AITimeout)
		defer cancel()

		reply, err := this.server.ai.Summarize(ctx, this.Addr, lines)
		if err != nil {
			log.Printf("ai summary failed for %s: %v", this.Addr, err)
		}
		this.SendMsg("[AI] 群聊摘要：\n" + reply + "\n")
	}()
}

func (this *User) sendHelp() {
	this.SendMsg("可用命令：\n" +
		"  直接输入内容      公聊\n" +
		"  to|用户名|内容    私聊\n" +
		"  rename|新用户名   改名\n" +
		"  who               查询在线用户\n" +
		"  /ai 问题          向 AI 助手提问\n" +
		"  /summary [条数]   总结最近聊天记录\n")
}
