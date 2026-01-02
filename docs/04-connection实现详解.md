# Connection 实现详解

> 本地连接管理 - 高性能双向索引设计

## 目录

- [整体架构定位](#整体架构定位)
- [数据结构](#数据结构)
  - [Connection 结构体](#connection-结构体)
  - [ConnectionManager 结构体](#connectionmanager-结构体)
- [核心方法详解](#核心方法详解)
  - [Connection 方法](#connection-方法)
  - [ConnectionManager 方法](#connectionmanager-方法)
- [双向索引设计](#双向索引设计)
- [并发安全保证](#并发安全保证)
- [sync.Map 深入分析](#syncmap-深入分析)
- [性能优化](#性能优化)
- [设计决策](#设计决策)

---

## 整体架构定位

Connection 模块是每个 Gateway **本地的**连接管理器，负责维护该 Gateway 上所有活跃的 WebSocket 连接。

### 在架构中的位置

```
┌──────────────────────────────────────────────┐
│         Gateway-01 (localhost:8080)          │
│                                              │
│  ┌────────────────────────────────────────┐ │
│  │   ConnectionManager (本地)             │ │
│  │                                        │ │
│  │   userToConn: {                        │ │
│  │     "alice" → Connection{              │ │
│  │       ID: "conn-aaa"                   │ │
│  │       Conn: websocket.Conn             │ │
│  │     }                                  │ │
│  │   }                                    │ │
│  │                                        │ │
│  │   connToUser: {                        │ │
│  │     "conn-aaa" → Connection{...}       │ │
│  │   }                                    │ │
│  └────────────────────────────────────────┘ │
└──────────────────────────────────────────────┘

┌──────────────────────────────────────────────┐
│         Gateway-02 (localhost:8081)          │
│                                              │
│  ┌────────────────────────────────────────┐ │
│  │   ConnectionManager (独立实例)         │ │
│  │                                        │ │
│  │   userToConn: {                        │ │
│  │     "bob" → Connection{                │ │
│  │       ID: "conn-bbb"                   │ │
│  │       Conn: websocket.Conn             │ │
│  │     }                                  │ │
│  │   }                                    │ │
│  └────────────────────────────────────────┘ │
└──────────────────────────────────────────────┘
```

### 与 Presence 的区别

| 特性 | Connection (本地) | Presence (全局) |
|------|------------------|-----------------|
| **存储位置** | Gateway 进程内存 | Redis 集群 |
| **作用范围** | 单个 Gateway | 所有 Gateway |
| **存储内容** | WebSocket 连接对象 | 路由信息（gwID + connID） |
| **查询延迟** | ~0.01ms（内存） | ~1ms（网络 RTT） |
| **用途** | 本地消息投递 | 跨网关路由 |

**协作流程**：
```
消息路由完整流程：

1. Handler 收到消息 "alice → bob"
2. 查询 Presence.Get("bob") → gwId=gateway-02, connId=conn-bbb
3. Router.RouteToGateway("gateway-02", message)
4. Gateway-02 收到 Redis Pub/Sub 消息
5. Gateway-02 的 Handler.deliverMessage("bob", message)
6. ConnectionManager.GetByUserID("bob") → Connection{Conn: websocket}
7. Connection.Send(message) → 发送到 WebSocket
```

---

## 数据结构

### Connection 结构体 (connection.go:10-17)

```go
type Connection struct {
    ID       string
    UserID   string
    Conn     *websocket.Conn
    LastPing time.Time
    mu       sync.Mutex
}
```

#### 字段详解

| 字段 | 类型 | 用途 | 示例 |
|------|------|------|------|
| `ID` | string | 连接唯一标识（UUID） | `"2563bded-1363-40f7..."` |
| `UserID` | string | 所属用户 ID | `"alice"` |
| `Conn` | *websocket.Conn | WebSocket 连接对象 | gorilla/websocket 连接 |
| `LastPing` | time.Time | 最后一次心跳时间 | `2025-12-25 10:30:45` |
| `mu` | sync.Mutex | 保护并发写入的锁 | - |

#### 为什么需要 ID 和 UserID 两个字段？

**双重标识的必要性**：

```
场景 1：同一用户多连接（多设备登录）

alice 从手机和电脑同时连接：
- Connection{ID: "conn-001", UserID: "alice", ...}  // 手机
- Connection{ID: "conn-002", UserID: "alice", ...}  // 电脑

如果只用 UserID 作为标识：
- 第二个连接会覆盖第一个
- 手机连接丢失 ❌

如果有独立的 Connection ID：
- 两个连接共存 ✅
- 但当前实现只保留最新的（业务选择）
```

**当前实现的行为**：
```go
// connection.go:70-72
func (cm *ConnectionManager) Add(conn *Connection) {
    cm.userToConn.Store(conn.UserID, conn)  // 同一 UserID 会覆盖
    cm.connToUser.Store(conn.ID, conn)
}
```

当前设计：**一个 UserID 只对应一个 Connection**（后连接挤掉先连接）。

#### LastPing 的作用

**用途**：心跳超时检测

```go
// connection.go:118-139
func (cm *ConnectionManager) CheckHealth(timeout time.Duration) int {
    now := time.Now()
    cm.userToConn.Range(func(_, value interface{}) bool {
        conn := value.(*Connection)
        if now.Sub(conn.GetLastPing()) > timeout {  // 超过 90s 未心跳
            toRemove = append(toRemove, conn)
        }
        return true
    })
    // ... 关闭并移除超时连接
}
```

**时间轴示例**：
```
T=0s:   Connection 创建，LastPing = now
T=30s:  收到心跳，UpdatePing() → LastPing = now
T=60s:  收到心跳，UpdatePing() → LastPing = now
T=90s:  收到心跳，UpdatePing() → LastPing = now
...     持续心跳，连接保活

异常情况：
T=0s:   LastPing = now
T=30s:  ❌ 心跳丢失
T=60s:  ❌ 心跳丢失
T=90s:  ❌ 心跳丢失
T=91s:  CheckHealth() → now.Sub(LastPing) = 91s > 90s → 关闭连接
```

#### mu 锁的粒度

**锁的保护范围**：
```go
// 保护的操作：
func (c *Connection) UpdatePing() {
    c.mu.Lock()           // 🔒 加锁
    c.LastPing = time.Now()
    c.mu.Unlock()         // 🔓 解锁
}

func (c *Connection) Send(messageType int, data []byte) error {
    c.mu.Lock()           // 🔒 加锁
    defer c.mu.Unlock()
    return c.Conn.WriteMessage(messageType, data)  // WebSocket 写入不是并发安全的
}
```

**为什么不保护整个 Connection？**

| 方案 | 粒度 | 并发性能 | 复杂度 |
|------|------|---------|--------|
| **全局锁** | ConnectionManager 级别 | ❌ 差 | ✅ 简单 |
| **对象锁 ✅** | 单个 Connection 级别 | ✅ 好 | ✅ 适中 |
| **字段锁** | 每个字段独立锁 | ✅ 极好 | ❌ 复杂 |

**当前设计的好处**：
- Alice 发送消息时不会阻塞 Bob 发送消息（每个 Connection 独立锁）
- 粒度合理，性能与复杂度平衡

---

### ConnectionManager 结构体 (connection.go:55-62)

```go
type ConnectionManager struct {
    // Bidirectional mappings
    userToConn sync.Map // userID -> *Connection
    connToUser sync.Map // connID -> *Connection

    mu sync.RWMutex
}
```

#### 核心设计：双向映射

**为什么需要两个 Map？**

```
查询场景 1：根据 UserID 查找连接
调用位置：handler.go:182 (deliverMessage)
需求：收到消息 "发给 alice"，需要找到 alice 的 WebSocket 连接

查询场景 2：根据 ConnID 查找连接
调用位置：（未来扩展）监控、统计、日志关联
需求：Redis 返回 connID，需要找到对应的连接

如果只有 userToConn：
- 场景 1：O(1) ✅
- 场景 2：O(N) 遍历所有连接 ❌

双向索引：
- 场景 1：O(1) ✅
- 场景 2：O(1) ✅
- 代价：2x 内存 + 维护一致性
```

**数据一致性保证**：
```go
// Add 方法必须同时更新两个 Map
func (cm *ConnectionManager) Add(conn *Connection) {
    cm.userToConn.Store(conn.UserID, conn)
    cm.connToUser.Store(conn.ID, conn)
}

// Remove 方法必须同时删除两个 Map
func (cm *ConnectionManager) Remove(conn *Connection) {
    cm.userToConn.Delete(conn.UserID)
    cm.connToUser.Delete(conn.ID)
}
```

**不一致的风险**：
```go
// ❌ 错误示例：只更新一个 Map
cm.userToConn.Delete(conn.UserID)
// 忘记删除 connToUser
// 结果：内存泄漏 + 查询结果不一致
```

#### mu 字段的疑问

**代码中的 `mu sync.RWMutex` 字段实际上未使用！**

```go
type ConnectionManager struct {
    userToConn sync.Map
    connToUser sync.Map
    mu sync.RWMutex  // ❓ 这个锁从未被使用
}
```

**为什么不需要额外的锁？**
- `sync.Map` 本身是**并发安全**的
- 不需要外部锁保护

**推测**：可能是早期设计遗留，可以安全删除。

**改进建议**：
```go
type ConnectionManager struct {
    // Bidirectional mappings (both are thread-safe)
    userToConn sync.Map // userID -> *Connection
    connToUser sync.Map // connID -> *Connection
}
```

---

## 核心方法详解

### Connection 方法

#### NewConnection - 构造函数 (connection.go:19-27)

```go
func NewConnection(id, userID string, conn *websocket.Conn) *Connection {
    return &Connection{
        ID:       id,
        UserID:   userID,
        Conn:     conn,
        LastPing: time.Now(),  // 初始化为当前时间
    }
}
```

**调用位置**：`handler.go:38`
```go
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
    wsConn, _ := upgrader.Upgrade(w, r, nil)
    connID := uuid.New().String()
    conn := NewConnection(connID, "", wsConn)  // UserID 初始为空
    s.handleConnection(wsConn, connID)
}
```

**为什么 UserID 初始为空？**
```
WebSocket 连接流程：
1. 客户端建立 WebSocket 连接（此时未认证）
2. 创建 Connection{UserID: ""} ← 匿名连接
3. 客户端发送 register 消息 {"type": "register", "userId": "alice"}
4. Handler 更新 Connection.UserID = "alice"
5. ConnectionManager.Add(conn) ← 此时才加入管理器

这种设计允许：
- 连接建立与用户认证分离
- 未认证的连接不会进入 ConnectionManager
```

#### UpdatePing - 更新心跳时间 (connection.go:29-34)

```go
func (c *Connection) UpdatePing() {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.LastPing = time.Now()
}
```

**调用位置**：`handler.go:121`
```go
case "heartbeat":
    c.UpdatePing()
    if err := s.presenceManager.Refresh(ctx, c.userID); err != nil {
        log.Printf("[Handler] Failed to refresh heartbeat: %v", err)
    }
```

**并发安全性**：
```
场景：多个 goroutine 同时更新 LastPing

Without Lock:
Goroutine A: read LastPing (T1)
Goroutine B: read LastPing (T1)
Goroutine A: write LastPing = T2
Goroutine B: write LastPing = T3  ← 可能覆盖 T2（数据竞争）

With Lock:
Goroutine A: Lock → write T2 → Unlock
Goroutine B: Lock (等待 A) → write T3 → Unlock  ✅ 顺序执行
```

#### GetLastPing - 读取心跳时间 (connection.go:36-41)

```go
func (c *Connection) GetLastPing() time.Time {
    c.mu.Lock()
    defer c.mu.Unlock()
    return c.LastPing
}
```

**为什么读取也要加锁？**

**不加锁的风险**：
```go
// ❌ 不安全的读取
func (c *Connection) GetLastPing() time.Time {
    return c.LastPing  // 可能读到 time.Time 结构体的一半数据（撕裂读）
}
```

**time.Time 的内部结构**：
```go
type Time struct {
    wall uint64  // 8 字节
    ext  int64   // 8 字节
    loc  *Location  // 8 字节指针
}
// 总计 24 字节，非原子操作
```

在 64 位机器上，读取 24 字节不是原子操作，可能发生：
```
Thread A 写入：{wall: 100, ext: 200, loc: &UTC}
Thread B 读取：{wall: 100, ext: 旧值, loc: &UTC}  ← 撕裂读
```

**结论**：即使是读取，也需要加锁保证原子性。

#### Send - 发送消息 (connection.go:43-48)

```go
func (c *Connection) Send(messageType int, data []byte) error {
    c.mu.Lock()
    defer c.mu.Unlock()
    return c.Conn.WriteMessage(messageType, data)
}
```

**为什么需要锁？**

**gorilla/websocket 文档明确说明**：
> Connections support one concurrent reader and one concurrent writer.
> Applications are responsible for ensuring that no more than one goroutine calls the write methods.

**并发写入的风险**：
```
Goroutine A: conn.WriteMessage(TextMessage, "Hello")
Goroutine B: conn.WriteMessage(TextMessage, "World")

Without Lock:
客户端可能收到：
- "HeWlolrllod" ← 数据交错
- 或连接崩溃

With Lock:
客户端收到：
- "Hello"
- "World"
✅ 顺序正确
```

**调用位置**：`handler.go:182`
```go
func (s *Server) deliverMessage(userID string, msg *Message) error {
    conn, ok := s.connManager.GetByUserID(userID)
    if !ok {
        return fmt.Errorf("user %s not connected locally", userID)
    }

    data, _ := json.Marshal(msg)
    if err := conn.Send(websocket.TextMessage, data); err != nil {
        return err
    }
    return nil
}
```

#### Close - 关闭连接 (connection.go:50-53)

```go
func (c *Connection) Close() error {
    return c.Conn.Close()
}
```

**为什么 Close 不需要锁？**

**gorilla/websocket 的保证**：
- `Close()` 方法内部是线程安全的
- 可以在任何 goroutine 调用
- 会自动中断正在进行的 `ReadMessage()` 和 `WriteMessage()`

**调用位置**：
1. `handler.go:106` - handleConnection defer
2. `connection.go:133` - CheckHealth 清理超时连接

---

### ConnectionManager 方法

#### NewConnectionManager - 构造函数 (connection.go:64-67)

```go
func NewConnectionManager() *ConnectionManager {
    return &ConnectionManager{}
}
```

**sync.Map 的零值可用性**：
```go
var cm ConnectionManager  // 零值初始化
cm.userToConn.Store("alice", conn)  // ✅ 可以直接使用
```

Go 的 `sync.Map` 设计为零值可用，无需显式初始化。

#### Add - 添加连接 (connection.go:69-73)

```go
func (cm *ConnectionManager) Add(conn *Connection) {
    cm.userToConn.Store(conn.UserID, conn)
    cm.connToUser.Store(conn.ID, conn)
}
```

**关键点：覆盖语义**

```go
// 首次添加
conn1 := &Connection{ID: "conn-001", UserID: "alice"}
cm.Add(conn1)
// userToConn: {"alice" → conn1}
// connToUser: {"conn-001" → conn1}

// 同一用户重新连接（覆盖）
conn2 := &Connection{ID: "conn-002", UserID: "alice"}
cm.Add(conn2)
// userToConn: {"alice" → conn2}  ← 覆盖旧连接
// connToUser: {"conn-001" → conn1, "conn-002" → conn2}  ← conn-001 仍在！
```

**潜在问题**：connToUser 会保留旧的 connID！

**修复建议**：
```go
func (cm *ConnectionManager) Add(conn *Connection) {
    // 如果 UserID 已存在，先删除旧连接的 connID 映射
    if old, ok := cm.userToConn.Load(conn.UserID); ok {
        oldConn := old.(*Connection)
        cm.connToUser.Delete(oldConn.ID)
    }

    cm.userToConn.Store(conn.UserID, conn)
    cm.connToUser.Store(conn.ID, conn)
}
```

#### Remove - 移除连接 (connection.go:75-79)

```go
func (cm *ConnectionManager) Remove(conn *Connection) {
    cm.userToConn.Delete(conn.UserID)
    cm.connToUser.Delete(conn.ID)
}
```

**调用位置**：
1. `handler.go:227` - 用户断开连接
2. `connection.go:134` - 心跳超时清理

**幂等性**：
```go
cm.Remove(conn)
cm.Remove(conn)  // ✅ 不会报错，Delete 不存在的 key 是安全的
```

#### GetByUserID - 根据用户 ID 查找 (connection.go:81-88)

```go
func (cm *ConnectionManager) GetByUserID(userID string) (*Connection, bool) {
    val, ok := cm.userToConn.Load(userID)
    if !ok {
        return nil, false
    }
    return val.(*Connection), true
}
```

**调用位置**：`handler.go:175`
```go
func (s *Server) deliverMessage(userID string, msg *Message) error {
    conn, ok := s.connManager.GetByUserID(userID)
    if !ok {
        return fmt.Errorf("user %s not connected locally", userID)
    }
    // ...
}
```

**性能**：
- `sync.Map.Load()` 快速路径：O(1) 原子读取，无锁
- 慢速路径：加读锁，查找内部 map

#### GetByConnID - 根据连接 ID 查找 (connection.go:90-97)

```go
func (cm *ConnectionManager) GetByConnID(connID string) (*Connection, bool) {
    val, ok := cm.connToUser.Load(connID)
    if !ok {
        return nil, false
    }
    return val.(*Connection), true
}
```

**使用场景**（未来扩展）：
```go
// 监控 API：查询连接详情
GET /api/connections/:connID

func getConnectionInfo(connID string) {
    conn, ok := cm.GetByConnID(connID)
    if !ok {
        return "Connection not found"
    }
    return ConnectionInfo{
        UserID:   conn.UserID,
        LastPing: conn.GetLastPing(),
    }
}
```

#### Count - 统计连接数 (connection.go:99-107)

```go
func (cm *ConnectionManager) Count() int {
    count := 0
    cm.userToConn.Range(func(_, _ interface{}) bool {
        count++
        return true
    })
    return count
}
```

**调用位置**：`handler.go:241` (统计接口)
```go
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
    stats := map[string]interface{}{
        "gateway_id":         s.gatewayID,
        "active_connections": s.connManager.Count(),
        "uptime_seconds":     time.Since(s.startTime).Seconds(),
    }
    json.NewEncoder(w).Encode(stats)
}
```

**性能特性**：
- O(N) 遍历所有连接
- `Range` 会获取内部读锁
- 不适合高频调用（建议缓存结果）

**优化建议**：
```go
type ConnectionManager struct {
    userToConn sync.Map
    connToUser sync.Map
    count      atomic.Int64  // ✅ 原子计数器
}

func (cm *ConnectionManager) Add(conn *Connection) {
    cm.userToConn.Store(conn.UserID, conn)
    cm.connToUser.Store(conn.ID, conn)
    cm.count.Add(1)  // O(1) 更新
}

func (cm *ConnectionManager) Count() int {
    return int(cm.count.Load())  // O(1) 读取
}
```

#### ForEach - 遍历所有连接 (connection.go:109-115)

```go
func (cm *ConnectionManager) ForEach(fn func(*Connection)) {
    cm.userToConn.Range(func(_, value interface{}) bool {
        fn(value.(*Connection))
        return true
    })
}
```

**使用场景**：广播消息
```go
// 发送系统公告给所有在线用户
func (s *Server) broadcast(message string) {
    s.connManager.ForEach(func(conn *Connection) {
        data, _ := json.Marshal(Message{Type: "broadcast", Content: message})
        conn.Send(websocket.TextMessage, data)
    })
}
```

**并发安全性**：
```
问题：遍历时能否修改 Map？

答案：✅ 可以，但有限制

sync.Map.Range 的保证：
- 遍历时可以并发 Store/Delete
- 遍历会看到部分新写入的数据（弱一致性）
- 不会崩溃或死锁

示例：
cm.ForEach(func(conn *Connection) {
    if conn.GetLastPing().Before(cutoff) {
        cm.Remove(conn)  // ✅ 安全，但可能跳过部分新连接
    }
})
```

**更安全的做法**（CheckHealth 的实现）：
```go
// 两阶段：先收集，再删除
var toRemove []*Connection
cm.userToConn.Range(func(_, value interface{}) bool {
    conn := value.(*Connection)
    if shouldRemove(conn) {
        toRemove = append(toRemove, conn)
    }
    return true
})

// 在遍历结束后统一删除
for _, conn := range toRemove {
    cm.Remove(conn)
}
```

#### CheckHealth - 健康检查 (connection.go:118-139)

```go
func (cm *ConnectionManager) CheckHealth(timeout time.Duration) int {
    removed := 0
    now := time.Now()

    var toRemove []*Connection

    // 阶段 1: 收集超时连接
    cm.userToConn.Range(func(_, value interface{}) bool {
        conn := value.(*Connection)
        if now.Sub(conn.GetLastPing()) > timeout {
            toRemove = append(toRemove, conn)
        }
        return true
    })

    // 阶段 2: 关闭并移除
    for _, conn := range toRemove {
        conn.Close()
        cm.Remove(conn)
        removed++
    }

    return removed
}
```

**调用位置**：`handler.go:147` (定时任务)
```go
func (s *Server) heartbeatChecker(ctx context.Context) {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            removed := s.connManager.CheckHealth(90 * time.Second)
            if removed > 0 {
                log.Printf("[Handler] Removed %d stale connections", removed)
            }
        case <-ctx.Done():
            return
        }
    }
}
```

**两阶段设计的必要性**：
```
为什么不在 Range 内直接 Remove？

方案 A（危险）：
cm.userToConn.Range(func(_, value interface{}) bool {
    conn := value.(*Connection)
    if timeout {
        cm.Remove(conn)  // ⚠️ 在遍历中修改
    }
    return true
})

风险：
- Range 可能跳过部分连接
- 可能重复处理某些连接
- 行为不确定

方案 B（安全 ✅）：
先收集 → 再删除
- 明确的两阶段
- 行为可预测
- 代码清晰
```

---

## 双向索引设计

### 为什么用双向索引？

**查询需求分析**：

| 查询方式 | 频率 | 调用位置 | 复杂度要求 |
|---------|------|---------|-----------|
| `GetByUserID` | 🔥 极高 | 每条消息投递 | O(1) |
| `GetByConnID` | 🔥 中等 | 监控、日志 | O(1) |

**方案对比**：

#### 方案 A：只用 userToConn

```go
type ConnectionManager struct {
    userToConn sync.Map  // userID → *Connection
}

func (cm *ConnectionManager) GetByUserID(userID string) (*Connection, bool) {
    return cm.userToConn.Load(userID)  // O(1) ✅
}

func (cm *ConnectionManager) GetByConnID(connID string) (*Connection, bool) {
    // ❌ O(N) 遍历所有连接
    var result *Connection
    cm.userToConn.Range(func(_, value interface{}) bool {
        conn := value.(*Connection)
        if conn.ID == connID {
            result = conn
            return false  // 找到后停止
        }
        return true
    })
    return result, result != nil
}
```

**缺点**：
- GetByConnID 性能差（O(N)）
- 10,000 连接时可能耗时数十毫秒

#### 方案 B：双向索引 ✅

```go
type ConnectionManager struct {
    userToConn sync.Map  // userID → *Connection
    connToUser sync.Map  // connID → *Connection
}

func (cm *ConnectionManager) GetByUserID(userID string) (*Connection, bool) {
    return cm.userToConn.Load(userID)  // O(1) ✅
}

func (cm *ConnectionManager) GetByConnID(connID string) (*Connection, bool) {
    return cm.connToUser.Load(connID)  // O(1) ✅
}
```

**优点**：
- 所有查询 O(1)
- 高并发性能优秀

**代价**：
- 2x 内存（每个连接存储两次指针）
- 维护一致性（Add/Remove 必须同步）

### 内存开销分析

**单个连接的内存占用**：

```
Connection 对象：
- ID (string):        24 字节（string header）+ ~36 字节（UUID）
- UserID (string):    24 字节 + ~10 字节（平均）
- Conn (*websocket):  8 字节（指针）
- LastPing (time):    24 字节
- mu (sync.Mutex):    8 字节
总计：~134 字节

双向索引开销：
- userToConn entry:   ~50 字节（map entry overhead）
- connToUser entry:   ~50 字节
总计：~100 字节

每个连接总开销：~234 字节
10,000 连接：~2.3 MB
```

**结论**：内存开销可忽略，换来的性能提升非常值得。

---

## 并发安全保证

### 三层并发保护

#### 层次 1：Connection 对象级锁

```go
type Connection struct {
    mu sync.Mutex
}

func (c *Connection) Send(msg) {
    c.mu.Lock()         // 🔒 保护单个连接的写入
    defer c.mu.Unlock()
    c.Conn.WriteMessage(msg)
}
```

**保护范围**：单个 Connection 的内部状态

#### 层次 2：sync.Map 内部锁

```go
cm.userToConn.Store("alice", conn)  // sync.Map 内部自动加锁
cm.userToConn.Load("bob")           // 快速路径无锁，慢速路径加读锁
```

**保护范围**：Map 的并发读写

#### 层次 3：业务逻辑串行化（可选）

```go
// 如果需要原子性保证多个操作：
func (cm *ConnectionManager) ReplaceConnection(userID string, newConn *Connection) {
    // ⚠️ 这段代码有竞态！
    if old, ok := cm.GetByUserID(userID); ok {
        cm.Remove(old)
    }
    cm.Add(newConn)
}
```

**问题**：两个操作之间可能被其他 goroutine 插入。

**解决方案**：使用 sync.Map 的 CompareAndSwap（Go 1.20+）
```go
func (cm *ConnectionManager) ReplaceConnection(userID string, oldConn, newConn *Connection) bool {
    return cm.userToConn.CompareAndSwap(userID, oldConn, newConn)
}
```

---

## sync.Map 深入分析

### 为什么用 sync.Map？

**Go 中并发 Map 的三种方案**：

| 方案 | 读性能 | 写性能 | 适用场景 |
|------|--------|--------|---------|
| `map + sync.RWMutex` | ⚠️ 中（需加读锁） | ⚠️ 中（需加写锁） | 读写均衡 |
| `sync.Map` ✅ | ✅ 极高（快速路径无锁） | ⚠️ 中 | **读多写少** |
| 分片锁 Map | ✅ 高 | ✅ 高 | 极高并发 |

**ConnectionManager 的读写比例**：
```
读操作（GetByUserID）：
- 每条消息投递都要查询
- QPS 可能达到数万次/秒

写操作（Add/Remove）：
- 用户连接/断开时触发
- QPS 通常只有数百次/秒

读写比：100:1 或更高
```

**结论**：sync.Map 是最佳选择。

### sync.Map 的内部原理

**双层存储结构**：

```go
type Map struct {
    mu     Mutex
    read   atomic.Pointer[readOnly]  // 只读 map（快速路径）
    dirty  map[interface{}]*entry    // 可写 map（慢速路径）
    misses int                        // 未命中计数
}
```

**读取流程**：

```
Load("alice"):

1. 快速路径（无锁）：
   read := m.read.Load()  // 原子读取 read map
   if entry, ok := read.m["alice"]; ok {
       return entry.load()  // ✅ 命中，返回（无锁！）
   }

2. 慢速路径（加锁）：
   m.mu.Lock()
   read = m.read.Load()
   if entry, ok := read.m["alice"]; ok {  // Double-check
       m.mu.Unlock()
       return entry.load()
   }

   entry, ok := m.dirty["alice"]  // 查找 dirty map
   m.mu.Unlock()
   return entry, ok
```

**写入流程**：

```
Store("alice", conn):

1. 检查 read map（无锁）：
   if entry, ok := read.m["alice"]; ok {
       if entry.tryStore(&conn) {  // CAS 更新
           return  // ✅ 快速路径成功
       }
   }

2. 慢速路径（加锁）：
   m.mu.Lock()
   m.dirty["alice"] = &entry{p: &conn}
   m.misses++
   if m.misses > len(read.m) {  // 未命中过多
       m.read.Store(readOnly{m: m.dirty})  // 提升 dirty 为 read
       m.dirty = nil
       m.misses = 0
   }
   m.mu.Unlock()
```

**性能特性**：

| 操作 | 命中 read | 命中 dirty | 性能 |
|------|----------|-----------|------|
| Load (90%+) | ✅ | - | 🚀 极快（无锁） |
| Load (10%-) | - | ✅ | ⚠️ 慢（加锁） |
| Store | ✅ | - | 🚀 快（CAS） |
| Store | - | ✅ | ⚠️ 慢（加锁） |

### 为什么适合 ConnectionManager？

**访问模式分析**：

```
稳定期（99% 时间）：
- 用户连接后长时间保持
- 大量 GetByUserID 查询（都命中 read map）
- 极少 Add/Remove

启动期或高峰期（1% 时间）：
- 大量用户同时连接（大量 Store）
- sync.Map 会自动调整 read/dirty

结果：
- 稳定期性能极佳（无锁读取）
- 峰值期性能可接受（自动优化）
```

---

## 性能优化

### 1. 避免全局锁

**反面教材**：
```go
// ❌ 全局锁设计（性能差）
type ConnectionManager struct {
    mu    sync.RWMutex
    conns map[string]*Connection
}

func (cm *ConnectionManager) GetByUserID(userID string) (*Connection, bool) {
    cm.mu.RLock()         // 所有读取都要等待
    defer cm.mu.RUnlock()
    conn, ok := cm.conns[userID]
    return conn, ok
}

func (cm *ConnectionManager) Add(conn *Connection) {
    cm.mu.Lock()          // 所有写入都要阻塞所有读
    defer cm.mu.Unlock()
    cm.conns[conn.UserID] = conn
}
```

**问题**：
- 1 个写操作会阻塞所有读操作
- 10,000 个并发读取会竞争同一个锁

**当前设计的优势**：
```go
// ✅ sync.Map + 对象级锁（高并发）
type ConnectionManager struct {
    userToConn sync.Map  // 无全局锁
}

func (cm *ConnectionManager) GetByUserID(userID string) (*Connection, bool) {
    return cm.userToConn.Load(userID)  // 快速路径无锁
}
```

**并发性能对比**：

| 并发读取 | 全局锁方案 | sync.Map 方案 | 性能提升 |
|---------|-----------|--------------|---------|
| 1000 QPS | ~0.5ms/op | ~0.01ms/op | 50x |
| 10000 QPS | ~5ms/op | ~0.01ms/op | 500x |

### 2. 对象级锁粒度

**Connection 内部的锁只保护该连接**：

```go
// Alice 发送消息
connAlice.Send(msg)  // 🔒 锁 Alice 的 Connection.mu

// 同时，Bob 发送消息（不会被阻塞）
connBob.Send(msg)    // 🔒 锁 Bob 的 Connection.mu

// 两个操作完全并行 ✅
```

**如果是全局锁**：
```go
cm.mu.Lock()
connAlice.Send(msg)
cm.mu.Unlock()

cm.mu.Lock()
connBob.Send(msg)    // ❌ 必须等待 Alice 完成
cm.mu.Unlock()
```

### 3. 两阶段删除（避免遍历中修改）

**安全模式**（当前实现）：
```go
// 阶段 1：收集
var toRemove []*Connection
cm.Range(...)

// 阶段 2：删除
for _, conn := range toRemove {
    cm.Remove(conn)
}
```

**性能影响**：
- 额外内存：O(N) 临时数组
- 好处：行为确定，易调试

### 4. 避免不必要的类型断言

**当前代码**：
```go
val, ok := cm.userToConn.Load(userID)
if !ok {
    return nil, false
}
return val.(*Connection), true  // 类型断言
```

**优化**（使用泛型，Go 1.18+）：
```go
// 未来可以使用泛型版本的 Map
type ConnectionMap[K comparable, V any] struct {
    m sync.Map
}

func (m *ConnectionMap[K, V]) Load(key K) (V, bool) {
    val, ok := m.m.Load(key)
    if !ok {
        var zero V
        return zero, false
    }
    return val.(V), true  // 编译器优化
}
```

---

## 设计决策

### 1. 为什么不支持多设备登录？

**当前行为**：
```go
cm.Add(conn)  // 同一 UserID 会覆盖旧连接
```

**如果要支持多设备**：
```go
type ConnectionManager struct {
    userToConns sync.Map  // userID → []*Connection
    connToUser  sync.Map  // connID → *Connection
}

func (cm *ConnectionManager) Add(conn *Connection) {
    // 追加而非覆盖
    conns, _ := cm.userToConns.LoadOrStore(conn.UserID, &[]*Connection{})
    *conns.(*[]*Connection) = append(*conns.(*[]*Connection), conn)

    cm.connToUser.Store(conn.ID, conn)
}

func (cm *ConnectionManager) GetAllByUserID(userID string) []*Connection {
    conns, ok := cm.userToConns.Load(userID)
    if !ok {
        return nil
    }
    return *conns.(*[]*Connection)
}
```

**选择当前方案的原因**：
- 简化业务逻辑（1 user = 1 connection）
- 降低系统复杂度
- 多设备可通过多个 UserID 实现（alice_mobile, alice_desktop）

### 2. 为什么不持久化连接？

**当前实现**：所有连接只存在内存中

**对比持久化方案**：

| 方案 | Gateway 重启后 | 内存占用 | 复杂度 |
|------|--------------|---------|--------|
| **内存 ✅** | 连接丢失 | 低 | ✅ 简单 |
| **Redis** | 连接仍丢失（WebSocket 无法序列化） | 中 | ⚠️ 复杂 |
| **数据库** | 连接仍丢失 | 高 | ❌ 过度设计 |

**关键认知**：WebSocket 连接**无法持久化**
- TCP 连接绑定到进程
- 进程重启后连接必然断开
- 客户端需要重连

**结论**：内存存储是唯一合理方案。

### 3. 为什么不使用连接池？

**连接池的适用场景**：
- HTTP 客户端连接数据库
- 连接创建成本高
- 连接可复用

**WebSocket 的特点**：
- **长连接**：一旦建立，持续数小时
- **一对一**：每个用户独占一个连接
- **无法复用**：连接绑定到特定用户

**结论**：WebSocket 不适用连接池模式。

### 4. 为什么不添加连接限流？

**潜在风险**：恶意用户快速重连（DoS 攻击）

**当前防护**：无

**改进建议**：
```go
type ConnectionManager struct {
    userToConn    sync.Map
    connToUser    sync.Map
    rateLimiter   *rate.Limiter  // 全局速率限制
}

func (cm *ConnectionManager) Add(conn *Connection) error {
    if !cm.rateLimiter.Allow() {
        return errors.New("too many connections")
    }
    cm.userToConn.Store(conn.UserID, conn)
    cm.connToUser.Store(conn.ID, conn)
    return nil
}
```

**业界实践**：
- Slack: 每个用户最多 5 个并发连接
- Discord: 每 IP 每分钟最多 120 次连接请求

---

## 总结

### 核心设计亮点

1. **双向索引**：userToConn + connToUser，所有查询 O(1)
2. **sync.Map**：读多写少场景的极致优化，快速路径无锁
3. **对象级锁**：细粒度锁提升并发性能
4. **两阶段清理**：安全的遍历删除模式
5. **简洁设计**：1 user = 1 connection，降低复杂度

### 代码质量

| 指标 | 评分 | 说明 |
|------|------|------|
| **正确性** | ⭐⭐⭐⭐ | 并发安全，逻辑清晰（Add 有小瑕疵） |
| **性能** | ⭐⭐⭐⭐⭐ | sync.Map + 双向索引，极致优化 |
| **可维护性** | ⭐⭐⭐⭐⭐ | 代码简洁，易理解 |
| **可扩展性** | ⭐⭐⭐⭐ | 可扩展多设备、限流等功能 |

### 改进建议

1. **修复 Add 方法** (connection.go:69)
   ```go
   // 覆盖旧连接时，清理 connToUser 中的旧 connID
   ```

2. **删除未使用的 mu 字段** (connection.go:61)
   ```go
   // ConnectionManager.mu 从未使用，可删除
   ```

3. **添加计数器优化** (connection.go:100)
   ```go
   // Count() 方法可用 atomic.Int64 优化为 O(1)
   ```

4. **添加限流保护**
   ```go
   // 防止连接 DoS 攻击
   ```

---

**相关文档**：
- [Handler 实现详解](./02-handler实现详解.md) - 如何使用 ConnectionManager
- [Presence 实现详解](./03-presence实现详解.md) - 全局状态 vs 本地连接
- [架构总览](./README.md) - 系统整体设计
