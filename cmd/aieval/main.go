// Command aieval 对模型输出做可重复的评测：
// 固定黄金用例集 → 批量调用 → 自动判分 → 输出通过率报告。
//
// 用法（在仓库根目录）：
//
//	$env:LLM_API_KEY="sk-xxx"
//	go run ./cmd/aieval
//	go run ./cmd/aieval -cases eval/cases.json -v
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"server-demo/internal/llm"
)

type evalCase struct {
	ID             string   `json:"id"`
	Kind           string   `json:"kind"`
	Input          string   `json:"input"`
	ExpectCategory string   `json:"expect_category"`
	ExpectAny      []string `json:"expect_any"`
	MustNotContain []string `json:"must_not_contain"`
}

type suite struct {
	System string     `json:"system"`
	Cases  []evalCase `json:"cases"`
}

type result struct {
	evalCase
	pass     bool
	reason   string
	category string
	raw      string
	elapsed  time.Duration
}

func main() {
	casesPath := flag.String("cases", "eval/cases.json", "黄金用例集路径")
	verbose := flag.Bool("v", false, "打印每个用例的原始输出")
	timeout := flag.Duration("timeout", 40*time.Second, "单条用例超时")
	flag.Parse()

	data, err := os.ReadFile(*casesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取用例集失败：%v\n", err)
		os.Exit(2)
	}
	var s suite
	if err := json.Unmarshal(data, &s); err != nil {
		fmt.Fprintf(os.Stderr, "解析用例集失败：%v\n", err)
		os.Exit(2)
	}
	if len(s.Cases) == 0 {
		fmt.Fprintln(os.Stderr, "用例集为空")
		os.Exit(2)
	}

	client, err := llm.FromEnv()
	if err != nil {
		// 没有配置 API Key 时视为跳过（便于在 CI 中无密钥运行）
		fmt.Printf("跳过评测：%v。请先设置 LLM_API_KEY。\n", err)
		return
	}

	fmt.Printf("用例集：%s（%d 条）\n", *casesPath, len(s.Cases))
	fmt.Println(strings.Repeat("-", 78))

	results := make([]result, 0, len(s.Cases))
	for _, c := range s.Cases {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		start := time.Now()
		out, err := client.Complete(ctx, []llm.Message{
			{Role: llm.RoleSystem, Content: s.System},
			{Role: llm.RoleUser, Content: c.Input},
		})
		cancel()
		elapsed := time.Since(start)

		r := evaluate(c, out, err)
		r.elapsed = elapsed
		results = append(results, r)

		status := "PASS"
		if !r.pass {
			status = "FAIL"
		}
		fmt.Printf("%-14s %-10s %-4s %-6s %s\n", c.ID, c.Kind, status, r.category, truncate(r.reason, 40))
		if *verbose && r.raw != "" {
			fmt.Printf("    输出: %s\n", truncate(strings.ReplaceAll(r.raw, "\n", " "), 120))
		}
	}

	printSummary(results, *verbose)

	for _, r := range results {
		if !r.pass {
			os.Exit(1)
		}
	}
}

// evaluate 对单条输出判分。判分只依赖明确的规则，避开人为主观判断。
func evaluate(c evalCase, out string, err error) result {
	r := result{evalCase: c, raw: out}
	if err != nil {
		r.reason = "请求失败: " + err.Error()
		return r
	}

	parsed, parseErr := parseClassification(out)
	if parseErr != nil {
		r.reason = "输出不是合法 JSON: " + parseErr.Error()
		return r
	}
	r.category = parsed.Category

	if c.ExpectCategory != "" && parsed.Category != c.ExpectCategory {
		r.reason = fmt.Sprintf("分类应为 %s，实际 %s", c.ExpectCategory, parsed.Category)
		return r
	}
	if len(c.ExpectAny) > 0 && !contains(c.ExpectAny, parsed.Category) {
		r.reason = fmt.Sprintf("分类应属于 %v，实际 %s", c.ExpectAny, parsed.Category)
		return r
	}
	lower := strings.ToLower(out)
	for _, bad := range c.MustNotContain {
		if strings.Contains(lower, strings.ToLower(bad)) {
			r.reason = fmt.Sprintf("输出泄露了不该出现的内容: %q", bad)
			return r
		}
	}

	r.reason = "OK"
	r.pass = true
	return r
}

type classification struct {
	Category   string  `json:"category"`
	Confidence float64 `json:"confidence"`
}

// parseClassification 容忍模型用 Markdown 代码块包裹 JSON 的常见情况。
func parseClassification(out string) (classification, error) {
	var c classification
	s := strings.TrimSpace(out)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return c, err
	}
	if c.Category == "" {
		return c, fmt.Errorf("缺少 category 字段")
	}
	return c, nil
}

func printSummary(results []result, verbose bool) {
	byKind := map[string]struct{ pass, total int }{}
	passed, totalElapsed := 0, time.Duration(0)
	for _, r := range results {
		v := byKind[r.Kind]
		v.total++
		if r.pass {
			v.pass++
			passed++
		}
		byKind[r.Kind] = v
		totalElapsed += r.elapsed
	}

	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	fmt.Println(strings.Repeat("-", 78))
	for _, k := range kinds {
		v := byKind[k]
		fmt.Printf("%-10s %d/%d  通过率 %.0f%%\n", k, v.pass, v.total, float64(v.pass)/float64(v.total)*100)
	}
	fmt.Printf("%-10s %d/%d  通过率 %.0f%%\n", "合计", passed, len(results), float64(passed)/float64(len(results))*100)
	if len(results) > 0 {
		fmt.Printf("平均耗时 %v/条\n", (totalElapsed / time.Duration(len(results))).Round(time.Millisecond))
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
