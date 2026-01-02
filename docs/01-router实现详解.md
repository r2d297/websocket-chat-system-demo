# Router 实现详解 / Router Implementation Details

> **文件 / File**: `internal/router/router.go`
> **作用 / Purpose**: 跨网关消息路由的核心组件 / Core component for cross-gateway message routing

## 目录 / Table of Contents

- [整体架构设计 / Overall Architecture](#整体架构设计--overall-architecture)
- [核心问题 / Core Problem](#核心问题--core-problem)
- [数据结构详解 / Data Structures](#数据结构详解--data-structures)
- [核心流程详解 / Core Processes](#核心流程详解--core-processes)
- [关键设计决策 / Key Design Decisions](#关键设计决策--key-design-decisions)
- [并发安全分析 / Concurrency Safety](#并发安全分析--concurrency-safety)
- [性能优化点 / Performance Optimizations](#性能优化点--performance-optimizations)
- [故障处理 / Fault Handling](#故障处理--fault-handling)

---

## 整体架构设计 / Overall Architecture

### 核心问题 / Core Problem

```
Alice 在 Gateway-01，Bob 在 Gateway-02
Alice is on Gateway-01, Bob is on Gateway-02

如何让 Alice 的消息送达 Bob？
How to deliver Alice's message to Bob?
```

### 解决方案：Redis Pub/Sub 模式 / Solution: Redis Pub/Sub Pattern

```
┌─────────────┐         ┌─────────────┐
│ Gateway-01  │         │ Gateway-02  │
│             │         │             │
│  Alice ────┐│         │┌──── Bob    │
└────────────┼┘         └┼────────────┘
             │           │
        Publish       Subscribe
             │           │
             ▼           ▼
      ┌──────────────────────┐
      │   Redis Pub/Sub      │
      │                      │
      │ gateway:gateway-01   │
      │ gateway:gateway-02   │
      └──────────────────────┘
```

**设计原理 / Design Principle:**
每个 Gateway **订阅自己的频道**，其他 Gateway 通过 **发布消息到目标频道** 来路由消息。

Each Gateway **subscribes to its own channel**, other Gateways route messages by **publishing to the target channel**.

---

## 数据结构详解 / Data Structures

### 1. Message 结构 / Message Structure (router.go:13-18)

```go
type Message struct {
    From    string `json:"from"`     // 发送者 userId / Sender's userId
    To      string `json:"to"`       // 接收者 userId / Receiver's userId
    Content string `json:"content"`  // 消息内容 / Message content
    Type    string `json:"type"`     // 消息类型 / Message type: "direct" or "broadcast"
}
```

**设计要点 / Design Highlights:**
- **轻量级 / Lightweight**: 只包含路由必需的字段 / Contains only routing-essential fields
- **序列化友好 / Serialization-friendly**: JSON 标签用于 Redis 传输 / JSON tags for Redis transmission
- **类型可扩展 / Type extensible**: `Type` 字段支持未来功能（群聊、系统通知等） / `Type` field supports future features (group chat, system notifications, etc.)

### 2. Router 结构 / Router Structure (router.go:24-30)

```go
type Router struct {
    redis     *redis.Client      // Redis 客户端 / Redis client
    gatewayID string              // 本 Gateway 的唯一 ID / This Gateway's unique ID
    pubsub    *redis.PubSub       // Redis 订阅对象 / Redis subscription object
    handler   MessageHandler      // 本地消息处理回调 / Local message handler callback
    done      chan struct{}       // 优雅关闭信号 / Graceful shutdown signal
}
```

**字段解析 / Field Descriptions:**

| 字段 / Field | 作用 / Purpose | 生命周期 / Lifecycle |
|------|------|---------|
| `redis` | 发布消息到其他 Gateway / Publish messages to other Gateways | 整个进程 / Entire process |
| `gatewayID` | 标识自己的频道名 / Identifies own channel name | 启动时设置，不可变 / Set at startup, immutable |
| `pubsub` | 接收其他 Gateway 的消息 / Receive messages from other Gateways | Start 时创建，Stop 时关闭 / Created at Start, closed at Stop |
| `handler` | 将消息交给本地连接管理器 / Deliver messages to local connection manager | 依赖注入 / Dependency injection |
| `done` | 通知 goroutine 退出 / Notify goroutine to exit | 无缓冲 channel / Unbuffered channel |

---

## 核心流程详解 / Core Processes

### 启动流程：Start() / Startup Flow: Start() (router.go:42-61)

```go
func (r *Router) Start(ctx context.Context, handler MessageHandler) error {
    r.handler = handler  // ← 1. 注册本地消息处理器 / Register local message handler

    // ← 2. 订阅自己的频道 / Subscribe to own channel
    channel := r.getGatewayChannel(r.gatewayID)
    // 例如 / Example: gatewayID="gateway-01" → channel="gateway:gateway-01"

    r.pubsub = r.redis.Subscribe(ctx, channel)

    // ← 3. 等待订阅确认（阻塞）/ Wait for subscription confirmation (blocking)
    _, err := r.pubsub.Receive(ctx)
    if err != nil {
        return fmt.Errorf("failed to subscribe: %w", err)
    }

    log.Printf("[Router] Subscribed to channel: %s", channel)

    // ← 4. 启动异步消息处理循环 / Start async message processing loop
    go r.processMessages(ctx)

    return nil
}
```

**关键设计 / Key Designs:**

#### 为什么要等待订阅确认？/ Why wait for subscription confirmation? (router.go:50)

```go
_, err := r.pubsub.Receive(ctx)
```

**问题说明 / Problem:**
Redis Pub/Sub 是**异步**的，`Subscribe()` 调用返回不代表订阅成功。必须等待 Redis 的确认消息，否则：

Redis Pub/Sub is **asynchronous**. `Subscribe()` returning doesn't mean subscription succeeded. Must wait for Redis confirmation, otherwise:

```
时序问题 / Timing Issue:
  T0: Gateway-01 调用 / calls Subscribe("gateway:gateway-01")
  T1: Gateway-02 发布消息到 / publishes message to "gateway:gateway-01"
  T2: Gateway-01 订阅还没生效 / subscription not yet effective
  结果 / Result: 消息丢失！/ Message lost!
```

#### 为什么用 goroutine 处理消息？/ Why use goroutine for message processing? (router.go:58)

```go
go r.processMessages(ctx)
```

**原因说明 / Reason:**
`processMessages()` 是**阻塞的无限循环**，需要在后台运行。如果在主线程运行：

`processMessages()` is a **blocking infinite loop**, must run in background. If run in main thread:
- `Start()` 永远不会返回 / `Start()` never returns
- 主程序无法继续执行（比如启动 HTTP 服务器）/ Main program can't continue (e.g., start HTTP server)

---

### 消息处理循环：processMessages() / Message Processing Loop: processMessages() (router.go:111-143)

**核心引擎 / Core Engine:**
这是整个路由器的**核心引擎**，24/7 运行，处理所有入站消息。

This is the Router's **core engine**, running 24/7, processing all inbound messages.

```go
func (r *Router) processMessages(ctx context.Context) {
    ch := r.pubsub.Channel()  // ← 获取 Redis 订阅的 Go channel / Get Redis subscription Go channel

    for {
        select {
        // ← Case 1: 收到 Redis 消息 / Received Redis message
        case msg := <-ch:
            if msg == nil {
                continue  // Redis 连接问题，跳过 / Redis connection issue, skip
            }

            // 反序列化消息 / Deserialize message
            var routedMsg Message
            if err := json.Unmarshal([]byte(msg.Payload), &routedMsg); err != nil {
                log.Printf("[Router] Failed to unmarshal message: %v", err)
                continue  // 错误的消息格式，跳过 / Invalid message format, skip
            }

            log.Printf("[Router] Received message for delivery: from=%s to=%s",
                       routedMsg.From, routedMsg.To)

            // ← 交给本地连接管理器投递 / Deliver to local connection manager
            if r.handler != nil {
                r.handler(&routedMsg)
            }

        // ← Case 2: 收到关闭信号 / Received shutdown signal
        case <-r.done:
            log.Println("[Router] Stopped message processing")
            return

        // ← Case 3: Context 取消（超时或父级取消）/ Context canceled (timeout or parent cancel)
        case <-ctx.Done():
            log.Println("[Router] Context cancelled, stopping")
            return
        }
    }
}
```

**关键问题 / Key Questions:**

#### 为什么用 select 多路复用？/ Why use select multiplexing? (router.go:115)

**同时监听 3 个 channel / Monitor 3 channels simultaneously:**

```go
select {
    case msg := <-ch:           // 正常消息 / Normal message
    case <-r.done:              // 优雅关闭 / Graceful shutdown
    case <-ctx.Done():          // 超时/取消 / Timeout/cancel
}
```

**如果只用 / If only using** `for msg := range ch`:
- 无法处理关闭信号 / Can't handle shutdown signal
- goroutine 会泄漏 / Goroutine leaks

#### 为什么跳过 nil 消息？/ Why skip nil messages? (router.go:117-119)

```go
if msg == nil {
    continue
}
```

**Redis 客户端在以下情况会发送 `nil` / Redis client sends `nil` in these cases:**
- 订阅/取消订阅事件 / Subscribe/unsubscribe events
- 心跳消息 / Heartbeat messages
- 网络重连 / Network reconnection

这些都不是真正的业务消息。/ These are not actual business messages.

---

### 消息路由：RouteToGateway() / Message Routing: RouteToGateway() (router.go:73-89)

**核心方法 / Core Method:**
这是**发送**消息到其他 Gateway 的核心方法。

This is the core method to **send** messages to other Gateways.

```go
func (r *Router) RouteToGateway(ctx context.Context, targetGatewayID string, msg *Message) error {
    // ← 1. 计算目标 Gateway 的频道名 / Calculate target Gateway's channel name
    channel := r.getGatewayChannel(targetGatewayID)
    // 例如 / Example: targetGatewayID="gateway-02" → channel="gateway:gateway-02"

    // ← 2. 序列化消息为 JSON / Serialize message to JSON
    data, err := json.Marshal(msg)
    if err != nil {
        return fmt.Errorf("failed to marshal message: %w", err)
    }

    // ← 3. 发布到 Redis 频道（非阻塞）/ Publish to Redis channel (non-blocking)
    err = r.redis.Publish(ctx, channel, data).Err()
    if err != nil {
        return fmt.Errorf("failed to publish message: %w", err)
    }

    log.Printf("[Router] Routed message from %s to %s via gateway %s",
               msg.From, msg.To, targetGatewayID)

    return nil
}
```

#### 完整的消息流 / Complete Message Flow

**以 Alice → Bob 为例 / Example: Alice → Bob:**

```
┌─────────────────────────────────────────────────────────────────┐
│  Step 1: Alice 发送消息 "Hello Bob" / Alice sends "Hello Bob"    │
│  ↓                                                               │
│  Gateway-01 的 handler.go:routeMessage()                         │
│  ↓                                                               │
│  查询 Redis / Query Redis: presence:bob → {gwId: "gateway-02"}  │
│  ↓                                                               │
│  Step 2: 调用 / Call router.RouteToGateway("gateway-02", msg)   │
│  ↓                                                               │
│  计算频道 / Calculate channel: "gateway:gateway-02"             │
│  ↓                                                               │
│  序列化 / Serialize: {"from":"alice","to":"bob",...}            │
│  ↓                                                               │
│  Step 3: Redis PUBLISH gateway:gateway-02 {...}                 │
│  ↓                                                               │
│  ════════════════════════════════════════════════════════════   │
│                         Redis Pub/Sub                           │
│  ════════════════════════════════════════════════════════════   │
│  ↓                                                               │
│  Step 4: Gateway-02 的 processMessages() 收到消息 / receives msg │
│  ↓                                                               │
│  反序列化 / Deserialize: msg.To = "bob"                          │
│  ↓                                                               │
│  Step 5: 调用 handler(msg) → deliverMessage(msg)                │
│  ↓                                                               │
│  查找本地连接 / Find local conn: connMgr.GetByUserID("bob")     │
│  ↓                                                               │
│  Step 6: 通过 WebSocket 发送给 Bob / Send to Bob via WebSocket  │
│  ↓                                                               │
│  Bob 收到 / Bob receives: "Hello Bob"                            │
└─────────────────────────────────────────────────────────────────┘
```

---

## 关键设计决策 / Key Design Decisions

### 1. 为什么用 Pub/Sub 而不是 MQ？/ Why Pub/Sub instead of MQ?

| 对比 / Comparison | Redis Pub/Sub | Kafka/RabbitMQ |
|------|--------------|----------------|
| 延迟 / Latency | ~1-2ms | ~5-10ms |
| 持久化 / Persistence | ❌ 内存 / Memory | ✅ 磁盘 / Disk |
| 消息丢失风险 / Message Loss Risk | 高 / High (订阅者离线时 / when subscriber offline) | 低 / Low (持久化 + 重试 / persistence + retry) |
| 运维复杂度 / Ops Complexity | 低 / Low | 高 / High |
| 适用场景 / Use Case | 实时通知 / Real-time notifications | 关键业务消息 / Critical business messages |

**选择 Pub/Sub 的理由 / Reasons for choosing Pub/Sub:**
- WebSocket 本身是**实时、临时**的连接 / WebSocket is **real-time, ephemeral** by nature
- 用户离线时丢消息是**可接受**的（客户端重连后拉取历史消息）/ Message loss when offline is **acceptable** (client pulls history after reconnect)
- 低延迟比可靠性更重要 / Low latency is more important than reliability

**生产环境改进 / Production Improvement:**
```
WebSocket → Gateway → Kafka（持久化 / persistence）→ Gateway → WebSocket
                 ↓
                 DB（离线消息 / offline messages）
```

### 2. 为什么每个 Gateway 订阅自己的频道？/ Why does each Gateway subscribe to its own channel?

**方案 A（本实现）/ Approach A (current)：独立频道 / Independent channels**
```
Gateway-01 订阅 / subscribes gateway:gateway-01
Gateway-02 订阅 / subscribes gateway:gateway-02
```

**优点 / Advantages:**
- ✅ 精准路由，无浪费 / Precise routing, no waste
- ✅ 扩展性好（添加 Gateway 不影响其他）/ Good scalability (adding Gateway doesn't affect others)
- ✅ 故障隔离（一个频道故障不影响其他）/ Fault isolation (one channel failure doesn't affect others)

**方案 B / Approach B：共享频道 / Shared channel**
```
所有 Gateway 订阅 / All Gateways subscribe gateway:all
每条消息带 targetGatewayID / Each message carries targetGatewayID
Gateway 收到后判断是否是自己 / Gateway checks if it's the target
```

**缺点 / Disadvantages:**
- ❌ 大量无用消息（100 个 Gateway 时，99% 的消息被丢弃）/ Massive waste (with 100 Gateways, 99% messages dropped)
- ❌ CPU 浪费在反序列化和判断上 / CPU wasted on deserialization and checking

### 3. 为什么用 Handler 回调而不是直接操作连接？/ Why use Handler callback instead of direct connection manipulation?

```go
type MessageHandler func(msg *Message)

func (r *Router) Start(ctx context.Context, handler MessageHandler) error {
    r.handler = handler
    ...
}
```

**好处 / Benefits:**

#### 依赖倒置 / Dependency Inversion (SOLID Principle)
```
router.go 不依赖 / doesn't depend on gateway.go
    ↓
router 是底层模块 / is lower-level module，只负责消息传输 / only handles message transport
gateway 是上层模块 / is higher-level module，负责业务逻辑 / handles business logic
    ↓
符合 SOLID 原则 / Complies with SOLID principles
```

#### 可测试性 / Testability
```go
// 单元测试时可以 mock handler / Can mock handler in unit tests
mockHandler := func(msg *router.Message) {
    receivedMessages = append(receivedMessages, msg)
}

router.Start(ctx, mockHandler)
```

#### 职责分离 / Separation of Concerns
```
Router 的职责 / Router's responsibilities:
  ✅ Redis Pub/Sub 通信 / Redis Pub/Sub communication
  ✅ 消息序列化/反序列化 / Message serialization/deserialization
  ❌ WebSocket 连接管理（不关心）/ WebSocket connection management (doesn't care)
  ❌ 消息格式转换（不关心）/ Message format conversion (doesn't care)

Gateway 的职责 / Gateway's responsibilities:
  ✅ WebSocket 连接管理 / WebSocket connection management
  ✅ 消息格式转换 / Message format conversion
  ❌ Redis 通信细节（不关心）/ Redis communication details (doesn't care)
```

---

## 并发安全分析 / Concurrency Safety

### 1. goroutine 管理 / Goroutine Management

```go
// Start() 启动一个 goroutine / Start() launches a goroutine
go r.processMessages(ctx)

// Stop() 通知 goroutine 退出 / Stop() signals goroutine to exit
close(r.done)
```

**优雅关闭流程 / Graceful Shutdown Flow:**

```
Step 1: 主线程调用 / Main thread calls router.Stop()
    ↓
Step 2: close(r.done)
    ↓
Step 3: processMessages() 的 select 收到 r.done 信号 / select receives r.done signal
    ↓
Step 4: return，goroutine 退出 / return, goroutine exits
    ↓
Step 5: pubsub.Close() 关闭 Redis 连接 / closes Redis connection
```

### 2. 为什么 `done` 是无缓冲 channel？/ Why is `done` an unbuffered channel?

```go
done chan struct{}  // 无缓冲 / unbuffered
```

**原因 / Reason:**
关闭信号是**广播**，使用 `close()` 而不是发送值：

Shutdown signal is a **broadcast**, using `close()` instead of sending values:

```go
close(r.done)  // 所有阻塞在 <-r.done 的 goroutine 都会立即收到信号
               // All goroutines blocking on <-r.done receive signal immediately
```

如果有缓冲，会浪费内存且无意义（`close()` 不需要缓冲）。

If buffered, wastes memory and meaningless (`close()` doesn't need buffering).

### 3. Handler 的并发调用 / Handler Concurrent Calls

```go
if r.handler != nil {
    r.handler(&routedMsg)  // ← 这里是单线程调用 / Single-threaded call here
}
```

**关键 / Key:**
`processMessages()` 只在**一个 goroutine** 中运行，因此 `handler` 不会被并发调用。

`processMessages()` runs in **only one goroutine**, so `handler` won't be called concurrently.

但 `handler` 内部（`deliverMessage()`）会访问 `ConnectionManager`，后者使用 `sync.Map` 保证并发安全。

But `handler` internally (`deliverMessage()`) accesses `ConnectionManager`, which uses `sync.Map` for concurrency safety.

---

## 性能优化点 / Performance Optimizations

### 1. 消息序列化选择 / Message Serialization Choice

**当前使用 JSON / Currently using JSON:**
```go
data, err := json.Marshal(msg)  // ~1-2μs
```

**替代方案 / Alternatives:**

| 方案 / Approach | 序列化速度 / Speed | 大小 / Size | 可读性 / Readability |
|------|-----------|------|--------|
| JSON | 基准 / Baseline | 100% | ★★★★★ |
| MessagePack | 2-3x 快 / faster | 70% | ★★ |
| Protobuf | 5-10x 快 / faster | 50% | ★ |

**生产环境推荐 / Production Recommendation:** **Protobuf**

```protobuf
message RouteMessage {
    string from = 1;
    string to = 2;
    string content = 3;
    string type = 4;
}
```

### 2. Redis Pipeline（批量发送 / Batch Send）

**当前每条消息一次 `PUBLISH` / Currently one `PUBLISH` per message:**
```go
r.redis.Publish(ctx, channel, data).Err()  // RTT = 1ms
```

**高频场景可以批量 / Can batch in high-frequency scenarios:**
```go
pipe := r.redis.Pipeline()
for _, msg := range messages {
    pipe.Publish(ctx, channel, data)
}
pipe.Exec(ctx)  // 批量执行 / Batch execution，RTT = 1ms
```

### 3. 频道名缓存 / Channel Name Caching

**当前每次计算 / Currently calculates every time:**
```go
channel := fmt.Sprintf("gateway:%s", gatewayID)  // ~100ns
```

**可以缓存 / Can cache:**
```go
type Router struct {
    channelName string  // 在 NewRouter 时计算一次 / Calculate once in NewRouter
}
```

---

## 故障处理 / Fault Handling

### Redis 连接断开怎么办？/ What if Redis connection drops?

```go
ch := r.pubsub.Channel()
for {
    select {
    case msg := <-ch:
        if msg == nil {
            continue  // ← Redis 重连时会返回 nil / Returns nil during reconnection
        }
        ...
    }
}
```

**自动重连 / Automatic Reconnection:**
**go-redis 客户端会自动重连**，但有短暂的消息丢失窗口：

**go-redis client auto-reconnects**, but with a brief message loss window:

```
T0: Redis 宕机 / Redis down
T1: Gateway 检测到连接断开 / Detects disconnection
T2: 自动重连 / Auto reconnect
T3: 重新订阅频道 / Re-subscribe to channel
    ↓
T1-T3 之间的消息丢失 / Messages lost between T1-T3
```

**解决方案（生产环境）/ Solutions (production):**
```
1. Redis Sentinel（主从切换 / Master-slave failover）
2. Redis Cluster（分片 + 高可用 / Sharding + HA）
3. 消息持久化到 Kafka / Message persistence to Kafka（避免丢失 / avoid loss）
```

### 消息积压怎么办？/ What if messages backlog?

```go
case msg := <-ch:
    // 如果 handler 处理太慢 / If handler is too slow，ch 会积压 / ch will backlog
    r.handler(&routedMsg)
```

**go-redis 的 Channel 有默认缓冲（100）/ go-redis Channel has default buffer (100):**
```go
pubsub.Channel()  // 内部 buffer = 100 / internal buffer = 100
```

超过 100 条未处理消息时，**新消息会被丢弃** / Beyond 100 unprocessed messages, **new messages dropped**.

**改进方案 / Improvement:**
```go
// 使用工作池异步处理 / Use worker pool for async processing
case msg := <-ch:
    go func(m *redis.Message) {
        r.handler(&routedMsg)
    }(msg)
```

**注意 / Note:** 要注意**消息顺序**会被打乱 / Message **ordering** will be disrupted.

---

## 总结 / Summary

### Router 的核心设计思想 / Router's Core Design Philosophy

1. **解耦通信层与业务层 / Decouple transport and business layers**
   - Router 只管消息传输 / Only handles message transport
   - Gateway 管连接和业务逻辑 / Handles connections and business logic

2. **单一职责原则 / Single Responsibility Principle**
   - `RouteToGateway()`: 发送 / Sending
   - `processMessages()`: 接收 / Receiving
   - `handler`: 本地投递 / Local delivery

3. **优雅退出设计 / Graceful Shutdown Design**
   - `done` channel 通知退出 / Signals exit
   - `ctx.Done()` 处理超时 / Handles timeout
   - `pubsub.Close()` 清理资源 / Cleans up resources

4. **可扩展性 / Scalability**
   - 添加新 Gateway 无需修改代码 / Add new Gateway without code changes
   - 支持 broadcast 等扩展功能 / Supports broadcast and other extensions
   - 消息格式易于演进 / Message format easy to evolve

### 关键代码行 / Key Code Lines

| 行号 / Line | 代码 / Code | 作用 / Purpose |
|------|------|------|
| 47 | `r.redis.Subscribe(ctx, channel)` | 订阅自己的频道 / Subscribe to own channel |
| 50 | `r.pubsub.Receive(ctx)` | 等待订阅确认 / Wait for subscription confirmation |
| 58 | `go r.processMessages(ctx)` | 启动异步消息处理 / Start async message processing |
| 81 | `r.redis.Publish(ctx, channel, data)` | 发布消息到其他 Gateway / Publish message to other Gateways |
| 112 | `ch := r.pubsub.Channel()` | 获取 Redis 消息 channel / Get Redis message channel |
| 130 | `r.handler(&routedMsg)` | 交给本地连接管理器 / Deliver to local connection manager |

---

**生产级设计 / Production-Grade Design:**
这个 Router 实现是**生产级分布式 WebSocket 架构的标准模式**，被 Slack、Discord 等公司广泛使用。🎯

This Router implementation is the **standard pattern for production-grade distributed WebSocket architecture**, widely used by companies like Slack and Discord. 🎯
