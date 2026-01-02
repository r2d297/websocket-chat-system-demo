# Handler 实现详解 / Handler Implementation Details

> **文件 / File**: `internal/gateway/handler.go`
> **作用 / Purpose**: WebSocket 连接处理和业务逻辑层 / WebSocket connection handling and business logic layer

## 目录 / Table of Contents

- [整体架构 / Overall Architecture](#整体架构--overall-architecture)
- [核心常量定义 / Core Constants](#核心常量定义--core-constants)
- [消息协议设计 / Message Protocol](#消息协议设计--message-protocol)
- [核心流程详解 / Core Processes](#核心流程详解--core-processes)
- [注册流程 / Registration Flow](#注册流程详解--registration-flow)
- [心跳机制 / Heartbeat Mechanism](#心跳机制详解--heartbeat-mechanism)
- [消息路由 / Message Routing](#消息路由详解--message-routing)
- [连接清理 / Connection Cleanup](#连接清理流程--connection-cleanup)
- [并发安全分析 / Concurrency Safety](#并发安全分析--concurrency-safety)
- [错误处理策略 / Error Handling](#错误处理策略--error-handling)

---

## 整体架构 / Overall Architecture

### handler.go 在系统中的位置 / Position in System

```
┌─────────────────────────────────────────────────┐
│                   Client                        │
└──────────────────┬──────────────────────────────┘
                   │ WebSocket
                   ▼
┌─────────────────────────────────────────────────┐
│              handler.go                         │
│  • 连接管理（注册/断开）/ Connection management  │
│  • 消息路由（本地/跨网关）/ Message routing      │
│  • 心跳检测 / Heartbeat detection              │
│  • 错误处理 / Error handling                   │
└──────┬──────────────┬───────────────────────────┘
       │              │
       ▼              ▼
┌──────────────┐  ┌──────────────┐
│ connection.go│  │  router.go   │
│ (本地状态)    │  │ (跨网关通信)  │
│ Local state  │  │ Cross-gateway│
└──────┬───────┘  └──────┬───────┘
       │                 │
       ▼                 ▼
┌──────────────┐  ┌──────────────┐
│presence.go   │  │  Redis       │
│(全局状态)    │  │  Pub/Sub     │
│ Global state │  │              │
└──────────────┘  └──────────────┘
```

---

## 核心常量定义 / Core Constants

### 心跳与消息类型常量 / Heartbeat and Message Type Constants (handler.go:14-25)

```go
const (
    // Heartbeat settings / 心跳设置
    heartbeatInterval = 30 * time.Second   // 客户端心跳间隔 / Client heartbeat interval
    heartbeatTimeout  = 90 * time.Second   // 超时阈值（3倍心跳）/ Timeout threshold (3x heartbeat)

    // Message types / 消息类型
    msgTypePing     = "ping"
    msgTypePong     = "pong"
    msgTypeMessage  = "message"
    msgTypeRegister = "register"
    msgTypeError    = "error"
)
```

### 为什么心跳超时是 3 倍间隔？/ Why is timeout 3x interval?

**正常情况 / Normal Case:**
```
T0: Client 发送 ping / sends ping
T30: Client 发送 ping / sends ping
T60: Client 发送 ping / sends ping
T90: Client 发送 ping / sends ping  ← Gateway 最多等到这里 / Gateway waits until here
```

**异常情况（网络抖动）/ Abnormal Case (network jitter):**
```
T0: Client 发送 ping（成功）/ sends ping (success)
T30: Client 发送 ping（丢包）/ sends ping (packet loss) ← 允许丢失 1 次 / Allow 1 loss
T60: Client 发送 ping（丢包）/ sends ping (packet loss) ← 允许丢失 2 次 / Allow 2 losses
T90: 仍未收到 → 判定超时 / Still not received → timeout  ← 容忍度 = 2 次丢包 / Tolerance = 2 losses
```

**3 倍是业界标准 / 3x is industry standard**，平衡了容错性和及时性 / Balances fault tolerance and timeliness：
- 2 倍 / 2x：太激进，网络抖动容易误杀 / Too aggressive, network jitter causes false positives
- 4 倍 / 4x：太保守，僵尸连接占用资源 / Too conservative, zombie connections waste resources

---

## 消息协议设计 / Message Protocol

### ClientMessage - 客户端发送 / Client Sends (handler.go:28-33)

```go
type ClientMessage struct {
    Type    string `json:"type"`           // 消息类型 / Message type
    To      string `json:"to,omitempty"`   // 接收者（仅 message 类型）/ Receiver (message type only)
    Content string `json:"content,omitempty"` // 消息内容 / Message content
    UserID  string `json:"userId,omitempty"`  // 用户 ID（仅 register 类型）/ User ID (register type only)
}
```

**实际消息示例 / Actual Message Examples:**

```json
// 注册 / Register
{"type": "register", "userId": "alice"}

// 心跳 / Heartbeat
{"type": "ping"}

// 发送消息 / Send message
{"type": "message", "to": "bob", "content": "Hello!"}
```

### ServerMessage - 服务器发送 / Server Sends (handler.go:36-41)

```go
type ServerMessage struct {
    Type    string `json:"type"`
    From    string `json:"from,omitempty"`    // 发送者 / Sender
    Content string `json:"content,omitempty"` // 消息内容 / Message content
    Error   string `json:"error,omitempty"`   // 错误信息 / Error message
}
```

**实际响应示例 / Actual Response Examples:**

```json
// 注册成功 / Registration success
{"type": "registered", "content": "Successfully registered"}

// 心跳响应 / Heartbeat response
{"type": "pong"}

// 收到消息 / Received message
{"type": "message", "from": "alice", "content": "Hello!"}

// 错误 / Error
{"type": "error", "error": "User not found"}
```

---

## 核心流程：handleConnection() / Core Flow: handleConnection()

**这是整个 handler.go 的主入口 / This is the main entry point**，处理单个 WebSocket 连接的完整生命周期 / Handles complete lifecycle of a single WebSocket connection。

### 流程图 / Flow Diagram

```
WebSocket 连接建立 / Connection established
    ↓
handleConnection() 启动 / starts
    ↓
┌─────────────────────────┐
│  无限循环读取消息 / Infinite loop reading messages        │
│  conn.ReadMessage()     │
└─────────┬───────────────┘
          │
          ├─→ register → 注册用户 / Register user → 启动心跳检测 / Start heartbeat
          ├─→ ping     → 更新心跳 / Update heartbeat → 刷新 Redis TTL / Refresh Redis TTL
          ├─→ message  → 路由消息 / Route message → routeMessage()
          └─→ 其他 / other → 发送错误 / Send error
          │
    连接断开/错误 / Disconnect/error
          ↓
    清理资源（移除连接、删除 Presence）/ Cleanup (remove connection, delete Presence)
          ↓
handleConnection() 结束 / ends
```

### 初始化阶段 / Initialization Phase (handler.go:44-51)

```go
func (s *Server) handleConnection(conn *websocket.Conn, connID string) {
    defer conn.Close()  // ← 确保连接总会关闭 / Ensure connection always closes

    var userID string       // 用户 ID（初始为空，注册后赋值）/ User ID (empty initially, set after register)
    var wsConn *Connection  // Connection 对象（注册后创建）/ Connection object (created after register)

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()  // ← 确保 context 总会取消 / Ensure context always canceled
```

**关键设计 / Key Design:**

| 变量 / Variable | 初始值 / Initial | 赋值时机 / Set When | 作用 / Purpose |
|------|--------|---------|------|
| `userID` | `""` | 收到 register 消息 / Receive register | 标识用户身份 / Identify user |
| `wsConn` | `nil` | 注册成功后 / After registration | 管理连接状态 / Manage connection state |
| `ctx` | 新 Context / New | 函数开始 / Function start | 控制心跳检测 goroutine / Control heartbeat goroutine |

**为什么 userID 初始为空？/ Why is userID initially empty?**

```
未注册用户只能做两件事 / Unregistered users can only:
  1. 发送 register 消息 / Send register message
  2. 断开连接 / Disconnect

其他消息（ping/message）都需要先注册 / Other messages (ping/message) require registration first
```

### 消息读取循环 / Message Reading Loop (handler.go:54-68)

```go
for {
    _, message, err := conn.ReadMessage()  // ← 阻塞读取 / Blocking read
    if err != nil {
        if websocket.IsUnexpectedCloseError(err,
            websocket.CloseGoingAway,
            websocket.CloseAbnormalClosure) {
            log.Printf("[Handler] WebSocket error: %v", err)
        }
        break  // ← 任何错误都退出循环 / Any error exits loop
    }

    var msg ClientMessage
    if err := json.Unmarshal(message, &msg); err != nil {
        log.Printf("[Handler] Failed to unmarshal message: %v", err)
        s.sendError(conn, "Invalid message format")
        continue  // ← JSON 错误继续循环，连接不断开 / JSON error continues, connection stays
    }
```

**为什么区分 break 和 continue？/ Why distinguish break and continue?**

```
ReadMessage 错误 → break
  • 连接已断开，无法恢复 / Connection broken, cannot recover
  • 退出循环，执行清理逻辑 / Exit loop, execute cleanup

JSON 解析错误 → continue
  • 只是消息格式错误，连接仍有效 / Just format error, connection still valid
  • 发送错误消息，等待下一条消息 / Send error, wait for next message
```

**IsUnexpectedCloseError 的作用？/ Purpose of IsUnexpectedCloseError?**

```go
websocket.IsUnexpectedCloseError(err,
    websocket.CloseGoingAway,        // 1001: 浏览器标签关闭 / Browser tab closed
    websocket.CloseAbnormalClosure)  // 1006: 网络异常 / Network error
```

过滤掉**正常关闭**（1000），只记录**异常关闭**的日志。

Filters out **normal close** (1000), only logs **abnormal close**.

---

## 注册流程详解 / Registration Flow

### 完整流程 / Complete Flow

```
Step 1: 客户端发送 / Client sends {"type": "register", "userId": "alice"}
    ↓
Step 2: 验证 userID 非空 / Validate userID not empty
    ↓
Step 3: 创建 Connection 对象 / Create Connection object
    wsConn = NewConnection(connID, userID, conn)
    ↓
Step 4: 添加到本地连接管理器 / Add to local connection manager
    s.connMgr.Add(wsConn)
    ↓
Step 5: 注册到 Redis Presence（带 CAS）/ Register to Redis Presence (with CAS)
    s.presenceMgr.Register(ctx, userID, gatewayID, connID)
    ↓
Step 6: 发送注册成功响应 / Send registration success
    {"type": "registered", "content": "Successfully registered"}
    ↓
Step 7: 启动心跳检测 goroutine / Start heartbeat goroutine
    go s.heartbeatChecker(ctx, wsConn)
```

### 代码详解 / Code Explanation (handler.go:71-100)

```go
case msgTypeRegister:
    // ← 验证用户 ID / Validate user ID
    if msg.UserID == "" {
        s.sendError(conn, "UserID is required for registration")
        continue
    }

    userID = msg.UserID  // ← 保存到外层作用域 / Save to outer scope
    wsConn = NewConnection(connID, userID, conn)

    // ← Step 1: 本地状态 / Local state
    s.connMgr.Add(wsConn)

    // ← Step 2: 全局状态（Redis）/ Global state (Redis)
    if err := s.presenceMgr.Register(ctx, userID, s.gatewayID, connID); err != nil {
        log.Printf("[Handler] Failed to register presence: %v", err)
        s.sendError(conn, "Failed to register")
        continue  // ← Redis 失败不断开连接，允许重试 / Redis failure doesn't disconnect, allow retry
    }

    log.Printf("[Handler] User %s registered on gateway %s (connID: %s)",
               userID, s.gatewayID, connID)

    // ← 发送成功响应 / Send success response
    s.sendMessage(conn, ServerMessage{
        Type:    "registered",
        Content: "Successfully registered",
    })

    // ← 启动心跳检测（异步）/ Start heartbeat (async)
    go s.heartbeatChecker(ctx, wsConn)
```

### 关键设计决策 / Key Design Decisions

#### 为什么先更新本地，再更新 Redis？/ Why update local first, then Redis?

**顺序 A（当前实现）/ Order A (current):**
```
connMgr.Add() → presenceMgr.Register()

优点 / Advantages:
  ✅ Redis 失败时，本地已有连接，可以重试注册
  ✅ When Redis fails, local connection exists, can retry registration
  ✅ 心跳检测立即可用 / Heartbeat detection immediately available
```

**顺序 B（反过来）/ Order B (reversed):**
```
presenceMgr.Register() → connMgr.Add()

缺点 / Disadvantages:
  ❌ Redis 成功但 connMgr.Add() 失败 → Redis 有脏数据
  ❌ Redis succeeds but connMgr.Add() fails → dirty data in Redis
  ❌ 其他 Gateway 会路由消息过来，但本地找不到连接
  ❌ Other Gateways route messages here, but local connection not found
```

#### Redis 注册失败为什么用 continue 而不是 break？/ Why continue instead of break on Redis failure?

```go
if err := s.presenceMgr.Register(...); err != nil {
    s.sendError(conn, "Failed to register")
    continue  // ← 不是 break / Not break
}
```

**原因 / Reason:** Redis 可能是**临时故障**（网络抖动）/ Redis might be **temporary failure** (network jitter)，客户端可以重试 / client can retry：

```
Client 行为 / Client behavior:
  T0: 发送 register / Send register → 收到 error / Receive error
  T1: 重试 register / Retry register → 成功 / Success
```

如果用 `break`，连接直接断开，体验很差。

If using `break`, connection disconnects immediately, bad UX.

---

## 心跳机制详解 / Heartbeat Mechanism

### 两个组成部分 / Two Components

#### 1. 客户端发送 ping / Client Sends Ping (handler.go:102-114)

```go
case msgTypePing:
    // ← 更新本地连接的最后心跳时间 / Update local connection's last heartbeat time
    if wsConn != nil {
        wsConn.UpdatePing()

        // ← 刷新 Redis Presence TTL（90s）/ Refresh Redis Presence TTL (90s)
        if err := s.presenceMgr.Refresh(ctx, userID); err != nil {
            log.Printf("[Handler] Failed to refresh presence: %v", err)
        }
    }

    // ← 立即响应 pong / Immediately respond pong
    s.sendMessage(conn, ServerMessage{Type: msgTypePong})
```

**为什么要同时更新本地和 Redis？/ Why update both local and Redis?**

```
本地 lastPing / Local lastPing:
  • 用于 heartbeatChecker() 检测超时 / Used for timeout detection by heartbeatChecker()
  • 快速（内存操作）/ Fast (memory operation)

Redis TTL:
  • 用于其他 Gateway 查询用户是否在线 / Used by other Gateways to check if user online
  • 防止 Gateway 宕机后 Presence 永久残留 / Prevent Presence residue after Gateway crash
```

#### 2. 服务端检测超时 / Server Detects Timeout (handler.go:212-229)

```go
func (s *Server) heartbeatChecker(ctx context.Context, conn *Connection) {
    ticker := time.NewTicker(heartbeatInterval)  // 30s
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:  // ← 每 30s 检查一次 / Check every 30s
            if time.Since(conn.GetLastPing()) > heartbeatTimeout {  // 90s
                log.Printf("[Handler] Connection timeout for user %s", conn.UserID)
                conn.Close()  // ← 强制关闭连接 / Force close connection
                return
            }

        case <-ctx.Done():  // ← 用户正常断开 / User normally disconnected
            return
        }
    }
}
```

### 时序图 / Timing Diagram

```
时间轴 / Timeline:
T0  ────────────────────────────────────────────────────────
    Client 连接，注册成功 / Client connects, registration success
    go heartbeatChecker() 启动 / starts

T30 ────────────────────────────────────────────────────────
    Client 发送 ping / sends ping → Server 更新 lastPing / updates lastPing

    heartbeatChecker: time.Since(lastPing) = 0s < 90s ✓

T60 ────────────────────────────────────────────────────────
    Client 发送 ping / sends ping → Server 更新 lastPing / updates lastPing

    heartbeatChecker: time.Since(lastPing) = 0s < 90s ✓

T90 ────────────────────────────────────────────────────────
    假设网络断开 / Assume network disconnected，Client 未发送 ping / didn't send ping

    heartbeatChecker: time.Since(lastPing) = 30s < 90s ✓

T120 ───────────────────────────────────────────────────────
    heartbeatChecker: time.Since(lastPing) = 60s < 90s ✓

T150 ───────────────────────────────────────────────────────
    heartbeatChecker: time.Since(lastPing) = 90s >= 90s ✗

    执行 conn.Close() / Execute conn.Close() → 触发断开流程 / Trigger disconnect flow
```

### 为什么检测间隔等于心跳间隔？/ Why is check interval equal to heartbeat interval?

```go
ticker := time.NewTicker(heartbeatInterval)  // 30s
```

**可选方案对比 / Options Comparison:**

| 方案 / Option | 检测间隔 / Interval | 优点 / Advantages | 缺点 / Disadvantages |
|------|---------|------|------|
| 方案 A（当前）/ A (current) | 30s | CPU 开销低 / Low CPU overhead | 检测延迟 ±30s / Detection delay ±30s |
| 方案 B / B | 10s | 更快发现超时 / Faster timeout detection | CPU 开销高 3 倍 / 3x CPU overhead |
| 方案 C / C | 60s | CPU 开销更低 / Even lower CPU | 超时发现太慢 / Too slow detection |

**当前实现是合理折中 / Current is reasonable compromise**：
- 连接超时后最多 30s 才被清理 / Connection cleaned up max 30s after timeout
- 对于 IM 系统，30s 延迟可接受 / For IM system, 30s delay acceptable

---

## 消息路由详解 / Message Routing

### 发送消息流程 / Send Message Flow (handler.go:116-134)

```go
case msgTypeMessage:
    // ← 验证：必须先注册 / Validate: must register first
    if userID == "" {
        s.sendError(conn, "Not registered")
        continue
    }

    // ← 验证：必须指定接收者 / Validate: must specify receiver
    if msg.To == "" {
        s.sendError(conn, "Recipient is required")
        continue
    }

    // ← 路由消息 / Route message
    if err := s.routeMessage(ctx, userID, msg.To, msg.Content); err != nil {
        log.Printf("[Handler] Failed to route message: %v", err)
        s.sendError(conn, "Failed to send message")
        continue
    }

    log.Printf("[Handler] Message routed: %s -> %s", userID, msg.To)
```

### routeMessage() - 查询路由并发送 / Query Route and Send (handler.go:154-170)

```go
func (s *Server) routeMessage(ctx context.Context, from, to, content string) error {
    // ← Step 1: 查询接收者在哪个 Gateway / Query which Gateway receiver is on
    presence, err := s.presenceMgr.Get(ctx, to)
    if err != nil {
        return err  // 用户离线或查询失败 / User offline or query failed
    }

    // ← Step 2: 构造路由消息 / Construct route message
    msg := &router.Message{
        From:    from,
        To:      to,
        Content: content,
        Type:    "direct",
    }

    // ← Step 3: 发送到目标 Gateway / Send to target Gateway
    return s.router.RouteToGateway(ctx, presence.GatewayID, msg)
}
```

### 完整消息流（跨网关）/ Complete Message Flow (Cross-Gateway)

```
Alice@Gateway-01 发送消息给 / sends message to Bob@Gateway-02

┌────────────────────────────────────────────────────────────┐
│ Step 1: Client 发送 WebSocket 消息 / sends WebSocket message │
│   {"type": "message", "to": "bob", "content": "Hello"}    │
└─────────────────┬──────────────────────────────────────────┘
                  ▼
┌────────────────────────────────────────────────────────────┐
│ Step 2: handleConnection() 收到消息 / receives message      │
│   switch msg.Type → case msgTypeMessage                    │
└─────────────────┬──────────────────────────────────────────┘
                  ▼
┌────────────────────────────────────────────────────────────┐
│ Step 3: routeMessage() 查询 Bob 的位置 / queries Bob's loc  │
│   presenceMgr.Get("bob") → {gwId: "gateway-02"}           │
└─────────────────┬──────────────────────────────────────────┘
                  ▼
┌────────────────────────────────────────────────────────────┐
│ Step 4: 发送到目标 Gateway / Send to target Gateway         │
│   router.RouteToGateway("gateway-02", msg)                 │
│   → Redis PUBLISH gateway:gateway-02 {...}                 │
└─────────────────┬──────────────────────────────────────────┘
                  ▼
        ════════════════════════════
             Redis Pub/Sub
        ════════════════════════════
                  ▼
┌────────────────────────────────────────────────────────────┐
│ Step 5: Gateway-02 的 router.processMessages() 收到消息 / receives │
│   调用 handler / calls: deliverMessage(msg)                │
└─────────────────┬──────────────────────────────────────────┘
                  ▼
┌────────────────────────────────────────────────────────────┐
│ Step 6: deliverMessage() 查找本地连接 / finds local conn    │
│   connMgr.GetByUserID("bob") → conn                        │
└─────────────────┬──────────────────────────────────────────┘
                  ▼
┌────────────────────────────────────────────────────────────┐
│ Step 7: 通过 WebSocket 发送给 Bob / Send to Bob via WebSocket│
│   conn.WriteMessage({"type":"message","from":"alice",...}) │
└────────────────────────────────────────────────────────────┘
```

---

## 本地消息投递：deliverMessage() / Local Message Delivery: deliverMessage()

**这是 Router 的回调函数 / This is Router's callback function**，处理**从 Redis 收到的消息** / handles **messages received from Redis**。

```go
func (s *Server) deliverMessage(msg *router.Message) {
    // ← 在本地连接管理器中查找接收者 / Find receiver in local connection manager
    conn, ok := s.connMgr.GetByUserID(msg.To)
    if !ok {
        log.Printf("[Handler] User %s not found locally", msg.To)
        return  // ← 正常情况：用户刚好断开连接 / Normal: user just disconnected
    }

    // ← 转换消息格式（router.Message → ServerMessage）
    // Convert message format (router.Message → ServerMessage)
    serverMsg := ServerMessage{
        Type:    msgTypeMessage,
        From:    msg.From,
        Content: msg.Content,
    }

    // ← 发送给客户端 / Send to client
    s.sendMessage(conn.Conn, serverMsg)
    log.Printf("[Handler] Message delivered to %s", msg.To)
}
```

### 为什么可能找不到用户？/ Why might user not be found?

**时序问题 / Timing Issue:**
```
T0: Bob 在线 / online，Alice 查询 Presence / queries Presence → gateway-02
T1: Alice 发送消息到 gateway-02 / sends message to gateway-02
T2: Bob 断开连接（Redis Pub/Sub 有延迟）/ disconnects (Redis Pub/Sub has delay)
T3: Gateway-02 收到消息 / receives message，但 Bob 已不在本地 / but Bob no longer local
    → 打印日志 / log it，丢弃消息 / discard message
```

**这是可接受的 / This is acceptable**:
- WebSocket 本身不保证消息可靠送达 / WebSocket doesn't guarantee reliable delivery
- 生产环境会有离线消息队列（存到 DB/Kafka）/ Production has offline queue (store in DB/Kafka)

---

## 连接清理流程 / Connection Cleanup

**当 `ReadMessage()` 返回错误时 / When `ReadMessage()` returns error**，退出消息循环 / exit message loop，执行清理 / execute cleanup：

```go
// Cleanup on disconnect / 断开连接时清理
if wsConn != nil {  // ← 只有注册过的用户才需要清理 / Only cleanup registered users
    // ← Step 1: 移除本地连接 / Remove local connection
    s.connMgr.Remove(wsConn)

    // ← Step 2: 删除 Redis Presence / Delete Redis Presence
    if err := s.presenceMgr.Remove(ctx, userID); err != nil {
        log.Printf("[Handler] Failed to remove presence: %v", err)
    }

    log.Printf("[Handler] User %s disconnected (connID: %s)", userID, connID)
}
```

### 为什么先移除本地，再删除 Redis？/ Why remove local first, then Redis?

**顺序 A（当前实现）/ Order A (current):**
```
connMgr.Remove() → presenceMgr.Remove()

优点 / Advantages:
  ✅ 立即停止接收新消息（其他 Gateway 还能路由过来）
  ✅ Immediately stop receiving new messages (other Gateways can still route here)
  ✅ Redis 删除失败影响小（TTL 会自动过期）
  ✅ Redis delete failure has small impact (TTL expires automatically)
```

**顺序 B（反过来）/ Order B (reversed):**
```
presenceMgr.Remove() → connMgr.Remove()

缺点 / Disadvantages:
  ❌ Redis 删除后，本地连接还在 → 短暂不一致
  ❌ After Redis delete, local connection still exists → brief inconsistency
  ❌ 其他 Gateway 认为用户离线，但本地还能收消息
  ❌ Other Gateways think user offline, but locally still receives messages
```

### 清理时序图 / Cleanup Timeline

```
用户断开连接的完整流程 / Complete disconnection flow:

┌─────────────────────────────────────────────────────────┐
│  1. 连接断开（网络/浏览器关闭）/ Connection drops (network/browser close) │
└───────────────────┬─────────────────────────────────────┘
                    ▼
┌─────────────────────────────────────────────────────────┐
│  2. ReadMessage() 返回错误 / returns error               │
│     websocket.IsUnexpectedCloseError() 记录日志 / logs  │
└───────────────────┬─────────────────────────────────────┘
                    ▼
┌─────────────────────────────────────────────────────────┐
│  3. break 退出消息循环 / exit message loop              │
└───────────────────┬─────────────────────────────────────┘
                    ▼
┌─────────────────────────────────────────────────────────┐
│  4. 执行清理代码 / Execute cleanup code                 │
│     if wsConn != nil {                                  │
│         connMgr.Remove(wsConn)                          │
│         presenceMgr.Remove(ctx, userID)                 │
│     }                                                   │
└───────────────────┬─────────────────────────────────────┘
                    ▼
┌─────────────────────────────────────────────────────────┐
│  5. defer 语句执行 / defer statements execute            │
│     cancel() → 通知 heartbeatChecker 退出 / notify exit │
│     conn.Close() → 确保连接关闭 / ensure close          │
└─────────────────────────────────────────────────────────┘
```

---

## 并发安全分析 / Concurrency Safety

### 1. handleConnection 的生命周期 / Lifecycle

```go
每个 WebSocket 连接 → 一个独立的 goroutine 运行 handleConnection
Each WebSocket connection → one independent goroutine runs handleConnection
```

**不会并发访问的变量 / Non-concurrent variables:**
- `userID`, `wsConn` - 只在当前 goroutine 读写 / Only read/write in current goroutine
- `ctx` - 只传递给子 goroutine，不修改 / Only passed to child goroutine, not modified

**需要并发保护的共享资源 / Shared resources requiring protection:**
- `s.connMgr` - 使用 `sync.Map`，线程安全 / Uses `sync.Map`, thread-safe
- `s.presenceMgr` - 底层是 Redis，原子操作 / Underlying Redis, atomic operations
- `s.router` - 发布到 Redis，无共享状态 / Publish to Redis, no shared state

### 2. heartbeatChecker 的并发 / Concurrency

```go
// handleConnection 中启动 / Started in handleConnection
go s.heartbeatChecker(ctx, wsConn)
```

**潜在竞态条件 / Potential race condition:**

```
Goroutine A (handleConnection):        Goroutine B (heartbeatChecker):
ReadMessage() 返回错误 / returns error
break 退出循环 / exit loop
执行 / execute connMgr.Remove(wsConn)
                                       ticker.C 触发 / triggers
                                       conn.GetLastPing() → 超时 / timeout
                                       conn.Close() ← 可能在这里 / possibly here
defer conn.Close()  ← 也在这里关闭 / also closes here
```

**解决方案 / Solution:**
`websocket.Conn.Close()` 是**幂等**的 / is **idempotent**，多次调用不会 panic / multiple calls don't panic。

### 3. Context 取消传播 / Context Cancellation Propagation

```go
ctx, cancel := context.WithCancel(...)
defer cancel()  // ← handleConnection 退出时取消 / Cancel when handleConnection exits

go s.heartbeatChecker(ctx, wsConn)
```

**取消流程 / Cancellation Flow:**

```
handleConnection 退出 / exits
    ↓
defer cancel() 执行 / executes
    ↓
ctx.Done() channel 被关闭 / channel closed
    ↓
heartbeatChecker 中的 select 收到信号 / receives signal in select
    ↓
case <-ctx.Done(): return
    ↓
heartbeatChecker goroutine 退出 / exits
```

这确保了**无 goroutine 泄漏** / This ensures **no goroutine leaks**。

---

## 错误处理策略 / Error Handling

### 错误分类与处理 / Error Classification and Handling

| 错误类型 / Error Type | 示例 / Example | 处理方式 / Handling | 原因 / Reason |
|---------|------|---------|------|
| **致命错误 / Fatal** | `ReadMessage()` 失败 / fails | `break` 退出循环 / exit loop | 连接已不可用 / Connection unusable |
| **可恢复错误 / Recoverable** | JSON 解析失败 / parse fails | `sendError()` + `continue` | 单条消息问题 / Single message issue |
| **业务错误 / Business** | 用户离线 / user offline | 返回 error 给调用者 / return error | 正常业务逻辑 / Normal business logic |
| **基础设施错误 / Infrastructure** | Redis 连接失败 / connection fails | 记录日志 + 继续运行 / log + continue | 临时故障，可能恢复 / Temporary, may recover |

---

## 性能优化考虑 / Performance Considerations

### 为什么不用 goroutine 处理每条消息？/ Why not goroutine per message?

```go
// 当前实现（同步）/ Current (synchronous)
for {
    _, message, err := conn.ReadMessage()
    // 直接处理消息 / Process directly
    switch msg.Type { ... }
}
```

**同步的好处 / Synchronous advantages:**
- ✅ 保证消息**顺序处理** / Guarantees message **order**
- ✅ 避免 goroutine 爆炸（1 万连接 = 1 万 goroutine，而非 10 万+）
- ✅ Avoids goroutine explosion (10K connections = 10K goroutines, not 100K+)
- ✅ 背压控制（客户端发太快会被 TCP 流控限制）
- ✅ Backpressure control (too fast sending throttled by TCP flow control)

---

## 总结 / Summary

### handler.go 的设计精髓 / Design Essence

1. **状态机设计 / State Machine Design**
   ```
   未连接 → 已连接 → 已注册 → 断开
   Not connected → Connected → Registered → Disconnected
             ↓         ↓
          拒绝消息 / Reject   处理消息 / Process
   ```

2. **职责分离 / Separation of Concerns**
   - `handleConnection`: 连接生命周期管理 / Connection lifecycle
   - `routeMessage`: 消息路由逻辑 / Message routing logic
   - `deliverMessage`: 本地投递 / Local delivery
   - `heartbeatChecker`: 健康检测 / Health detection

3. **优雅降级 / Graceful Degradation**
   - Redis 失败 → 允许重试，不断开连接 / Allow retry, don't disconnect
   - 消息投递失败 → 记录日志，不影响其他用户 / Log, don't affect others

4. **资源清理 / Resource Cleanup**
   - `defer conn.Close()` - 确保连接关闭 / Ensure connection closes
   - `defer cancel()` - 确保 goroutine 退出 / Ensure goroutine exits
   - 清理顺序：本地 → 远程（先快后慢）/ Cleanup order: local → remote (fast first)

---

**这个 handler.go 是一个教科书级别的 WebSocket 处理器实现 / This handler.go is a textbook-level WebSocket handler implementation**，涵盖了生产环境的所有核心要素 / covers all core elements for production! 🎯
