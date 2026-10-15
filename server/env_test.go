package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	contents := "# 注释\n\nLLM_API_KEY = 'file-secret'\nLLM_MODEL=\"file-model\"\nLLM_TIMEOUT=20s\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLM_API_KEY", "system-secret")
	t.Setenv("LLM_MODEL", "system-model")
	// 确保测试不依赖运行环境中的变量。
	old, existed := os.LookupEnv("LLM_TIMEOUT")
	_ = os.Unsetenv("LLM_TIMEOUT")
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv("LLM_TIMEOUT", old)
		} else {
			_ = os.Unsetenv("LLM_TIMEOUT")
		}
	})
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LLM_API_KEY") != "system-secret" || os.Getenv("LLM_MODEL") != "system-model" {
		t.Fatal(".env 不应覆盖系统环境变量")
	}
	if os.Getenv("LLM_TIMEOUT") != "20s" {
		t.Fatal("未设置的环境变量应从 .env 加载")
	}
}

func TestLoadEnvFileMissingAndInvalid(t *testing.T) {
	if err := loadEnvFile(filepath.Join(t.TempDir(), "missing.env")); err != nil {
		t.Fatalf("文件不存在时不应报错: %v", err)
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("invalid-line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err == nil || !strings.Contains(err.Error(), "KEY=VALUE") {
		t.Fatalf("错误格式应报告行号和格式要求: %v", err)
	}
}
