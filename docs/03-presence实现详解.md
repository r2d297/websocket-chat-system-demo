# Presence 实现详解

> Redis 全局状态管理 - 分布式系统的"大脑"

## 目录

- [整体架构定位](#整体架构定位)
- [数据结构](#数据结构)
- [核心方法详解](#核心方法详解)
  - [Register - CAS 注册](#register---cas-注册)
  - [Refresh - 心跳刷新](#refresh---心跳刷新)
  - [Get - 查询用户位置](#get---查询用户位置)
  - [Remove - 清理状态](#remove---清理状态)
  - [IsOnline - 在线检查](#isonline---在线检查)
- [CAS 机制深入分析](#cas-机制深入分析)
- [Lua 脚本详解](#lua-脚本详解)
- [TTL 与心跳设计](#ttl-与心跳设计)
- [竞态条件处理](#竞态条件处理)
- [性能优化](#性能优化)
- [设计决策](#设计决策)

---

## 整体架构定位

Presence Manager 是整个分布式 WebSocket 系统的**状态中枢**，负责回答核心问题：

```
❓ 用户 alice 在哪个 Gateway 上？
✅ Presence: alice 在 gateway-02，连接 ID 是 conn-xyz
```

### 在架构中的位置

```
┌─────────────────────────────────────────────┐
│           Gateway-01 (端口 8080)             │
│  ┌─────────┐      ┌──────────────────┐     │
│  │ Handler │─────▶│ Presence.Get()   │     │
│  └─────────┘      └──────────┬───────┘     │
└─────────────────────────────┼──────────────┘
                               │
                               ▼
                    ┌──────────────────────┐
                    │   Redis Cluster      │
                    │                      │
                    │ presence:alice       │
                    │   gwId: gateway-02   │◀────┐
                    │   connId: conn-xyz   │     │
                    │   ts: 1703512345     │     │
                    └──────────────────────┘     │
                               ▲                 │
                               │                 │
┌──────────────────────────────┼─────────────────┼─┐
│           Gateway-02 (端口 8081)               │ │
│  ┌─────────┐      ┌──────────┴─────────┐      │ │
│  │ Handler │─────▶│ Presence.Register()│──────┘ │
│  └─────────┘      └────────────────────┘        │
└─────────────────────────────────────────────────┘
```

### 核心职责

1. **状态注册**: 用户连接时记录所在 Gateway
2. **位置查询**: 消息路由时查找目标用户位置
3. **心跳刷新**: 保持状态新鲜度，延长 TTL
4. **自动清理**: TTL 过期自动删除僵尸状态
5. **竞态防护**: CAS 机制防止脏写

---

## 数据结构

### 常量定义 (presence.go:11-14)

```go
const (
    presenceKeyPrefix = "presence:"
    presenceTTL       = 90 * time.Second // 3x heartbeat interval
)
```

#### 设计要点

| 常量 | 值 | 设计考量 |
|------|-----|----------|
| `presenceKeyPrefix` | `"presence:"` | Redis 键命名空间，避免冲突 |
| `presenceTTL` | `90s` | 3 倍心跳间隔（30s），允许 2 次心跳丢失 |

**为什么 TTL = 3x 心跳？**
```
时间轴：
0s ────┬──── 30s ────┬──── 60s ────┬──── 90s (过期)
       心跳 1         心跳 2         心跳 3
       ✓             ✗ (丢失)       ✓ (救回)
```
- 1 次丢失：仍有 60s 余地
- 2 次连续丢失：留 30s 缓冲
- 3 次全丢失：判定为离线（合理）

---

### Info 结构体 (presence.go:16-22)

```go
type Info struct {
    UserID    string
    GatewayID string
    ConnID    string
    Timestamp int64
}
```

#### 字段含义

| 字段 | 类型 | 用途 | 示例 |
|------|------|------|------|
| `UserID` | string | 用户唯一标识 | `"alice"` |
| `GatewayID` | string | 所在 Gateway ID | `"gateway-02"` |
| `ConnID` | string | WebSocket 连接 ID | `"2563bded-1363..."` |
| `Timestamp` | int64 | Unix 时间戳（秒） | `1703512345` |

#### 为什么需要 Timestamp？

**核心用途：解决重连竞态**

```
场景：用户快速断开 → 重连

时间线：
T1: Alice 连接到 GW-01 → Register(alice, gw-01, ts=100)
T2: GW-01 崩溃，连接断开
T3: Alice 重连到 GW-02 → Register(alice, gw-02, ts=103)
T4: GW-01 的心跳协程延迟发送 → Refresh(alice, ts=101) ❌

如果没有时间戳：
- T4 的旧心跳会覆盖 T3 的新状态
- Alice 实际在 GW-02，Redis 却记录 GW-01
- 导致消息路由失败

有了时间戳 CAS：
- T4 的 ts=101 < T3 的 ts=103
- Lua 脚本拒绝旧数据写入
- 状态正确保留在 GW-02
```

---

### Manager 结构体 (presence.go:24-27)

```go
type Manager struct {
    redis *redis.Client
}
```

#### 设计模式：依赖注入

```go
// ✅ 外部注入 Redis 客户端
func NewManager(redisClient *redis.Client) *Manager {
    return &Manager{
        redis: redisClient,
    }
}
```

**为什么不在内部创建 Redis 客户端？**

| 方案 | 优点 | 缺点 |
|------|------|------|
| **内部创建** | 使用简单 | 难以测试、配置不灵活 |
| **外部注入 ✅** | 可测试、共享连接池 | 需要调用方管理 |

测试示例：
```go
// 单元测试可以注入 mock Redis
mockRedis := newMockRedisClient()
pm := NewManager(mockRedis)
```

---

## 核心方法详解

### Register - CAS 注册

**方法签名** (presence.go:37)
```go
func (m *Manager) Register(ctx context.Context, userID, gatewayID, connID string) error
```

#### 调用时机

```
用户连接流程：
WebSocket Upgrade → handleConnection → 收到 register 消息 → Presence.Register()
```

调用位置：`internal/gateway/handler.go:89`
```go
if err := s.presenceManager.Register(ctx, msg.UserID, s.gatewayID, c.id); err != nil {
    log.Printf("[Handler] Failed to register user %s: %v", msg.UserID, err)
    return
}
```

#### 实现细节（分步骤）

**步骤 1: 准备参数** (presence.go:38-39)
```go
key := presenceKeyPrefix + userID  // "presence:alice"
timestamp := time.Now().Unix()      // 1703512345
```

**步骤 2: Lua 脚本执行** (presence.go:42-59)

完整脚本解析：
```lua
-- KEYS[1] = "presence:alice"
local key = KEYS[1]

-- ARGV[1] = "gateway-02", ARGV[2] = "conn-xyz", ARGV[3] = "1703512345", ARGV[4] = "90"
local new_gw = ARGV[1]
local new_conn = ARGV[2]
local new_ts = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

-- 获取当前记录的时间戳
local current_ts = redis.call('HGET', key, 'ts')

-- 🔒 CAS 核心：时间戳比较
-- 如果当前时间戳更新，拒绝本次写入（防止旧数据覆盖新数据）
if current_ts and tonumber(current_ts) > new_ts then
    return 0  -- 拒绝
end

-- ✅ 写入新数据（3 个字段）
redis.call('HSET', key, 'gwId', new_gw, 'connId', new_conn, 'ts', new_ts)

-- ⏰ 设置 TTL（90 秒后自动删除）
redis.call('EXPIRE', key, ttl)

return 1  -- 成功
```

**步骤 3: 调用 Lua 脚本** (presence.go:61-62)
```go
result, err := m.redis.Eval(ctx, script, []string{key},
    gatewayID, connID, timestamp, int(presenceTTL.Seconds())).Result()
```

参数映射：
```
KEYS[1]  ← key          ("presence:alice")
ARGV[1]  ← gatewayID    ("gateway-02")
ARGV[2]  ← connID       ("conn-xyz")
ARGV[3]  ← timestamp    (1703512345)
ARGV[4]  ← TTL 秒数     (90)
```

**步骤 4: 错误处理** (presence.go:64-70)
```go
if err != nil {
    return fmt.Errorf("failed to register presence: %w", err)
}

// result = 0 表示 CAS 失败（时间戳旧）
if result.(int64) == 0 {
    return fmt.Errorf("stale update rejected for user %s", userID)
}
```

#### Redis 数据示例

成功注册后的 Redis 状态：
```bash
redis> HGETALL presence:alice
1) "gwId"
2) "gateway-02"
3) "connId"
4) "2563bded-1363-40f7-aa91-beb6a33d59c8"
5) "ts"
6) "1703512345"

redis> TTL presence:alice
(integer) 90
```

---

### Refresh - 心跳刷新

**方法签名** (presence.go:76)
```go
func (m *Manager) Refresh(ctx context.Context, userID string) error
```

#### 调用时机

```
心跳流程：
客户端每 30s 发送 ping → Handler 收到 → Presence.Refresh()
```

调用位置：`internal/gateway/handler.go:121`
```go
case "heartbeat":
    if err := s.presenceManager.Refresh(ctx, c.userID); err != nil {
        log.Printf("[Handler] Failed to refresh heartbeat: %v", err)
    }
```

#### 实现细节

**步骤 1: 更新时间戳** (presence.go:77-80)
```go
key := presenceKeyPrefix + userID
timestamp := time.Now().Unix()
```

**步骤 2: Pipeline 批量操作** (presence.go:81-84)

```go
pipe := m.redis.Pipeline()
pipe.HSet(ctx, key, "ts", timestamp)  // 更新 ts 字段
pipe.Expire(ctx, key, presenceTTL)    // 刷新 TTL 为 90s
_, err := pipe.Exec(ctx)
```

**为什么用 Pipeline？**

| 方案 | 网络往返 | 延迟 |
|------|---------|------|
| 分开执行 | 2 次 RTT | ~2ms |
| Pipeline | 1 次 RTT | ~1ms |

Pipeline 示例：
```
Without Pipeline:
Client → Server: HSET presence:alice ts 1703512345
Server → Client: OK
Client → Server: EXPIRE presence:alice 90
Server → Client: OK
总计：2 次网络往返

With Pipeline:
Client → Server: HSET + EXPIRE (批量)
Server → Client: [OK, OK]
总计：1 次网络往返
```

#### 为什么要刷新 TTL？

```
时间轴（无 TTL 刷新）：
T0:  Register (TTL = 90s)
T30: Heartbeat (只更新 ts，TTL 剩余 60s)
T60: Heartbeat (TTL 剩余 30s)
T90: ❌ Key 过期删除！（即使用户在线）

时间轴（有 TTL 刷新）：
T0:  Register (TTL = 90s)
T30: Heartbeat (TTL 重置为 90s)
T60: Heartbeat (TTL 重置为 90s)
T90: Heartbeat (TTL 重置为 90s)
永不过期（只要心跳持续）
```

**关键点**：每次心跳都重置 TTL 为 90s，而非延长 90s。

---

### Get - 查询用户位置

**方法签名** (presence.go:94)
```go
func (m *Manager) Get(ctx context.Context, userID string) (*Info, error)
```

#### 调用时机

```
消息路由流程：
收到 send 消息 → 查找目标用户位置 → Presence.Get() → Router.RouteToGateway()
```

调用位置：`internal/gateway/handler.go:169`
```go
targetPresence, err := s.presenceManager.Get(ctx, msg.To)
if err != nil {
    log.Printf("[Handler] User %s is offline: %v", msg.To, err)
    return
}
```

#### 实现细节

**步骤 1: 读取 Hash 所有字段** (presence.go:95-100)
```go
key := presenceKeyPrefix + userID

result, err := m.redis.HGetAll(ctx, key).Result()
// result = map[string]string{
//     "gwId":   "gateway-02",
//     "connId": "conn-xyz",
//     "ts":     "1703512345",
// }
```

**步骤 2: 检查用户是否在线** (presence.go:102-104)
```go
if len(result) == 0 {
    return nil, fmt.Errorf("user %s is offline", userID)
}
```

**可能返回空的情况**：
- Key 不存在（从未注册）
- Key 已过期（TTL 耗尽）
- Key 被 Remove 删除

**步骤 3: 解析时间戳** (presence.go:106-109)
```go
timestamp := int64(0)
if ts, ok := result["ts"]; ok {
    fmt.Sscanf(ts, "%d", &timestamp)  // "1703512345" → 1703512345
}
```

**为什么要解析？**
- Redis Hash 值类型是 string
- Go 结构体 `Timestamp` 字段是 int64
- 需要类型转换

**步骤 4: 构造返回值** (presence.go:111-116)
```go
return &Info{
    UserID:    userID,
    GatewayID: result["gwId"],
    ConnID:    result["connId"],
    Timestamp: timestamp,
}, nil
```

#### 性能特性

| 操作 | 复杂度 | 说明 |
|------|--------|------|
| `HGETALL` | O(N) | N = Hash 字段数（此处 N=3，常数时间） |
| 网络延迟 | ~1ms | 局域网 Redis |
| 总延迟 | ~1-2ms | 消息路由的关键路径 |

---

### Remove - 清理状态

**方法签名** (presence.go:120)
```go
func (m *Manager) Remove(ctx context.Context, userID string) error
```

#### 调用时机

```
用户断开连接流程：
WebSocket 关闭 → handleConnection defer → unregisterConnection → Presence.Remove()
```

调用位置：`internal/gateway/handler.go:226`
```go
func (s *Server) unregisterConnection(ctx context.Context, c *Connection) {
    if c.userID != "" {
        if err := s.presenceManager.Remove(ctx, c.userID); err != nil {
            log.Printf("[Handler] Failed to remove presence: %v", err)
        }
    }
}
```

#### 实现细节

**步骤 1: 删除 Key** (presence.go:121-123)
```go
key := presenceKeyPrefix + userID
err := m.redis.Del(ctx, key).Err()
```

**Redis 操作**：
```bash
DEL presence:alice
```

**步骤 2: 错误处理** (presence.go:124-126)
```go
if err != nil {
    return fmt.Errorf("failed to remove presence: %w", err)
}
```

#### 为什么需要主动 Remove？

**对比：主动删除 vs 依赖 TTL**

| 方案 | 断开连接后 | 消息路由 | 资源占用 |
|------|-----------|---------|---------|
| **只依赖 TTL** | 90s 后删除 | 90s 内仍会路由到旧 Gateway | 高 |
| **主动 Remove ✅** | 立即删除 | 立即返回 "用户离线" | 低 |

**场景对比**：
```
用户 alice 断开连接

方案 1（只 TTL）：
T0:  Alice 断开
T0+: Bob 发消息给 Alice → 查到 gwId=gateway-01 → 投递失败（连接已关）
T90: TTL 过期，Key 删除

方案 2（主动 Remove）：
T0:  Alice 断开 → 立即 DEL presence:alice
T0+: Bob 发消息给 Alice → 查询返回 "user offline" → 立即反馈
```

**结论**：主动删除提供更好的用户体验。

---

### IsOnline - 在线检查

**方法签名** (presence.go:132)
```go
func (m *Manager) IsOnline(ctx context.Context, userID string) (bool, error)
```

#### 实现细节

```go
key := presenceKeyPrefix + userID

exists, err := m.redis.Exists(ctx, key).Result()
if err != nil {
    return false, fmt.Errorf("failed to check presence: %w", err)
}

return exists > 0, nil
```

#### Redis EXISTS 命令

```bash
redis> EXISTS presence:alice
(integer) 1  # Key 存在

redis> EXISTS presence:bob
(integer) 0  # Key 不存在
```

#### 使用场景

虽然当前代码未直接调用，但可用于：
- 健康检查：`/api/users/:id/online`
- 好友列表：批量查询在线状态
- 管理后台：实时监控在线用户数

示例扩展：
```go
// 批量查询在线状态
func (m *Manager) BatchIsOnline(ctx context.Context, userIDs []string) (map[string]bool, error) {
    pipe := m.redis.Pipeline()
    cmds := make(map[string]*redis.IntCmd)

    for _, uid := range userIDs {
        key := presenceKeyPrefix + uid
        cmds[uid] = pipe.Exists(ctx, key)
    }

    _, err := pipe.Exec(ctx)
    if err != nil {
        return nil, err
    }

    result := make(map[string]bool)
    for uid, cmd := range cmds {
        result[uid] = cmd.Val() > 0
    }
    return result, nil
}
```

---

## CAS 机制深入分析

### 什么是 CAS？

**CAS (Compare-And-Set)**：在更新前先检查值是否符合预期，只有符合时才执行更新。

### 为什么需要 CAS？

#### 问题场景：快速重连竞态

```
时间线（无 CAS）：

T1: Alice 连接到 Gateway-01
    Register(alice, gw-01, conn-aaa, ts=100)
    启动心跳协程（每 30s 执行一次）

T2: Gateway-01 崩溃
    Alice 的连接断开
    ⚠️ 心跳协程仍在运行（延迟发送）

T3: Alice 重连到 Gateway-02
    Register(alice, gw-02, conn-bbb, ts=103)
    Redis 状态：gwId=gw-02, connId=conn-bbb, ts=103

T4: Gateway-01 的心跳协程延迟执行
    Refresh(alice, ts=101)  ❌ 不应该执行
    如果没有 CAS → 覆盖 T3 的新状态
    Redis 状态：gwId=gw-02, connId=conn-bbb, ts=101 ❌ 错误！

T5: Bob 发消息给 Alice
    查询 Redis → gwId=gw-02
    路由到 Gateway-02 → 找不到 connId=conn-bbb（因为 ts 被旧数据覆盖）
    消息投递失败 ❌
```

#### 解决方案：CAS 时间戳检查

```lua
-- Lua 脚本中的 CAS 核心逻辑
local current_ts = redis.call('HGET', key, 'ts')

if current_ts and tonumber(current_ts) > new_ts then
    return 0  -- 拒绝旧数据
end

redis.call('HSET', key, 'gwId', new_gw, 'connId', new_conn, 'ts', new_ts)
```

**有了 CAS 的时间线**：
```
T4: Gateway-01 的心跳协程延迟执行
    Refresh(alice, ts=101)
    Lua 检查：current_ts(103) > new_ts(101) → return 0
    ✅ 拒绝旧数据，保护新状态
```

### Register 的 CAS 竞态详解

#### 场景 1：正常重连（CAS 成功）

```
Initial: presence:alice → gwId=gw-01, ts=100

Event: Alice 重连到 GW-02
Register(alice, gw-02, conn-bbb, ts=105)

Lua 执行：
current_ts = 100
new_ts = 105
100 > 105? NO → 允许更新
Result: presence:alice → gwId=gw-02, ts=105 ✅
```

#### 场景 2：旧数据延迟到达（CAS 拒绝）

```
Current: presence:alice → gwId=gw-02, ts=105

Event: GW-01 的旧请求到达
Register(alice, gw-01, conn-aaa, ts=100)

Lua 执行：
current_ts = 105
new_ts = 100
105 > 100? YES → 拒绝更新
Result: presence:alice → gwId=gw-02, ts=105 ✅ 保持不变
Error: "stale update rejected for user alice"
```

#### 场景 3：并发注册（最新胜出）

```
Initial: presence:alice 不存在

并发请求：
- Thread A: Register(alice, gw-01, ts=100)
- Thread B: Register(alice, gw-02, ts=103)

可能的执行顺序：

顺序 1：A 先执行
1. A 的 Lua: current_ts=nil, new_ts=100 → 写入 ts=100
2. B 的 Lua: current_ts=100, new_ts=103 → 100>103? NO → 写入 ts=103 ✅
   Result: gwId=gw-02 (新的胜出)

顺序 2：B 先执行
1. B 的 Lua: current_ts=nil, new_ts=103 → 写入 ts=103
2. A 的 Lua: current_ts=103, new_ts=100 → 103>100? YES → 拒绝 ❌
   Result: gwId=gw-02 (新的保留)

结论：无论执行顺序如何，最终都是最新的时间戳胜出！
```

---

## Lua 脚本详解

### 为什么使用 Lua？

| 方案 | 原子性 | 网络开销 | 竞态风险 |
|------|--------|---------|---------|
| **Go 代码** | ❌ 无 | 多次 RTT | ✅ 有 |
| **Redis Lua ✅** | ✅ 有 | 1 次 RTT | ❌ 无 |

### 原子性保证

#### Go 代码实现（有竞态）

```go
// ❌ 这段代码有竞态条件
func (m *Manager) Register(ctx context.Context, userID, gwID string, ts int64) error {
    key := presenceKeyPrefix + userID

    // 步骤 1: 读取当前时间戳
    currentTS, err := m.redis.HGet(ctx, key, "ts").Int64()

    // ⚠️ 时间窗口：其他请求可能在此期间修改数据

    // 步骤 2: 比较时间戳
    if currentTS > ts {
        return errors.New("stale update")
    }

    // 步骤 3: 写入新数据
    m.redis.HSet(ctx, key, "gwId", gwID, "ts", ts)

    return nil
}
```

**竞态示例**：
```
Thread A                     Thread B
───────────────────────────────────────────────
HGET ts → 100
                             HGET ts → 100
比较：100 < 105? YES
                             比较：100 < 103? YES
HSET ts=105
                             HSET ts=103 ❌ 覆盖了 105！
```

#### Lua 脚本实现（无竞态）

```lua
-- ✅ Lua 脚本原子执行
local current_ts = redis.call('HGET', key, 'ts')
if current_ts and tonumber(current_ts) > new_ts then
    return 0
end
redis.call('HSET', key, 'gwId', new_gw, 'connId', new_conn, 'ts', new_ts)
return 1
```

**原子性保证**：
- Lua 脚本在 Redis 服务器端执行
- 执行期间 Redis 是**单线程**，不会被其他命令打断
- 整个"读取-比较-写入"过程是**原子的**

### Lua 脚本执行流程

```
┌──────────────────────────────────────────────┐
│ Go Client (Gateway-02)                       │
│                                              │
│ result := redis.Eval(ctx, script,           │
│     []string{"presence:alice"},              │
│     "gateway-02", "conn-xyz", 1703512345, 90)│
└────────────────┬─────────────────────────────┘
                 │
                 │ 序列化并发送
                 │
                 ▼
┌──────────────────────────────────────────────┐
│ Redis Server                                 │
│                                              │
│ 1. 解析 Lua 脚本                             │
│ 2. 加载到 Lua 虚拟机                         │
│ 3. 设置参数：                                │
│    KEYS[1] = "presence:alice"                │
│    ARGV[1] = "gateway-02"                    │
│    ARGV[2] = "conn-xyz"                      │
│    ARGV[3] = "1703512345"                    │
│    ARGV[4] = "90"                            │
│ 4. 🔒 执行脚本（期间阻塞其他命令）            │
│    - HGET presence:alice ts                  │
│    - 比较时间戳                              │
│    - HSET presence:alice ...                 │
│    - EXPIRE presence:alice 90                │
│ 5. 🔓 返回结果：1                            │
└────────────────┬─────────────────────────────┘
                 │
                 │ 返回执行结果
                 │
                 ▼
┌──────────────────────────────────────────────┐
│ Go Client                                    │
│                                              │
│ if result.(int64) == 0 {                     │
│     return errors.New("stale update")        │
│ }                                            │
└──────────────────────────────────────────────┘
```

### Lua 脚本性能

**Script SHA 缓存优化**（可选优化）：
```go
// 首次执行时计算脚本的 SHA1
scriptSHA := redis.ScriptLoad(ctx, script)

// 后续调用使用 SHA1，减少网络传输
result := redis.EvalSha(ctx, scriptSHA, []string{key}, args...)
```

性能对比：
| 方法 | 脚本大小 | 网络传输 |
|------|---------|---------|
| `EVAL` | ~500 字节 | 每次都传输完整脚本 |
| `EVALSHA` | 40 字节 | 只传输 SHA1 哈希 |

**本项目未优化的原因**：
- 脚本长度适中（~500 字节）
- 注册操作频率不高（每个用户只注册一次）
- 代码简洁性优先

---

## TTL 与心跳设计

### 参数关系

```
┌─────────────────────────────────────────────┐
│           时间参数设计                       │
│                                             │
│  客户端心跳间隔:  30s                        │
│  Redis TTL:       90s (3x heartbeat)        │
│  容错窗口:        60s (允许 2 次心跳丢失)     │
└─────────────────────────────────────────────┘
```

### 时间轴分析

#### 场景 1：正常心跳

```
T=0s:   Register → TTL=90s
        ┌─────────────────────────────────────────────────────┐ TTL
        │                                                     │
T=30s:  Heartbeat → TTL 重置为 90s
        └─────┬──────────────────────────────────────────────────┐
              │                                                  │
T=60s:  Heartbeat → TTL 重置为 90s
              └─────┬──────────────────────────────────────────────┐
                    │                                              │
T=90s:  Heartbeat → TTL 重置为 90s
                    └─────┬──────────────────────────────────────────┐
                          │                                          │
...     持续心跳，永不过期
```

#### 场景 2：心跳丢失 1 次（仍可恢复）

```
T=0s:   Register → TTL=90s
T=30s:  ❌ 心跳丢失（网络抖动）→ TTL 剩余 60s
T=60s:  ✅ 心跳恢复 → TTL 重置为 90s
Result: 无影响，状态正常
```

#### 场景 3：心跳丢失 2 次（仍可恢复）

```
T=0s:   Register → TTL=90s
T=30s:  ❌ 心跳丢失 → TTL 剩余 60s
T=60s:  ❌ 心跳丢失 → TTL 剩余 30s
T=90s:  ✅ 心跳恢复 → TTL 重置为 90s （在过期前 0s 救回！）
Result: 险象环生，但状态保留
```

#### 场景 4：连接真正断开

```
T=0s:   Register → TTL=90s
T=30s:  ❌ 连接断开，无心跳 → TTL 剩余 60s
T=60s:  ❌ 无心跳 → TTL 剩余 30s
T=90s:  ❌ 无心跳 → TTL=0，Key 自动删除
T=91s:  其他用户查询 → "user offline"
Result: ✅ 自动清理，无僵尸状态
```

### 为什么是 3x 而非 2x 或 4x？

| 倍数 | 容错次数 | 风险 | 清理延迟 |
|------|---------|------|---------|
| 2x (60s) | 1 次 | ⚠️ 高（网络抖动易误判） | ✅ 短（60s） |
| **3x (90s) ✅** | 2 次 | ✅ 低（合理容错） | ✅ 可接受（90s） |
| 4x (120s) | 3 次 | ✅ 极低 | ⚠️ 长（120s） |

**业界实践**：
- Kubernetes Liveness Probe: 默认 `failureThreshold=3`
- Consul Health Check: 默认超时 = 3x 检查间隔
- Etcd Lease: 推荐 TTL = 3x keepalive

---

## 竞态条件处理

### 竞态场景总结

本系统处理的 4 类竞态：

#### 1. 快速重连竞态

**场景**：用户从 GW-01 断开后立即重连到 GW-02，旧 Gateway 的心跳延迟到达。

**解决方案**：Register Lua 脚本时间戳 CAS 检查
```lua
if current_ts and tonumber(current_ts) > new_ts then
    return 0  -- 拒绝旧数据
end
```

#### 2. 并发注册竞态

**场景**：同一用户同时连接多个 Gateway（客户端 bug 或恶意行为）。

**解决方案**：时间戳最新者胜出
```
Thread A: Register(alice, gw-01, ts=100)
Thread B: Register(alice, gw-02, ts=103)
Result: 无论执行顺序，最终 gwId=gw-02 (ts 更大)
```

#### 3. 心跳与断开竞态

**场景**：用户断开连接时，心跳刷新协程仍在运行。

**时间线**：
```
T0: 用户断开 → handleConnection defer 执行 Remove()
T0.1ms: 心跳协程 goroutine 执行 Refresh()

可能的执行顺序：
顺序 1: Remove → Refresh
  - Remove 删除 Key
  - Refresh 刷新不存在的 Key（HSET 会创建新 Key！）
  - ❌ 产生僵尸状态

顺序 2: Refresh → Remove
  - Refresh 刷新 TTL
  - Remove 删除 Key
  - ✅ 最终状态正确
```

**当前实现的问题**：顺序 1 会产生僵尸状态！

**解决方案**：
```go
// ✅ 改进版 Refresh（检查 Key 是否存在）
func (m *Manager) Refresh(ctx context.Context, userID string) error {
    key := presenceKeyPrefix + userID
    timestamp := time.Now().Unix()

    // Lua 脚本：只刷新存在的 Key
    script := `
        local key = KEYS[1]
        if redis.call('EXISTS', key) == 0 then
            return 0  -- Key 不存在，拒绝刷新
        end
        redis.call('HSET', key, 'ts', ARGV[1])
        redis.call('EXPIRE', key, ARGV[2])
        return 1
    `

    return m.redis.Eval(ctx, script, []string{key}, timestamp, int(presenceTTL.Seconds())).Err()
}
```

**或更简单方案**：
```go
// ✅ 使用 Context 取消心跳协程
func (s *Server) handleConnection(conn *websocket.Conn, connID string) {
    ctx, cancel := context.WithCancel(s.ctx)
    defer cancel()  // 连接关闭时立即取消所有子协程

    go s.heartbeatChecker(ctx, c)  // 协程会监听 ctx.Done()
}
```

#### 4. TTL 自动删除竞态

**场景**：Key 即将过期时，心跳刷新和 TTL 删除并发执行。

**Redis 保证**：EXPIRE 和 DEL 是原子操作，不存在此竞态。

---

## 性能优化

### 1. Pipeline 批量操作

**使用位置**：Refresh 方法 (presence.go:81-84)

```go
pipe := m.redis.Pipeline()
pipe.HSet(ctx, key, "ts", timestamp)
pipe.Expire(ctx, key, presenceTTL)
_, err := pipe.Exec(ctx)
```

**性能对比**：
```
10,000 个用户同时心跳

Without Pipeline:
  - 20,000 次网络往返（每个用户 2 次 RTT）
  - 假设 RTT = 1ms
  - 总时间 = 20,000ms = 20 秒

With Pipeline:
  - 10,000 次网络往返（每个用户 1 次 RTT）
  - 总时间 = 10,000ms = 10 秒
  - ✅ 性能提升 50%
```

### 2. Lua 脚本减少 RTT

**Register 方法**：如果用 Go 代码实现需要 4 次 RTT
```
1. HGET presence:alice ts  (读取旧时间戳)
2. (Go 代码比较时间戳)
3. HSET presence:alice ... (写入数据)
4. EXPIRE presence:alice 90 (设置 TTL)

总计：4 次网络往返
```

**Lua 脚本**：只需 1 次 RTT
```
1. EVAL script (包含所有逻辑)

总计：1 次网络往返
```

**性能提升**：75% RTT 减少

### 3. Redis Hash 数据结构

**为什么用 Hash 而非 String？**

| 方案 | 存储格式 | 部分更新 | 内存占用 |
|------|---------|---------|---------|
| **String** | JSON 字符串 | ❌ 需重写整个 JSON | 高 |
| **Hash ✅** | 字段-值对 | ✅ 只更新单个字段 | 低 |

**示例**：
```bash
# String 方案（不灵活）
SET presence:alice '{"gwId":"gateway-02","connId":"xyz","ts":1703512345}'
# 更新 ts 需要重写整个 JSON

# Hash 方案（灵活）
HSET presence:alice gwId gateway-02
HSET presence:alice connId xyz
HSET presence:alice ts 1703512345
# 只更新 ts
HSET presence:alice ts 1703512399
```

### 4. 避免 KEYS 命令

**反面教材**（永远不要这样做）：
```go
// ❌ 极其危险的代码！
func (m *Manager) GetAllOnlineUsers(ctx context.Context) ([]string, error) {
    keys, err := m.redis.Keys(ctx, "presence:*").Result()  // 阻塞 Redis！
    return keys, err
}
```

**为什么危险？**
- `KEYS` 命令是 **O(N)**，N = Redis 所有 key 的数量
- 在生产环境会**阻塞 Redis 服务器**，导致所有请求超时
- 100 万个 key 的情况下可能阻塞数秒

**正确做法**：使用 SCAN
```go
// ✅ 安全的实现
func (m *Manager) ScanOnlineUsers(ctx context.Context) ([]string, error) {
    var cursor uint64
    var users []string

    for {
        keys, nextCursor, err := m.redis.Scan(ctx, cursor, "presence:*", 100).Result()
        if err != nil {
            return nil, err
        }

        for _, key := range keys {
            users = append(users, strings.TrimPrefix(key, presenceKeyPrefix))
        }

        cursor = nextCursor
        if cursor == 0 {
            break
        }
    }

    return users, nil
}
```

---

## 设计决策

### 1. 为什么用 Redis 而非 Etcd/Consul？

| 特性 | Redis | Etcd | Consul |
|------|-------|------|--------|
| **性能** | ✅ 极高 (100k+ ops/s) | ⚠️ 中等 (10k ops/s) | ⚠️ 中等 |
| **Lua 脚本** | ✅ 原生支持 | ❌ 无 | ❌ 无 |
| **TTL 精度** | ✅ 秒级 | ✅ 秒级 | ✅ 秒级 |
| **运维成熟度** | ✅ 极高 | ✅ 高 | ✅ 高 |
| **学习曲线** | ✅ 低 | ⚠️ 中 | ⚠️ 中 |

**结论**：Presence 场景需要高 QPS 和 Lua 原子操作，Redis 是最佳选择。

### 2. 为什么用 Hash 而非多个 Key？

**方案对比**：
```bash
# 方案 A：单个 Hash (当前实现)
HSET presence:alice gwId gateway-02
HSET presence:alice connId xyz
HSET presence:alice ts 1703512345
EXPIRE presence:alice 90

# 方案 B：多个独立 Key
SET presence:alice:gwId gateway-02 EX 90
SET presence:alice:connId xyz EX 90
SET presence:alice:ts 1703512345 EX 90
```

| 方案 | 原子性 | TTL 管理 | 内存开销 |
|------|--------|---------|---------|
| **Hash ✅** | ✅ 单次操作更新多字段 | ✅ 一个 TTL | ✅ 低（一个 Key） |
| **多 Key** | ❌ 需 Lua 保证原子 | ⚠️ 需同步 3 个 TTL | ⚠️ 高（3 个 Key） |

### 3. 为什么不存储连接对象？

**不存储的内容**：
- ❌ WebSocket 连接对象
- ❌ 用户的会话数据
- ❌ 消息历史记录

**只存储路由信息**：
- ✅ UserID
- ✅ GatewayID
- ✅ ConnectionID
- ✅ Timestamp

**原因**：
1. **序列化成本**：WebSocket 连接无法序列化
2. **网络开销**：会话数据可能很大（KB 级）
3. **状态一致性**：复杂对象难以保证一致性
4. **最小化原则**：只存储路由所需的最小信息

### 4. 为什么时间戳用 Unix 秒而非毫秒？

```go
timestamp := time.Now().Unix()      // ✅ 当前实现（秒）
// vs
timestamp := time.Now().UnixMilli() // ❌ 毫秒（更精确）
```

**秒级精度足够的理由**：
- 心跳间隔是 **30 秒**，毫秒级精度无意义
- CAS 竞态窗口在**秒级**（不同 Gateway 时钟偏差）
- Redis Hash 存储数字更紧凑

**需要毫秒的场景**：
- 高频交易系统（微秒级竞态）
- 实时游戏服务器（帧级同步）
- 本项目不需要

---

## 总结

### 核心设计亮点

1. **CAS 机制**：Lua 脚本时间戳检查，防止旧数据覆盖
2. **TTL 自动清理**：90s 过期 = 3x 心跳间隔，允许 2 次容错
3. **Pipeline 优化**：批量操作减少 RTT，性能提升 50%
4. **原子操作**：Lua 脚本保证"读取-比较-写入"的原子性
5. **最小化存储**：只存路由信息，不存连接对象

### 代码质量

| 指标 | 评分 | 说明 |
|------|------|------|
| **正确性** | ⭐⭐⭐⭐⭐ | CAS 防竞态，TTL 防泄漏 |
| **性能** | ⭐⭐⭐⭐⭐ | Pipeline + Lua，极致优化 |
| **可维护性** | ⭐⭐⭐⭐ | 代码简洁，注释清晰 |
| **可测试性** | ⭐⭐⭐⭐ | 依赖注入，易 mock |
| **可扩展性** | ⭐⭐⭐⭐⭐ | Redis 水平扩展 |

### 改进建议

1. **心跳竞态修复** (presence.go:76)
   ```go
   // 当前 Refresh 未检查 Key 存在性，可能产生僵尸状态
   // 建议添加 EXISTS 检查
   ```

2. **EVALSHA 优化** (presence.go:61)
   ```go
   // 可缓存 Lua 脚本 SHA，减少网络传输
   // 适用于高 QPS 场景
   ```

3. **指标暴露**
   ```go
   // 建议添加 Prometheus 指标
   // - presence_register_total
   // - presence_cas_rejected_total
   // - presence_refresh_duration_seconds
   ```

4. **批量查询**
   ```go
   // 添加 BatchGet 方法
   // 支持一次查询多个用户的 Presence
   ```

---

**相关文档**：
- [Router 实现详解](./01-router实现详解.md) - 消息路由如何使用 Presence
- [Handler 实现详解](./02-handler实现详解.md) - 何时调用 Presence 方法
- [Connection 实现详解](./04-connection实现详解.md) - 本地连接管理
- [架构总览](./README.md) - 系统整体设计
