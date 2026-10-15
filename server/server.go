package main

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	messageChanBuffer   = 64
	maxMessageBytes     = 4096
	defaultWriteTimeout = 5 * time.Second
)

type Server struct {
	Ip   string
	Port int

	// 在线用户列表
	OnlineMap map[string]*User
	mapLock   sync.RWMutex

	// 消息广播的 channel
	Message chan string

	IdleTimeout  time.Duration
	AITimeout    time.Duration
	WriteTimeout time.Duration

	// 聊天记录，供 /summary 与 AI 上下文使用
	History        *History
	contextHistory *contextHistory
	ai             *AIHandler

	// 网页版客户端监听地址；为空表示不启用
	webAddr string

	listener  net.Listener
	done      chan struct{}
	closeOnce sync.Once
}

// ServerOption 用于配置 Server。
type ServerOption func(*Server)

// WithAIHandler 启用 AI 能力；不设置时 /ai 命令会提示未配置。
func WithAIHandler(h *AIHandler) ServerOption {
	return func(s *Server) { s.ai = h }
}

// WithHistoryCapacity 设置聊天记录缓冲容量。
func WithHistoryCapacity(n int) ServerOption {
	return func(s *Server) {
		s.History = NewHistory(n)
		s.contextHistory = newContextHistory(n)
	}
}

// WithIdleTimeout 设置用户空闲踢出时间。
func WithIdleTimeout(d time.Duration) ServerOption {
	return func(s *Server) {
		if d > 0 {
			s.IdleTimeout = d
		}
	}
}

// WithAITimeout 设置单次 AI 请求的整体超时。
func WithAITimeout(d time.Duration) ServerOption {
	return func(s *Server) {
		if d > 0 {
			s.AITimeout = d
		}
	}
}

// WithWriteTimeout 设置向客户端写入一条消息的最长等待时间。
func WithWriteTimeout(d time.Duration) ServerOption {
	return func(s *Server) {
		if d > 0 {
			s.WriteTimeout = d
		}
	}
}

// WithWebUI 启用网页版客户端：浏览器每个窗口即一个客户端。
// addr 为空时使用默认地址 127.0.0.1:8080；不调用该选项则不启用。
func WithWebUI(addr string) ServerOption {
	return func(s *Server) {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			addr = defaultWebAddr
		}
		s.webAddr = addr
	}
}

// 创建 server 接口
func NewServer(ip string, port int, opts ...ServerOption) *Server {
	server := &Server{
		Ip:             ip,
		Port:           port,
		OnlineMap:      make(map[string]*User),
		Message:        make(chan string, messageChanBuffer),
		IdleTimeout:    time.Second * 1000,
		AITimeout:      time.Second * 30,
		WriteTimeout:   defaultWriteTimeout,
		History:        NewHistory(200),
		contextHistory: newContextHistory(200),
		done:           make(chan struct{}),
	}
	for _, opt := range opts {
		opt(server)
	}
	return server
}

// Listen 创建监听；单独抽出来是为了让测试可以监听随机端口。
func (this *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(this.Ip, strconv.Itoa(this.Port)))
	if err != nil {
		return nil, err
	}
	this.listener = ln
	return ln, nil
}

// Serve 在给定监听上服务，阻塞直到关闭。
func (this *Server) Serve(ln net.Listener) {
	go this.ListenMessager()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-this.done:
				return
			default:
			}
			log.Printf("accept error: %v", err)
			continue
		}
		go this.Handler(conn)
	}
}

// 启动服务器
func (this *Server) Start() {
	ln, err := this.Listen()
	if err != nil {
		log.Printf("listen error: %v", err)
		return
	}

	if this.webAddr != "" {
		this.startWebUI()
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		log.Println("收到退出信号，正在关闭服务器…")
		this.Shutdown()
	}()

	this.Serve(ln)
	log.Println("服务器已关闭")
}

// Shutdown 优雅关闭：通知所有协程退出并停止监听。
func (this *Server) Shutdown() {
	this.closeOnce.Do(func() {
		close(this.done)
		if this.listener != nil {
			this.listener.Close()
		}
	})
}

// Done 返回关闭信号，供测试等待。
func (this *Server) Done() <-chan struct{} { return this.done }

// 监听 message 广播消息 channel 的协程，一旦有消息就发给全部在线 user
func (this *Server) ListenMessager() {
	for {
		select {
		case <-this.done:
			return
		case msg := <-this.Message:
			// 先复制一份用户列表再发送，避免持锁做 IO
			this.mapLock.RLock()
			users := make([]*User, 0, len(this.OnlineMap))
			for _, cli := range this.OnlineMap {
				users = append(users, cli)
			}
			this.mapLock.RUnlock()

			for _, cli := range users {
				// 非阻塞投递：慢客户端只会丢自己的消息，不会拖住整个广播
				cli.TrySend(msg)
			}
		}
	}
}

// 广播消息方法
func (this *Server) BroadCast(user *User, msg string) {
	sendMsg := "[" + user.Name + "]:" + msg
	log.Println(sendMsg)
	select {
	case this.Message <- sendMsg:
	case <-this.done:
	}
}

func (this *Server) Handler(conn net.Conn) {
	log.Printf("链接成功: %s", conn.RemoteAddr())
	defer conn.Close()

	user := NewUser(conn, this)
	user.Online()

	// 监听用户是否活跃的 channel
	isLive := make(chan bool, 1)

	// 接受客户端发送的消息
	go func() {
		// 允许 4096 字节内容加上 CRLF；ReadSlice 遇到更长的行会返回 ErrBufferFull。
		reader := bufio.NewReaderSize(conn, maxMessageBytes+2)
		for {
			line, err := reader.ReadSlice('\n')
			if err != nil {
				if errors.Is(err, bufio.ErrBufferFull) {
					user.SendMsg("消息过长\n")
				} else if !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
					log.Printf("conn read err: %v", err)
				}
				user.Close()
				return
			}
			msg := strings.TrimSuffix(string(line[:len(line)-1]), "\r")
			if len(msg) > maxMessageBytes {
				user.SendMsg("消息过长\n")
				user.Close()
				return
			}
			user.DoMessage(msg)
			// 用户任意操作，说明活跃
			select {
			case isLive <- true:
			default:
			}
		}
	}()

	timer := time.NewTimer(this.IdleTimeout)
	defer timer.Stop()
	for {
		select {
		case <-isLive:
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(this.IdleTimeout)
		case <-timer.C:
			user.SendMsg("你被踢出\n")
			user.Close()
			return
		case <-user.done:
			return
		case <-this.done:
			user.SendMsg("服务器正在关闭\n")
			user.Close()
			return
		}
	}
}
