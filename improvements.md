# IM 系统改进清单

---

## 🔴 严重问题（Bug级）

### 1. 内存泄漏：`time.After` 在循环中重复创建

**文件**: `server/server.go` 第 89 行

```go
case <-time.After(time.Second * 100):
```

**问题**: 每次 select 循环都创建新的 Timer，用户活跃时旧 Timer 堆积直到超时才被 GC。

**修复**: 改用 `time.NewTimer` + `Reset()`：

```go
timer := time.NewTimer(time.Second * 100)
defer timer.Stop()
for {
    select {
    case <-isLive:
        if !timer.Stop() {
            <-timer.C
        }
        timer.Reset(time.Second * 100)
    case <-timer.C:
        user.SendMsg("你被踢出")
        close(user.C)
        conn.Close()
        return
    }
}
```

---

### 2. 客户端断连后主循环无感知

**文件**: `client/client.go` 第 39-41 行、第 136-148 行

**问题**: `DealResponse()` 的 `io.Copy` 在服务端断开后退出，但 `Run()` 主循环完全不知道，菜单照常显示。

**修复**: 通过 channel 通知主循环连接已断开，自动退出或显示提示。

---

### 3. 非 EOF 读取错误时未清理用户

**文件**: `server/server.go` 第 69-73 行

```go
if err != nil && err != io.EOF {
    fmt.Println("conn read err", err)
    return  // ← 没有调用 user.Offline()
}
```

**问题**: Read 返回非 EOF 错误直接 return，用户残留在 OnlineMap 中成为"僵尸用户"。

**修复**: 在 return 前补上 `user.Offline()`。

---

## 🟡 设计问题

### 4. 私聊消息多余换行符

**文件**: `client/client.go` 第 101 行

```go
sendMsg := "to|" + remoteName + "|" + chatMsg + "\n\n"
```

**问题**: 两个 `\n`，服务端 `buf[:n-1]` 只去掉一个，导致消息末尾多空行。

**修复**: 改为 `"\n"`。

---

### 5. Client 结构体 `x` 字段语义混乱

**文件**: `client/client.go` 第 19 行、第 45 行

**问题**: `x` 初始化为 999 无意义，`menu()` 内部用局部变量 `var x int` 再赋值给 `client.x`。

**修复**: 直接在 `menu()` 中用 `fmt.Scanln(&client.x)`。

---

### 6. 服务端缺少优雅关闭

**文件**: `server/server.go` 第 101-125 行

**问题**: 只能 Ctrl+C 强制退出，无法优雅通知用户、清理资源。

**修复**: 监听 `os.Signal`，收到信号后关闭 listener、通知在线用户、等待协程退出。

---

## 🟢 体验改进

### 7. 缺少"查询在线用户"菜单入口

**文件**: `client/client.go`

**问题**: `who` 命令只在私聊时自动调用，用户无法主动查询。

**修复**: 在菜单添加选项 `4.查询在线用户`，发送 `"who\n"`。

---

### 8. 广播消息格式优化

**文件**: `server/server.go` 第 51 行

```go
sendMsg := "[" + user.Addr + "]" + user.Name + ":" + msg
```

**问题**: 改名后 Addr 和 Name 均显示，冗余。

**修复**: 直接显示 `[Alice]: 大家好`。

---

### 9. 超时时间硬编码

**文件**: `server/server.go` 第 89 行

**问题**: 100 秒硬编码。

**修复**: 在 Server 结构体中添加可配置字段 `IdleTimeout time.Duration`。

---

### 10. 客户端不支持命令行指定用户名

**文件**: `client/client.go`

**问题**: 每次改名都要进菜单操作。

**修复**: 添加 `-name` flag：

```go
flag.StringVar(&client.Name, "name", "", "设置用户名")
```

---

## 修复优先级建议

| 顺序 | 问题 | 理由 |
|------|------|------|
| 1 | #3 僵尸用户 | 数据正确性 |
| 2 | #1 内存泄漏 | 长期运行稳定性 |
| 3 | #4 多余换行 | 消息格式 |
| 4 | #2 断连感知 | 用户体验 |
| 5 | #5 字段混淆 | 代码整洁 |
| 6 | #7 菜单缺失 | 功能完整 |
| 7 | #8 格式优化 | 用户体验 |
| 8 | #9 硬编码 | 可配置性 |
| 9 | #10 启动参数 | 便捷性 |
| 10 | #6 优雅关闭 | 生产级特性 |