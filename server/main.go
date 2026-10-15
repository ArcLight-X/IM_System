package main

import (
	"log"
	"os"

	"server-demo/internal/llm"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := loadEnvFile(".env"); err != nil {
		log.Printf("读取 .env 失败：%v", err)
	}

	opts := []ServerOption{
		WithHistoryCapacity(200),
		// 网页版客户端：浏览器每个窗口 = 一个客户端；设置 WEB_ADDR 可改地址
		WithWebUI(os.Getenv("WEB_ADDR")),
	}

	// 未配置 API Key 时不影响普通聊天，只是 AI 功能不可用
	client, err := llm.FromEnv()
	if err != nil {
		log.Printf("AI 功能未启用：%v", err)
	} else {
		opts = append(opts, WithAIHandler(NewAIHandler(client)))
		log.Println("AI 功能已启用，支持 /ai、/polish 与 /summary")
	}

	server := NewServer("127.0.0.1", 8888, opts...)
	log.Println("IM 服务器已启动 127.0.0.1:8888")
	server.Start()
}
