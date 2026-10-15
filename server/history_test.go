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
