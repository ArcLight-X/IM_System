package main

import (
	"bufio"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// 网页版客户端：把浏览器窗口变成 IM 客户端。
//
// 每个页面（窗口）打开时向 TCP 服务端建立一条独立连接，
// 服务端到浏览器的推送走 SSE（GET /events），浏览器发言走 POST（/send）。
// 因此「一个窗口 = 一个客户端」：多开窗口即可模拟多个用户，
// 并且与终端客户端、AI 功能完全互通，TCP 服务端核心无需改动。

// 默认的网页版监听地址。
const defaultWebAddr = "127.0.0.1:8080"

//go:embed webui
var webuiAssets embed.FS

// WebUI 负责在浏览器与 TCP 服务端之间转发消息。
type WebUI struct {
	dialAddr string

	mu       sync.Mutex
	sessions map[string]*webSession
}

type webSession struct {
	id   string
	conn net.Conn
	mu   sync.Mutex
}

var webSessionSeq uint64

// NewWebUI 创建转发层，dialAddr 是 TCP 服务端地址（如 127.0.0.1:8888）。
func NewWebUI(dialAddr string) *WebUI {
	return &WebUI{dialAddr: dialAddr, sessions: make(map[string]*webSession)}
}

// Handler 返回网页客户端的 HTTP 路由。
func (w *WebUI) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/events", w.handleEvents)
	mux.HandleFunc("/send", w.handleSend)

	sub, err := fs.Sub(webuiAssets, "webui")
	if err != nil {
		log.Printf("web ui assets error: %v", err)
		mux.HandleFunc("/", func(rw http.ResponseWriter, _ *http.Request) {
			http.Error(rw, "web ui assets missing", http.StatusInternalServerError)
		})
		return mux
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}

// startWebUI 由 Server.Start 调用，在 s.webAddr 上启动网页版客户端。
func (s *Server) startWebUI() {
	ui := NewWebUI(net.JoinHostPort(dialHost(s.Ip), strconv.Itoa(s.Port)))
	ln, err := net.Listen("tcp", s.webAddr)
	if err != nil {
		log.Printf("web ui listen error: %v", err)
		return
	}
	srv := &http.Server{Handler: ui.Handler()}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("web ui serve error: %v", err)
		}
	}()
	// 跟随服务器一起优雅关闭
	go func() {
		<-s.done
		_ = srv.Close()
	}()
	log.Printf("网页版客户端已启动: http://%s/  （每个浏览器窗口 = 一个客户端）", browserAddr(s.webAddr))
}

// handleEvents 为一个浏览器窗口建立推送通道（SSE）。
func (w *WebUI) handleEvents(rw http.ResponseWriter, req *http.Request) {
	flusher, ok := rw.(http.Flusher)
	if !ok {
		http.Error(rw, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	conn, err := net.Dial("tcp", w.dialAddr)
	if err != nil {
		http.Error(rw, "无法连接聊天服务器: "+err.Error(), http.StatusBadGateway)
		return
	}

	sess := w.addSession(conn)
	defer w.closeSession(sess)

	rw.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("X-Accel-Buffering", "no")
	rw.WriteHeader(http.StatusOK)

	name := strings.TrimSpace(req.URL.Query().Get("name"))
	if err := writeSSE(rw, "init", fmt.Sprintf(`{"session":%q,"name":%q}`, sess.id, name)); err != nil {
		return
	}
	flusher.Flush()

	if name != "" {
		if err := sess.write("rename|" + name); err != nil {
			return
		}
	}

	// 浏览器关闭页面时唤醒下面的读循环
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-req.Context().Done():
		case <-stop:
		}
		_ = conn.Close()
	}()

	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			if writeSSE(rw, "message", line) != nil {
				return
			}
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}

type sendRequest struct {
	Session string `json:"session"`
	Text    string `json:"text"`
}

// handleSend 把浏览器输入的一行命令转发给对应的 TCP 连接。
func (w *WebUI) handleSend(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		rw.Header().Set("Allow", http.MethodPost)
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var in sendRequest
	if err := json.NewDecoder(io.LimitReader(req.Body, 1<<16)).Decode(&in); err != nil {
		http.Error(rw, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	sess := w.getSession(in.Session)
	if sess == nil {
		http.Error(rw, "session not found", http.StatusNotFound)
		return
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		http.Error(rw, "empty message", http.StatusBadRequest)
		return
	}
	if err := sess.write(text); err != nil {
		http.Error(rw, "send failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	rw.WriteHeader(http.StatusNoContent)
}

func (w *WebUI) addSession(conn net.Conn) *webSession {
	sess := &webSession{id: newSessionID(), conn: conn}
	w.mu.Lock()
	w.sessions[sess.id] = sess
	w.mu.Unlock()
	return sess
}

func (w *WebUI) getSession(id string) *webSession {
	if id == "" {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sessions[id]
}

func (w *WebUI) closeSession(sess *webSession) {
	w.mu.Lock()
	delete(w.sessions, sess.id)
	w.mu.Unlock()
	_ = sess.conn.Close()
}

func (s *webSession) write(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.Write([]byte(text + "\n"))
	return err
}

// writeSSE 输出一条 SSE 事件；data 中的换行会拆成多行 data:。
func writeSSE(w io.Writer, event, data string) error {
	var sb strings.Builder
	if event != "" {
		sb.WriteString("event: ")
		sb.WriteString(event)
		sb.WriteString("\n")
	}
	for _, line := range strings.Split(data, "\n") {
		sb.WriteString("data: ")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	_, err := io.WriteString(w, sb.String())
	return err
}

func newSessionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("sess-%d", atomic.AddUint64(&webSessionSeq, 1))
}

// dialHost 把监听用的通配地址换成可用于本地拨号的地址。
func dialHost(ip string) string {
	switch ip {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	}
	return ip
}

// browserAddr 把 0.0.0.0:8080 之类改写成浏览器可访问的地址，仅用于日志展示。
func browserAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return net.JoinHostPort(dialHost(host), port)
}
