# Server 实现详解

> HTTP 服务器与组件协调 - 系统的"指挥官"

## 目录

- [整体架构定位](#整体架构定位)
- [数据结构](#数据结构)
- [核心方法详解](#核心方法详解)
  - [NewServer - 依赖注入](#newserver---依赖注入)
  - [Start - 启动流程](#start---启动流程)
  - [Stop - 优雅关闭](#stop---优雅关闭)
  - [handleWebSocket - 连接升级](#handlewebsocket---连接升级)
  - [healthCheckLoop - 健康检查](#healthcheckloop---健康检查)
- [组件协调机制](#组件协调机制)
- [HTTP 路由设计](#http-路由设计)
- [WebSocket 升级流程](#websocket-升级流程)
- [优雅关闭详解](#优雅关闭详解)
- [设计决策](#设计决策)

---

## 整体架构定位

Server 是整个 Gateway 的**入口点**和**协调者**，负责：

1. **组件初始化**：创建并组装所有子组件
2. **HTTP 服务**：提供 WebSocket、健康检查、统计等端点
3. **生命周期管理**：启动、运行、优雅关闭
4. **组件协调**：连接 Handler、Router、Presence、Connection 等模块

### 在架构中的位置

```
┌───────────────────────────────────────────────────────────┐
│                     Server (server.go)                     │
│                      🎯 协调中心                           │
│                                                            │
│  ┌──────────────────────────────────────────────────────┐ │
│  │  HTTP Server (gorilla/websocket)                     │ │
│  │  • GET /ws        → handleWebSocket (WebSocket 升级)  │ │
│  │  • GET /health    → handleHealth    (健康检查)        │ │
│  │  • GET /stats     → handleStats     (统计信息)        │ │
│  └──────────┬────────────────────────────────────────────┘ │
│             │                                              │
│  ┌──────────▼────────────────────────────────────────────┐ │
│  │  组件管理                                             │ │
│  │  ┌────────────┐  ┌──────────────┐  ┌───────────────┐ │ │
│  │  │ Handler    │  │ Router       │  │ Presence      │ │ │
│  │  │ (业务逻辑)  │  │ (消息路由)   │  │ (状态管理)     │ │ │
│  │  └────────────┘  └──────────────┘  └───────────────┘ │ │
│  │  ┌────────────┐  ┌──────────────┐                    │ │
│  │  │ Connection │  │ healthCheck  │                    │ │
│  │  │ (连接管理)  │  │ (定时任务)   │                    │ │
│  │  └────────────┘  └──────────────┘                    │ │
│  └────────────────────────────────────────────────────────┘ │
└───────────────────────────────────────────────────────────┘
                            │
                            ▼
                 ┌──────────────────────┐
                 │   Redis Cluster      │
                 │   (外部依赖)          │
                 └──────────────────────┘
```

### 职责划分

| 组件 | Server 的责任 | 组件自身的责任 |
|------|--------------|---------------|
| **Handler** | 调用 handleConnection | 处理 WebSocket 消息 |
| **Router** | 启动时传入 deliverMessage 回调 | 跨网关消息路由 |
| **Presence** | 创建实例 | Redis 状态管理 |
| **Connection** | 创建实例 | 本地连接管理 |
| **HTTP Server** | 配置路由、启动/关闭 | 处理 HTTP 请求 |

---

## 数据结构

### upgrader 全局变量 (server.go:18-22)

```go
var upgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool {
        return true // Allow all origins for demo
    },
}
```

#### CheckOrigin 的作用

**WebSocket 跨域保护**：

浏览器在发起 WebSocket 连接时会发送 `Origin` 头：
```http
GET /ws HTTP/1.1
Host: gateway.example.com
Upgrade: websocket
Connection: Upgrade
Origin: https://malicious.com  ← 浏览器自动添加
```

**CheckOrigin 函数**：
```go
func(r *http.Request) bool {
    origin := r.Header.Get("Origin")
    // return true  → 允许任何来源（不安全！）
    // return origin == "https://trusted.com"  → 只允许特定域名
}
```

**当前实现的风险**：
```go
CheckOrigin: func(r *http.Request) bool {
    return true  // ⚠️ DEMO 环境可以，生产环境危险！
}
```

**生产环境的正确做法**：
```go
var upgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool {
        origin := r.Header.Get("Origin")
        // 白名单机制
        allowedOrigins := map[string]bool{
            "https://app.example.com":     true,
            "https://mobile.example.com":  true,
        }
        return allowedOrigins[origin]
    },
}
```

**为什么需要检查 Origin？**

攻击场景：
```
1. 用户登录了 app.example.com（合法网站）
2. 用户同时访问 malicious.com（恶意网站）
3. 恶意网站的 JS 代码：
   const ws = new WebSocket("wss://gateway.example.com/ws")
   // 浏览器会自动带上 app.example.com 的 Cookie！
4. 如果不检查 Origin，恶意网站可以：
   - 冒充用户发送消息
   - 窃听用户的实时消息
```

---

### Server 结构体 (server.go:24-32)

```go
type Server struct {
    gatewayID   string
    port        int
    connMgr     *ConnectionManager
    presenceMgr *presence.Manager
    router      *router.Router
    httpServer  *http.Server
}
```

#### 字段详解

| 字段 | 类型 | 用途 | 示例 |
|------|------|------|------|
| `gatewayID` | string | Gateway 唯一标识 | `"gateway-01"` |
| `port` | int | HTTP 监听端口 | `8080` |
| `connMgr` | *ConnectionManager | 本地连接管理器 | - |
| `presenceMgr` | *presence.Manager | Redis 状态管理器 | - |
| `router` | *router.Router | 消息路由器 | - |
| `httpServer` | *http.Server | HTTP 服务器实例 | - |

#### 为什么字段都是私有的？

**封装原则**：

```go
// ✅ 当前设计：私有字段
type Server struct {
    gatewayID string  // 外部无法直接修改
}

// 通过方法控制访问
func (s *Server) GetGatewayID() string {
    return s.gatewayID
}

// ❌ 如果是公开字段
type Server struct {
    GatewayID string  // 任何代码都可以修改！
}

// 风险：
server.GatewayID = ""  // 可能导致系统混乱
```

**好处**：
- 防止外部误修改
- 保持内部状态一致性
- 未来可添加访问控制逻辑

---

## 核心方法详解

### NewServer - 依赖注入 (server.go:34-46)

```go
func NewServer(gatewayID string, port int, redisClient *redis.Client) *Server {
    presenceMgr := presence.NewManager(redisClient)
    msgRouter := router.NewRouter(redisClient, gatewayID)

    return &Server{
        gatewayID:   gatewayID,
        port:        port,
        connMgr:     NewConnectionManager(),
        presenceMgr: presenceMgr,
        router:      msgRouter,
    }
}
```

#### 调用位置：main.go:26

```go
func main() {
    redisClient := redis.NewClient(&redis.Options{
        Addr: "localhost:6379",
    })

    server := gateway.NewServer(gatewayID, port, redisClient)
    server.Start(ctx)
}
```

#### 依赖注入模式

**为什么不在 NewServer 内部创建 Redis 客户端？**

| 方案 | 优点 | 缺点 |
|------|------|------|
| **内部创建** | 使用简单 | 难以测试、配置不灵活 |
| **外部注入 ✅** | 可测试、共享连接池 | 调用方需管理 |

**依赖图**：
```
main.go:
  ┌─ 创建 redisClient
  │
  └─▶ NewServer(redisClient)
        ├─▶ presence.NewManager(redisClient)  ← 共享同一个连接池
        └─▶ router.NewRouter(redisClient)     ← 共享同一个连接池
```

**共享连接池的好处**：
```
如果每个组件各自创建 Redis 客户端：
- Presence: 100 个连接
- Router: 100 个连接
总计：200 个连接 ❌

共享连接池：
- 所有组件共用 1 个连接池
- 连接池大小：10 个连接（可配置）
总计：10 个连接 ✅
```

#### 组件初始化顺序

**顺序无关性**：
```go
presenceMgr := presence.NewManager(redisClient)  // 步骤 1
msgRouter := router.NewRouter(redisClient, gatewayID)  // 步骤 2

// 可以交换顺序，因为两者无依赖关系
msgRouter := router.NewRouter(redisClient, gatewayID)  // 步骤 1
presenceMgr := presence.NewManager(redisClient)  // 步骤 2
```

**为什么无依赖？**
- 只是创建对象，未启动服务
- 真正的依赖关系在 `Start()` 方法中建立

---

### Start - 启动流程 (server.go:48-76)

```go
func (s *Server) Start(ctx context.Context) error {
    // 1. 启动消息路由器
    if err := s.router.Start(ctx, s.deliverMessage); err != nil {
        return fmt.Errorf("failed to start router: %w", err)
    }

    // 2. 启动健康检查协程
    go s.healthCheckLoop(ctx)

    // 3. 配置 HTTP 路由
    mux := http.NewServeMux()
    mux.HandleFunc("/ws", s.handleWebSocket)
    mux.HandleFunc("/health", s.handleHealth)
    mux.HandleFunc("/stats", s.handleStats)

    // 4. 创建 HTTP 服务器
    s.httpServer = &http.Server{
        Addr:    fmt.Sprintf(":%d", s.port),
        Handler: mux,
    }

    log.Printf("[Server] Gateway %s starting on port %d", s.gatewayID, s.port)

    // 5. 启动 HTTP 服务器（阻塞）
    if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
        return fmt.Errorf("failed to start HTTP server: %w", err)
    }

    return nil
}
```

#### 启动流程详解

**步骤 1：启动 Router** (server.go:51)

```go
s.router.Start(ctx, s.deliverMessage)
```

**关键点**：传入 `s.deliverMessage` 回调函数

**回调机制**：
```
Router 收到跨网关消息后：
1. Redis Pub/Sub 触发回调
2. 调用 handler(targetUserID, message)
3. handler 实际上是 Server.deliverMessage
4. deliverMessage 查询 ConnectionManager
5. 找到 WebSocket 连接并发送
```

**依赖倒置原则**：
```
传统方式（Router 依赖 Server）：
Router:
  func processMessage(msg) {
      server.DeliverMessage(msg)  // ❌ Router 依赖 Server
  }

当前方式（依赖注入）：
Router:
  func Start(handler func(userID, msg)) {
      this.handler = handler  // ✅ Router 不知道 Server 的存在
  }

Server:
  router.Start(s.deliverMessage)  // Server 注入自己的实现
```

**步骤 2：启动健康检查** (server.go:56)

```go
go s.healthCheckLoop(ctx)
```

**为什么用 goroutine？**
```
healthCheckLoop 是一个无限循环：
for {
    select {
    case <-ticker.C:
        // 定期清理超时连接
    }
}

如果不用 go：
s.healthCheckLoop(ctx)  // ❌ 阻塞在这里，无法继续启动 HTTP 服务器
```

**步骤 3-4：配置 HTTP 服务器** (server.go:59-67)

```go
mux := http.NewServeMux()
mux.HandleFunc("/ws", s.handleWebSocket)
mux.HandleFunc("/health", s.handleHealth)
mux.HandleFunc("/stats", s.handleStats)

s.httpServer = &http.Server{
    Addr:    fmt.Sprintf(":%d", s.port),
    Handler: mux,
}
```

**为什么使用 ServeMux？**

| 方案 | 路由能力 | 适用场景 |
|------|---------|---------|
| `http.HandleFunc()` 全局 | ✅ 基础 | 简单应用 |
| `http.NewServeMux()` ✅ | ✅ 基础 + 隔离 | 需要多实例 |
| `gin/echo` | ✅ 高级（路径参数、中间件） | 复杂应用 |

**当前选择 ServeMux 的原因**：
```go
// 场景：同一进程启动多个 Gateway（测试环境）
gateway1 := NewServer("gw-01", 8080, redis)
gateway2 := NewServer("gw-02", 8081, redis)

// 如果使用全局 http.HandleFunc：
http.HandleFunc("/ws", gateway1.handleWebSocket)
http.HandleFunc("/ws", gateway2.handleWebSocket)  // ❌ 覆盖了 gateway1

// 使用独立 ServeMux：
mux1 := http.NewServeMux()
mux1.HandleFunc("/ws", gateway1.handleWebSocket)

mux2 := http.NewServeMux()
mux2.HandleFunc("/ws", gateway2.handleWebSocket)
// ✅ 两者隔离
```

**步骤 5：启动 HTTP 服务器** (server.go:71-73)

```go
if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
    return fmt.Errorf("failed to start HTTP server: %w", err)
}
```

**为什么检查 `err != http.ErrServerClosed`？**

```
正常关闭流程：
1. 调用 server.Stop(ctx)
2. Stop() 调用 httpServer.Shutdown(ctx)
3. Shutdown() 会让 ListenAndServe() 返回 http.ErrServerClosed
4. 这是预期行为，不是错误 ✅

异常情况：
1. 端口被占用 → 返回 "address already in use"
2. 权限不足 → 返回 "permission denied"
这些才是真正的错误 ❌
```

**代码逻辑**：
```go
if err != nil && err != http.ErrServerClosed {
    // 只有在非预期错误时才返回错误
    return fmt.Errorf("failed to start HTTP server: %w", err)
}
return nil  // 正常关闭返回 nil
```

---

### Stop - 优雅关闭 (server.go:78-98)

```go
func (s *Server) Stop(ctx context.Context) error {
    log.Printf("[Server] Shutting down gateway %s", s.gatewayID)

    // 1. 停止 Router（停止接收新消息）
    if err := s.router.Stop(); err != nil {
        log.Printf("[Server] Error stopping router: %v", err)
    }

    // 2. 关闭所有 WebSocket 连接
    s.connMgr.ForEach(func(conn *Connection) {
        conn.Close()
    })

    // 3. 关闭 HTTP 服务器
    if s.httpServer != nil {
        return s.httpServer.Shutdown(ctx)
    }

    return nil
}
```

#### 优雅关闭的步骤顺序

**为什么是这个顺序？**

```
错误顺序（先关 HTTP）：
1. 关闭 HTTP 服务器 → 无法接收新连接 ✅
2. 停止 Router → 但可能有消息正在投递 ❌
3. 关闭 WebSocket → 消息丢失！❌

正确顺序（当前实现）：
1. 停止 Router → 不再接收跨网关消息 ✅
2. 关闭 WebSocket → 通知客户端断开 ✅
3. 关闭 HTTP → 拒绝新连接 ✅
```

**时间轴示例**：
```
T0: 收到 SIGTERM 信号
    调用 server.Stop(ctx)

T0+10ms: router.Stop()
    - Redis Pub/Sub 取消订阅
    - processMessages goroutine 退出
    - 不再接收新消息 ✅

T0+20ms: connMgr.ForEach(conn.Close)
    - 发送 WebSocket Close Frame
    - 客户端收到关闭通知
    - 客户端主动重连到其他 Gateway ✅

T0+50ms: httpServer.Shutdown(ctx)
    - 停止接收新的 HTTP 请求
    - 等待现有请求完成（如果有）
    - 释放端口 ✅

T0+100ms: 进程退出
```

#### httpServer.Shutdown 详解

**Shutdown vs Close**

| 方法 | 行为 | 数据丢失风险 |
|------|------|------------|
| `Close()` | 立即关闭 | ❌ 高（现有连接强制断开） |
| `Shutdown(ctx)` ✅ | 优雅关闭 | ✅ 低（等待现有连接完成） |

**Shutdown 的工作流程**：
```go
httpServer.Shutdown(ctx)

内部逻辑：
1. 停止接收新连接（关闭 listener）
2. 等待现有连接：
   - WebSocket 连接发送 Close Frame
   - HTTP 请求完成响应
3. 如果超时（ctx 超时），强制关闭剩余连接
4. 返回
```

**超时控制**：
```go
// main.go 中的使用
shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()

server.Stop(shutdownCtx)
// 最多等待 10 秒，超时后强制关闭
```

#### 当前实现的不足

**问题 1：未清理 Presence**

```go
func (s *Server) Stop(ctx context.Context) error {
    s.router.Stop()
    s.connMgr.ForEach(func(conn *Connection) {
        conn.Close()
        // ❌ 未清理 Redis 中的 presence 数据！
    })
    // ...
}
```

**影响**：
- Redis 中的 presence 数据会保留 90 秒（TTL）
- 这 90 秒内，其他 Gateway 会尝试路由到已关闭的 Gateway
- 消息投递失败

**修复建议**：
```go
func (s *Server) Stop(ctx context.Context) error {
    s.router.Stop()

    // ✅ 清理 Presence
    s.connMgr.ForEach(func(conn *Connection) {
        if conn.UserID != "" {
            s.presenceMgr.Remove(ctx, conn.UserID)
        }
        conn.Close()
    })

    if s.httpServer != nil {
        return s.httpServer.Shutdown(ctx)
    }
    return nil
}
```

**问题 2：未等待 healthCheckLoop 退出**

```go
// Start 中启动：
go s.healthCheckLoop(ctx)

// Stop 中没有等待它结束！
```

**风险**：
- healthCheckLoop 可能在关闭后仍在运行
- 可能尝试访问已关闭的连接

**修复建议**：
```go
type Server struct {
    // ...
    wg sync.WaitGroup  // 跟踪后台 goroutine
}

func (s *Server) Start(ctx context.Context) error {
    s.wg.Add(1)
    go func() {
        defer s.wg.Done()
        s.healthCheckLoop(ctx)
    }()
    // ...
}

func (s *Server) Stop(ctx context.Context) error {
    // ...
    s.wg.Wait()  // 等待所有 goroutine 结束
    return nil
}
```

---

### handleWebSocket - 连接升级 (server.go:100-112)

```go
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
    // 1. 升级 HTTP 连接为 WebSocket
    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        log.Printf("[Server] Failed to upgrade connection: %v", err)
        return
    }

    // 2. 生成连接 ID
    connID := uuid.New().String()
    log.Printf("[Server] New WebSocket connection: %s", connID)

    // 3. 处理连接（阻塞直到连接关闭）
    s.handleConnection(conn, connID)
}
```

#### WebSocket 升级流程

**HTTP 握手过程**：

```
客户端请求：
GET /ws HTTP/1.1
Host: localhost:8080
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==
Sec-WebSocket-Version: 13

服务器响应（成功）：
HTTP/1.1 101 Switching Protocols
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=

服务器响应（失败）：
HTTP/1.1 403 Forbidden
（如果 CheckOrigin 返回 false）
```

**upgrader.Upgrade 的作用**：

1. **验证握手**：检查 HTTP 头是否符合 WebSocket 规范
2. **计算 Accept 密钥**：`Sec-WebSocket-Accept = SHA1(Key + Magic String)`
3. **劫持连接**：从 HTTP 层接管 TCP 连接
4. **返回 WebSocket 对象**：封装了 `ReadMessage()` 和 `WriteMessage()`

**为什么返回后立即调用 handleConnection？**

```go
s.handleConnection(conn, connID)  // 阻塞直到连接关闭

// 如果不阻塞：
s.handleConnection(conn, connID)
return  // ❌ 函数返回，conn 被垃圾回收，连接断开！
```

**handleConnection 内部有无限循环**：
```go
// handler.go:66
func (s *Server) handleConnection(conn *websocket.Conn, connID string) {
    defer conn.Close()

    for {
        _, message, err := conn.ReadMessage()  // 阻塞等待消息
        if err != nil {
            break  // 连接关闭时退出循环
        }
        // 处理消息...
    }
}
```

#### 每个连接一个 goroutine

**并发模型**：

```
10,000 个客户端连接 = 10,000 个 goroutine

每个 goroutine：
- 阻塞在 conn.ReadMessage()
- 收到消息时唤醒处理
- 处理完继续阻塞

内存开销：
- 每个 goroutine：~2KB 栈空间
- 10,000 个：~20MB
- 可接受 ✅
```

**对比其他模型**：

| 模型 | Go | Node.js | Java (传统线程) |
|------|-----|---------|----------------|
| 并发单元 | goroutine | 事件循环 | 线程 |
| 内存/单元 | ~2KB | - | ~1MB |
| 10K 连接开销 | ~20MB | ~10MB | ~10GB ❌ |

**Go 的优势**：goroutine 是轻量级的，非常适合 WebSocket 场景。

---

### handleHealth - 健康检查 (server.go:114-118)

```go
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK)
    fmt.Fprintf(w, "OK")
}
```

#### 使用场景

**Kubernetes Liveness Probe**：
```yaml
livenessProbe:
  httpGet:
    path: /health
    port: 8080
  initialDelaySeconds: 10
  periodSeconds: 5
```

**负载均衡器健康检查**：
```
AWS ALB → GET /health every 30s
如果返回 200：保留在负载池
如果返回 非200：移出负载池
```

#### 改进建议

**当前实现过于简单**，建议增强：

```go
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
    // 检查 Redis 连接
    ctx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
    defer cancel()

    if err := s.presenceMgr.redis.Ping(ctx).Err(); err != nil {
        w.WriteHeader(http.StatusServiceUnavailable)
        fmt.Fprintf(w, "Redis unhealthy: %v", err)
        return
    }

    // 检查连接数是否超载
    connCount := s.connMgr.Count()
    if connCount > 50000 {  // 假设最大容量
        w.WriteHeader(http.StatusServiceUnavailable)
        fmt.Fprintf(w, "Overloaded: %d connections", connCount)
        return
    }

    w.WriteHeader(http.StatusOK)
    fmt.Fprintf(w, "OK")
}
```

---

### handleStats - 统计信息 (server.go:120-124)

```go
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    fmt.Fprintf(w, `{"gatewayId":"%s","connections":%d}`, s.gatewayID, s.connMgr.Count())
}
```

#### 改进建议

**当前实现缺少关键指标**，建议扩展：

```go
type Stats struct {
    GatewayID       string    `json:"gatewayId"`
    Connections     int       `json:"connections"`
    Uptime          float64   `json:"uptimeSeconds"`
    MessagesIn      int64     `json:"messagesIn"`
    MessagesOut     int64     `json:"messagesOut"`
    LastHealthCheck time.Time `json:"lastHealthCheck"`
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
    stats := Stats{
        GatewayID:       s.gatewayID,
        Connections:     s.connMgr.Count(),
        Uptime:          time.Since(s.startTime).Seconds(),
        MessagesIn:      atomic.LoadInt64(&s.metrics.messagesIn),
        MessagesOut:     atomic.LoadInt64(&s.metrics.messagesOut),
        LastHealthCheck: s.lastHealthCheck,
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(stats)
}
```

**使用场景**：
```bash
# 监控脚本
curl http://localhost:8080/stats
{
  "gatewayId": "gateway-01",
  "connections": 1523,
  "uptimeSeconds": 3600.5,
  "messagesIn": 45230,
  "messagesOut": 45189
}
```

---

### healthCheckLoop - 健康检查 (server.go:126-143)

```go
func (s *Server) healthCheckLoop(ctx context.Context) {
    ticker := time.NewTicker(60 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            removed := s.connMgr.CheckHealth(heartbeatTimeout)
            if removed > 0 {
                log.Printf("[Server] Health check: removed %d stale connections", removed)
            }

        case <-ctx.Done():
            return
        }
    }
}
```

#### 为什么需要定期检查？

**问题场景**：
```
T0: 客户端连接，开始心跳
T30: 发送心跳 ✅
T60: 发送心跳 ✅
T90: 网络中断，心跳发送失败 ❌
T120: 客户端崩溃，TCP 连接未正常关闭

问题：
- 服务器端 conn.ReadMessage() 会一直阻塞
- 不会收到任何错误
- 连接永远不会被清理（僵尸连接）
```

**解决方案：定期检查**
```
healthCheckLoop 每 60 秒检查一次：
- 遍历所有连接
- 检查 LastPing 是否超过 90 秒
- 超时则关闭连接
```

#### 参数配置

```go
ticker := time.NewTicker(60 * time.Second)  // 检查间隔
removed := s.connMgr.CheckHealth(heartbeatTimeout)  // 超时阈值（90s）
```

**为什么检查间隔 = 60s，超时阈值 = 90s？**

| 参数 | 值 | 原因 |
|------|-----|------|
| 心跳间隔 | 30s | 客户端发送频率 |
| 超时阈值 | 90s (3x) | 允许 2 次心跳丢失 |
| 检查间隔 | 60s (2x) | 超时后最多 60s 内清理 |

**时间轴示例**：
```
T0:   最后一次心跳
T30:  心跳丢失
T60:  healthCheck #1 → 60-0=60s < 90s → 保留
T90:  心跳丢失
T120: healthCheck #2 → 120-0=120s > 90s → 清理 ✅
```

**为什么不用更短的检查间隔（如 10s）？**
- 检查需要遍历所有连接（O(N)）
- 10,000 连接可能耗时 10-50ms
- 每 10s 执行一次会增加 CPU 负载
- 60s 是性能与及时性的平衡

#### Context 取消机制

```go
select {
case <-ticker.C:
    // 定期执行

case <-ctx.Done():
    return  // 收到关闭信号，退出循环
}
```

**调用链**：
```
main.go:
  ctx, cancel := context.WithCancel(context.Background())
  go server.Start(ctx)

  <-sigint  // 收到 SIGINT 信号
  cancel()  // 取消 context

server.Start(ctx):
  go s.healthCheckLoop(ctx)  // 传递 context

healthCheckLoop(ctx):
  <-ctx.Done()  // 检测到取消，退出
```

---

## 组件协调机制

### 组件启动顺序

```
NewServer():
  ├─ NewConnectionManager()   (顺序 1：无依赖)
  ├─ presence.NewManager()    (顺序 2：无依赖)
  └─ router.NewRouter()       (顺序 3：无依赖)

Start():
  ├─ router.Start()           (顺序 1：必须先启动，接收消息)
  ├─ healthCheckLoop()        (顺序 2：后台任务)
  └─ httpServer.ListenAndServe() (顺序 3：阻塞主线程)
```

### 组件通信方式

#### 1. 回调函数（Router → Server）

```go
// Server 启动时注入回调
s.router.Start(ctx, s.deliverMessage)

// Router 收到消息后调用回调
func (r *Router) processMessages(ctx context.Context, handler MessageHandler) {
    for msg := range msgChan {
        handler(msg.To, &msg)  // ← 调用 Server.deliverMessage
    }
}
```

**优点**：
- 解耦：Router 不需要知道 Server 的存在
- 灵活：可以注入不同的处理函数（测试时很有用）

#### 2. 直接调用（Server → 其他组件）

```go
// Server 直接调用其他组件的方法
s.connMgr.GetByUserID(userID)
s.presenceMgr.Register(ctx, userID, gwID, connID)
```

**为什么不用回调？**
- 调用方向单一：Server 是协调者，只有它调用其他组件
- 无需解耦：Server 天然依赖所有组件

---

## HTTP 路由设计

### 路由表

| 路径 | 方法 | 用途 | 响应 |
|------|------|------|------|
| `/ws` | GET | WebSocket 升级 | 101 Switching Protocols |
| `/health` | GET | 健康检查 | 200 OK / 503 Unavailable |
| `/stats` | GET | 统计信息 | JSON 数据 |

### 为什么只用 GET 方法？

**WebSocket 限制**：
- WebSocket 握手必须是 GET 请求
- HTTP 规范要求

**健康检查惯例**：
- GET /health 是行业标准
- Kubernetes、AWS、GCP 等都默认 GET

**统计接口**：
- 只读操作用 GET（RESTful 风格）
- 如果有修改操作应该用 POST/PUT

### 缺失的路由（可扩展）

```go
// 管理接口（需要认证）
mux.HandleFunc("/admin/connections", s.handleListConnections)      // GET: 列出所有连接
mux.HandleFunc("/admin/connections/:id", s.handleCloseConnection)  // DELETE: 强制断开连接
mux.HandleFunc("/admin/broadcast", s.handleBroadcast)              // POST: 广播消息

// 指标接口（Prometheus 格式）
mux.HandleFunc("/metrics", s.handleMetrics)                        // GET: Prometheus 指标
```

---

## WebSocket 升级流程

### 完整流程图

```
┌──────────────────────────────────────────────────────────────┐
│  客户端                                                      │
│  ws = new WebSocket("ws://localhost:8080/ws")                │
└────────────────────────┬─────────────────────────────────────┘
                         │
                         ▼
┌──────────────────────────────────────────────────────────────┐
│  HTTP 握手请求                                               │
│  GET /ws HTTP/1.1                                            │
│  Upgrade: websocket                                          │
│  Connection: Upgrade                                         │
│  Sec-WebSocket-Key: xxx                                      │
└────────────────────────┬─────────────────────────────────────┘
                         │
                         ▼
┌──────────────────────────────────────────────────────────────┐
│  Server.handleWebSocket                                      │
│  ├─ upgrader.Upgrade(w, r, nil)                              │
│  │  ├─ 验证 HTTP 头                                          │
│  │  ├─ 调用 CheckOrigin(r)                                   │
│  │  ├─ 计算 Sec-WebSocket-Accept                             │
│  │  └─ 劫持 TCP 连接                                         │
│  ├─ 生成 connID = UUID                                       │
│  └─ handleConnection(conn, connID)  ← 阻塞                   │
└────────────────────────┬─────────────────────────────────────┘
                         │
                         ▼
┌──────────────────────────────────────────────────────────────┐
│  HTTP 握手响应                                               │
│  HTTP/1.1 101 Switching Protocols                            │
│  Upgrade: websocket                                          │
│  Connection: Upgrade                                         │
│  Sec-WebSocket-Accept: yyy                                   │
└────────────────────────┬─────────────────────────────────────┘
                         │
                         ▼
┌──────────────────────────────────────────────────────────────┐
│  WebSocket 连接建立                                          │
│  • 客户端开始发送消息                                        │
│  • 服务器在 handleConnection 中处理                          │
└──────────────────────────────────────────────────────────────┘
```

---

## 优雅关闭详解

### 信号处理流程

```go
// main.go
func main() {
    ctx, cancel := context.WithCancel(context.Background())

    // 监听信号
    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

    go func() {
        <-sigChan  // 阻塞直到收到信号
        log.Println("Received shutdown signal")
        cancel()   // 取消 context
    }()

    // 启动服务器
    if err := server.Start(ctx); err != nil {
        log.Fatal(err)
    }

    // 优雅关闭
    shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer shutdownCancel()
    server.Stop(shutdownCtx)
}
```

### 关闭时序图

```
T0: 收到 SIGTERM
    ├─ cancel()                    (取消主 context)
    │
T1: Start() 中的 goroutine 收到 ctx.Done()
    ├─ router.Start 中的循环退出
    ├─ healthCheckLoop 退出
    │
T2: httpServer.ListenAndServe() 返回 http.ErrServerClosed
    │
T3: main() 调用 server.Stop(shutdownCtx)
    ├─ router.Stop()               (取消 Redis 订阅)
    ├─ connMgr.ForEach(conn.Close) (关闭所有 WebSocket)
    └─ httpServer.Shutdown()       (等待现有请求完成)
    │
T4: Shutdown 完成或超时 (10s)
    │
T5: 进程退出
```

### 零停机部署（未实现）

**当前问题**：
- 收到 SIGTERM → 立即关闭连接 → 客户端断开

**理想流程**：
1. 收到 SIGTERM
2. 停止接收新连接
3. 通知客户端即将关闭（发送特殊消息）
4. 客户端主动重连到其他 Gateway
5. 等待所有客户端迁移（最多 30s）
6. 关闭剩余连接
7. 进程退出

**实现建议**：
```go
func (s *Server) GracefulStop(ctx context.Context) error {
    // 1. 停止接收新连接
    s.httpServer.Shutdown(ctx)

    // 2. 通知所有客户端即将关闭
    s.connMgr.ForEach(func(conn *Connection) {
        notification := Message{
            Type:    "shutdown",
            Content: "Server is shutting down, please reconnect",
        }
        data, _ := json.Marshal(notification)
        conn.Send(websocket.TextMessage, data)
    })

    // 3. 等待客户端主动断开（最多 30s）
    waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
    defer cancel()

    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            if s.connMgr.Count() == 0 {
                return nil  // 所有客户端已断开
            }
        case <-waitCtx.Done():
            // 超时，强制关闭剩余连接
            s.connMgr.ForEach(func(conn *Connection) {
                conn.Close()
            })
            return nil
        }
    }
}
```

---

## 设计决策

### 1. 为什么不使用 gin/echo 框架？

| 框架 | 优点 | 缺点 |
|------|------|------|
| **net/http ✅** | 标准库、零依赖 | 路由功能弱 |
| **gin/echo** | 丰富的中间件、路由 | 依赖重、过度设计 |

**当前需求**：
- 只有 3 个路由
- 不需要复杂的中间件
- WebSocket 是核心，HTTP 只是辅助

**结论**：标准库足够。

### 2. 为什么不分离 HTTP 和 WebSocket 端口？

**可选方案**：
```go
// 方案 A：同一端口（当前实现）
:8080/ws      ← WebSocket
:8080/health  ← HTTP

// 方案 B：分离端口
:8080/ws      ← WebSocket
:8081/health  ← HTTP（管理端口）
```

**选择 A 的原因**：
- 简化部署（只需开放一个端口）
- 简化负载均衡配置
- HTTP 请求量很小，不会干扰 WebSocket

**何时应该分离？**
- 管理接口需要内网隔离
- HTTP 流量很大（如文件上传）

### 3. 为什么没有实现认证？

**当前实现**：
```go
CheckOrigin: func(r *http.Request) bool {
    return true  // ⚠️ 允许所有来源
}
```

**生产环境应该添加**：

#### 方式 1：Token 认证
```go
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
    token := r.URL.Query().Get("token")
    if !s.authService.ValidateToken(token) {
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return
    }
    // ...
}

// 客户端
ws = new WebSocket("ws://localhost:8080/ws?token=xxx")
```

#### 方式 2：Cookie 认证
```go
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
    cookie, err := r.Cookie("session")
    if err != nil || !s.sessionStore.Validate(cookie.Value) {
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return
    }
    // ...
}
```

#### 方式 3：JWT 认证
```go
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
    authHeader := r.Header.Get("Authorization")
    token := strings.TrimPrefix(authHeader, "Bearer ")

    claims, err := s.jwtService.Verify(token)
    if err != nil {
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return
    }

    userID := claims["sub"].(string)
    // ...
}
```

---

## 总结

### 核心设计亮点

1. **依赖注入**：Redis 客户端外部注入，易于测试和共享连接池
2. **组件协调**：Server 作为协调者，清晰的组件边界
3. **优雅关闭**：Context 取消 + HTTP Shutdown，零数据丢失
4. **回调机制**：Router 通过回调解耦，符合依赖倒置原则
5. **简洁设计**：使用标准库，避免过度工程

### 代码质量

| 指标 | 评分 | 说明 |
|------|------|------|
| **正确性** | ⭐⭐⭐⭐ | 基本正确，但优雅关闭有改进空间 |
| **性能** | ⭐⭐⭐⭐⭐ | 高效的 goroutine 模型 |
| **可维护性** | ⭐⭐⭐⭐⭐ | 代码清晰，职责明确 |
| **可扩展性** | ⭐⭐⭐⭐ | 易于添加新路由和中间件 |
| **安全性** | ⭐⭐ | 缺少认证和 Origin 检查 |

### 改进建议

1. **优雅关闭增强** (server.go:78)
   ```go
   // 在关闭连接前清理 Redis Presence
   // 等待后台 goroutine 退出
   ```

2. **Origin 检查** (server.go:18)
   ```go
   // 生产环境必须启用 Origin 白名单
   ```

3. **添加认证** (server.go:101)
   ```go
   // Token/JWT/Cookie 认证
   ```

4. **健康检查增强** (server.go:115)
   ```go
   // 检查 Redis 连接、负载状态
   ```

5. **指标完善** (server.go:121)
   ```go
   // 添加 Prometheus 指标
   ```

---

**相关文档**：
- [Handler 实现详解](./02-handler实现详解.md) - 连接处理逻辑
- [Router 实现详解](./01-router实现详解.md) - 消息路由机制
- [架构总览](./README.md) - 系统整体设计
