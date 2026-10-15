package main

import (
	"fmt"
	"sync"
	"testing"
)

func TestHistoryKeepsCapacity(t *testing.T) {
	h := NewHistory(3)
	for i := 1; i <= 5; i++ {
		h.Add(fmt.Sprintf("消息%d", i))
	}
	if got := h.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}
	got := h.Recent(3)
	want := []string{"消息3", "消息4", "消息5"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Recent[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHistoryRecent(t *testing.T) {
	h := NewHistory(10)
	for i := 1; i <= 5; i++ {
		h.Add(fmt.Sprintf("消息%d", i))
	}

	tests := []struct {
		name string
		n    int
		want int
	}{
		{"取最近 2 条", 2, 2},
		{"n 超过总数时返回全部", 100, 5},
		{"n <= 0 时返回全部", 0, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(h.Recent(tt.n)); got != tt.want {
				t.Errorf("len = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestHistoryIgnoresEmptyAndNil(t *testing.T) {
	var nilHistory *History
	nilHistory.Add("x") // 不应 panic
	if got := nilHistory.Recent(1); got != nil {
		t.Errorf("nil History 应返回 nil，实际 %v", got)
	}

	h := NewHistory(5)
	h.Add("")
	if h.Len() != 0 {
		t.Errorf("空字符串不应记录，Len = %d", h.Len())
	}
}

func TestHistoryConcurrentAccess(t *testing.T) {
	h := NewHistory(100)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			h.Add(fmt.Sprintf("并发写入%d", i))
		}(i)
		go func() {
			defer wg.Done()
			_ = h.Recent(5)
		}()
	}
	wg.Wait()
}

func TestContextHistoryPrivateVisibilityAndOrder(t *testing.T) {
	h := newContextHistory(3)
	alice := &User{Name: "alice"}
	bob := &User{Name: "bob"}
	charlie := &User{Name: "charlie"}
	h.add("[公聊] 开始", alice, nil)
	h.add("[私聊] alice对bob说：只给双方", alice, bob)
	h.add("[公聊] 结束", charlie, nil)

	got := h.recentFor(bob, 3)
	if len(got) != 3 || got[0] != "[公聊] 开始" || got[1] != "[私聊] alice对bob说：只给双方" || got[2] != "[公聊] 结束" {
		t.Fatalf("参与者上下文顺序错误: %v", got)
	}
	if got := h.recentFor(charlie, 3); len(got) != 2 || got[0] != "[公聊] 开始" || got[1] != "[公聊] 结束" {
		t.Fatalf("第三人不应看到私聊: %v", got)
	}
	// 改名不会影响当前连接的可见性，别人复用原昵称也不能读取该私聊。
	bob.Name = "new-bob"
	oldNameOwner := &User{Name: "bob"}
	if got := h.recentFor(bob, 3); len(got) != 3 {
		t.Fatalf("改名后失去私聊上下文: %v", got)
	}
	if got := h.recentFor(oldNameOwner, 3); len(got) != 2 {
		t.Fatalf("复用昵称的用户看到了私聊: %v", got)
	}
	h.add("[公聊] 新消息", alice, nil)
	if got := h.recentFor(bob, 3); len(got) != 3 || got[0] != "[私聊] alice对bob说：只给双方" || got[1] != "[公聊] 结束" || got[2] != "[公聊] 新消息" {
		t.Fatalf("容量满后应丢弃最旧消息: %v", got)
	}
}
