# IM System（含 AI 助手）

基于 Go 与 TCP 网络编程实现的多用户即时通讯系统，支持公聊、私聊、改名、在线用户查询，
并集成了 LLM 助手：`/ai` 问答与 `/summary` 群聊摘要。

## 功能

| 命令 | 说明 |
| --- | --- |
| 直接输入内容 | 公聊（广播给所有在线用户） |
| `to\|用户名\|内容` | 私聊 |
| `rename\|新用户名` | 改名 |
| `who` | 查询在线用户 |
| `/ai 问题` | 向 AI 助手提问，自动带上最近聊天记录作为上下文 |
| `/summary [条数]` | 总结最近 N 条聊天记录（默认 20） |
| `/help` | 命令列表 |

## 目录结构

```
client/          终端客户端
server/          TCP 服务端与 AI 处理器
  server.go      连接管理、广播、空闲踢出、优雅关闭
  user.go        用户与命令处理
  ai.go          prompt 组装、上下文裁剪、超时重试、缓存、限流
  history.go     固定容量聊天记录缓冲
  webui.go       网页版客户端（SSE + POST 转发到 TCP）
  webui/         页面资源（index.html / app.js / style.css）
internal/llm/    模型调用层（接口 + OpenAI 兼容 HTTP 实现）
eval/            黄金用例集
cmd/aieval/      评测运行器
```

## AI 部分的几个设计点

**接口抽象。** 业务代码只依赖 `llm.Client` 接口，生产实现走 HTTP，测试注入 fake
或 `httptest` 假服务端。模型输出因此变得可复现——100+ 条用例全部离线运行，不依赖外部服务。

**慢请求不阻塞主流程。** 模型调用耗时以秒计，若在消息分发循环里同步等待，会拖垮所有用户。
`/ai` 在独立 goroutine 中执行，结果通过带锁的写通道回传；同时用信号量限制并发在途请求数，
并用对应用户的 `TryLock` 拒绝重复提问（提示「上一个问题还在处理中」）。

**超时、重试与降级。** 单次调用由 `context.WithTimeout` 约束，只对可重试错误
（429、5xx、超时、空响应）重试；最终失败时返回降级文案而不是让用户空等。

**上下文预算与裁剪。** `TrimHistory` 按 token 预算保留最近消息，system prompt 永远保留，
预算不足时丢弃最旧的消息、至少保留最新一条。`estimateTokens` 按「CJK 1 字 ≈ 1 token，
其他 4 字符 ≈ 1 token」估算。

**缓存与限流。** 相同问题在 TTL 内命中缓存，不消耗模型额度；每用户滑动窗口限流（默认 10 次/分钟），
限流发生在缓存判定之后。`Stats()` 暴露请求数、缓存命中、降级次数、重试次数、token 消耗与累计延迟。

**广播不阻塞。** 广播前先复制用户列表再发送，避免持锁做 IO；每个用户的消息缓冲满了就丢弃，
慢客户端只会丢自己的消息，不会拖住整个广播。

**写操作串行化。** 广播、命令回复与 AI 异步回复可能来自不同 goroutine，
所有 `conn.Write` 都经过 `User.writeMu`，避免并发写同一连接。

## 快速开始

```powershell
# 1) 配置模型（不配也能跑，只是 /ai 不可用）
$env:LLM_API_KEY  = "sk-xxx"
$env:LLM_BASE_URL = "https://api.openai.com/v1"   # 可选，兼容任意 OpenAI 格式接口
$env:LLM_MODEL    = "gpt-4o-mini"                 # 可选

# 2) 启动服务端
go run ./server

# 3) 启动客户端（可开多个终端）
go run ./client -name alice
```

## 网页版客户端（图形界面）

不想用命令行时，可以用浏览器当客户端：启动服务端后打开 `http://127.0.0.1:8080/`，
**每个浏览器窗口就是一个独立客户端**（多开窗口即可模拟多个用户），并且与终端客户端、
AI 助手完全互通。

```powershell
$env:WEB_ADDR = "127.0.0.1:8080"   # 可选，默认就是它
go run ./server                     # 同时开启 TCP 8888 与网页 8080
```

界面上可以切换「公聊 / 私聊 / AI 问答」，并有在线用户、改名、群聊摘要、断开等按钮；
点页面右上角「新窗口」会再开一个客户端窗口。

实现原理：浏览器用 SSE（`GET /events`）接收服务端推送、用 POST（`/send`）发送消息，
服务端内置的转发层把两者桥接到 8888 的 TCP 协议上，因此 **TCP 服务端核心逻辑零改动**，
网页客户端与终端客户端、AI 功能天然互通。前端资源用 `//go:embed` 打进二进制。

## 测试

```powershell
go test ./... -count=1            # 全部测试
go test ./server -run TestAIConcurrentRouting -v
go test ./server ./internal/llm -cover
go test ./... -race               # 需要较新的 MinGW-w64（见下方限制）
```

覆盖范围：

- `internal/llm`：正常响应、429/500/400、非法 JSON、空 choices、空 content、超时、环境变量配置。
- `server`：token 估算与上下文裁剪、prompt 组装、缓存命中、重试次数、不可重试错误、
  超时降级、限流、并发上限、未配置降级。
- `server` 集成测试（真实 TCP）：12 个客户端并发提问且回答不串号、畸形命令不崩溃、
  慢客户端不阻塞广播、未配置 AI 时的提示、空闲踢出、优雅关闭。

## 输出评测（eval）

固定黄金用例集，对模型输出做自动判分，覆盖正常、边界（空输入/英文/混合意图）与
prompt 注入三类场景：

```powershell
$env:LLM_API_KEY = "sk-xxx"
go run ./cmd/aieval -v
```

判分规则：输出必须是合法 JSON（容忍 Markdown 代码块包裹）、`category` 命中期望值或候选集合、
且不得泄露系统提示词。任一用例失败进程返回 1，便于接入 CI 作为 prompt 回归门禁。

## 已知限制

- 服务端用户与聊天记录都在内存中，重启即丢失；生产化需要落库。
- `go test -race` 需要较新的 MinGW-w64。若 PATH 中是 Dev-Cpp 自带的旧版 gcc，
  会因为缺少 `WaitOnAddress` 等 Windows 8+ API 而链接失败；换用较新的 MinGW-w64，
  或在 WSL/Linux 下运行即可。
- 未实现流式输出（`stream: true`），长回答会让用户等待整段生成完成。
