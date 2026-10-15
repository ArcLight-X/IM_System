package main

import "sync"

// History 是固定容量的聊天记录环形缓冲，供 /summary 与 AI 上下文使用。
// 容量满时丢弃最旧的一条，避免内存无限增长。
type History struct {
	mu       sync.Mutex
	lines    []string
	capacity int
}

// NewHistory 创建聊天记录缓冲；capacity <= 0 时使用默认值 200。
func NewHistory(capacity int) *History {
	if capacity <= 0 {
		capacity = 200
	}
	return &History{
		lines:    make([]string, 0, capacity),
		capacity: capacity,
	}
}

// Add 追加一条记录。
func (h *History) Add(line string) {
	if h == nil || line == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.lines) >= h.capacity {
		copy(h.lines, h.lines[1:])
		h.lines[len(h.lines)-1] = line
		return
	}
	h.lines = append(h.lines, line)
}

// Recent 返回最近的 n 条记录（按时间顺序）。
func (h *History) Recent(n int) []string {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if n <= 0 || n > len(h.lines) {
		n = len(h.lines)
	}
	out := make([]string, n)
	copy(out, h.lines[len(h.lines)-n:])
	return out
}

// Len 返回当前记录条数。
func (h *History) Len() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.lines)
}

// contextHistory 保留最近的聊天事件；私聊仅对参与连接可见。
type contextHistory struct {
	mu       sync.Mutex
	entries  []contextEntry
	capacity int
}

type contextEntry struct {
	line      string
	sender    *User
	recipient *User // nil 表示公聊
}

func newContextHistory(capacity int) *contextHistory {
	if capacity <= 0 {
		capacity = 200
	}
	return &contextHistory{entries: make([]contextEntry, 0, capacity), capacity: capacity}
}

func (h *contextHistory) add(line string, sender, recipient *User) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.entries) == h.capacity {
		copy(h.entries, h.entries[1:])
		h.entries[len(h.entries)-1] = contextEntry{line, sender, recipient}
		return
	}
	h.entries = append(h.entries, contextEntry{line, sender, recipient})
}

func (h *contextHistory) recentFor(user *User, n int) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	lines := make([]string, 0, n)
	for i := len(h.entries) - 1; i >= 0 && len(lines) < n; i-- {
		e := h.entries[i]
		if e.recipient == nil || e.sender == user || e.recipient == user {
			lines = append(lines, e.line)
		}
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines
}
