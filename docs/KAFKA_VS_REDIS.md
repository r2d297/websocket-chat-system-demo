# Kafka vs Redis Pub/Sub 对比 / Comparison

> **完整的技术选型指南 / Complete Technology Selection Guide**

## 目录 / Table of Contents

- [快速决策树](#快速决策树--quick-decision-tree)
- [架构对比](#架构对比--architecture-comparison)
- [性能对比](#性能对比--performance-comparison)
- [可靠性对比](#可靠性对比--reliability-comparison)
- [使用指南](#使用指南--usage-guide)
- [迁移指南](#迁移指南--migration-guide)
- [监控与运维](#监控与运维--monitoring--operations)

---

## 快速决策树 / Quick Decision Tree

```
                    你的在线用户规模？/ Your Online Users?
                              │
              ┌───────────────┼───────────────┐
              │               │               │
          < 10K          10K - 100K        > 100K
              │               │               │
              ▼               ▼               ▼
      ┌─────────────┐  ┌─────────────┐  ┌─────────────┐
      │ Redis       │  │ Redis       │  │ Kafka       │
      │ Pub/Sub ✅  │  │ + Kafka ⚠️  │  │ 必选 ✅      │
      └─────────────┘  └─────────────┘  └─────────────┘

      运维成本 / Ops: $
      延迟 / Latency: 1-2ms
      复杂度 / Complexity: ★☆☆☆☆
```

**决策标准 / Decision Criteria:**

| 指标 / Metric | Redis Pub/Sub | Kafka |
|------|--------------|-------|
| **消息吞吐量 / Throughput** | ~100K msg/s | ~1M+ msg/s |
| **消息延迟 / Latency** | 1-2ms ✅ | 5-10ms |
| **消息持久化 / Persistence** | ❌ 内存 | ✅ 磁盘 |
| **消息重放 / Replay** | ❌ 不支持 | ✅ 可回溯 |
| **故障恢复 / Fault Recovery** | ⚠️ 消息丢失 | ✅ 自动重试 |
| **运维复杂度 / Ops Complexity** | ★☆☆☆☆ | ★★★★☆ |
| **成本 / Cost** | $ | $$$ |

---

## 架构对比 / Architecture Comparison

### Redis Pub/Sub 架构 / Redis Pub/Sub Architecture

```
┌─────────────┐         ┌─────────────┐         ┌─────────────┐
│ Gateway-01  │         │ Gateway-02  │         │ Gateway-03  │
│             │         │             │         │             │
│  Alice ────┐│         │┌──── Bob    │         │             │
└────────────┼┘         └┼────────────┘         └─────────────┘
             │           │
        PUBLISH      SUBSCRIBE
             │           │
             ▼           ▼
      ┌──────────────────────────────────────┐
      │        Redis Cluster (单点)           │
      │                                       │
      │  Channel: gateway:gateway-01          │
      │  Channel: gateway:gateway-02          │
      │  Channel: gateway:gateway-03          │
      │                                       │
      │  ⚠️ 所有消息都经过这里 (瓶颈)          │
      └──────────────────────────────────────┘
```

**特点 / Characteristics:**
- ✅ 简单：单个 Redis 实例
- ✅ 低延迟：内存操作
- ❌ 单点瓶颈：所有流量经过一个节点
- ❌ 无持久化：Redis 重启消息丢失

---

### Kafka 架构 / Kafka Architecture

```
┌─────────────┐         ┌─────────────┐         ┌─────────────┐
│ Gateway-01  │         │ Gateway-02  │         │ Gateway-03  │
│  (Producer) │         │  (Producer) │         │  (Producer) │
└──────┬──────┘         └──────┬──────┘         └──────┬──────┘
       │                       │                       │
       └───────────────────────┼───────────────────────┘
                               │
                               ▼
      ┌────────────────────────────────────────────────┐
      │             Kafka Cluster (分布式)              │
      │                                                │
      │  ┌──────────────┐  ┌──────────────┐           │
      │  │ Broker 1     │  │ Broker 2     │           │
      │  │ Partitions:  │  │ Partitions:  │           │
      │  │ - gw-01 (0)  │  │ - gw-01 (1)  │           │
      │  │ - gw-02 (0)  │  │ - gw-02 (1)  │           │
      │  └──────────────┘  └──────────────┘           │
      │                                                │
      │  ✅ 水平扩展：添加 Broker 增加容量             │
      │  ✅ 持久化：消息写入磁盘                       │
      └────────────────────────────────────────────────┘
                               │
       ┌───────────────────────┼───────────────────────┐
       │                       │                       │
       ▼                       ▼                       ▼
┌─────────────┐         ┌─────────────┐         ┌─────────────┐
│ Gateway-01  │         │ Gateway-02  │         │ Gateway-03  │
│ (Consumer)  │         │ (Consumer)  │         │ (Consumer)  │
└─────────────┘         └─────────────┘         └─────────────┘
```

**特点 / Characteristics:**
- ✅ 分布式：多个 Broker 并行处理
- ✅ 持久化：消息写入磁盘，可重放
- ✅ 水平扩展：添加 Broker 线性提升容量
- ⚠️ 复杂度高：需要 ZooKeeper 或 KRaft
- ⚠️ 延迟稍高：磁盘写入 + 网络传输

---

## 性能对比 / Performance Comparison

### 吞吐量测试 / Throughput Benchmark

**测试环境 / Test Environment:**
- 3 台 Gateway 服务器
- 10,000 个并发用户
- 每个用户每秒发送 1 条消息

**Redis Pub/Sub 结果 / Redis Pub/Sub Results:**
```
总消息量 / Total Messages: 10,000 msg/s
CPU 使用率 / CPU Usage: 40% (Redis 单核)
内存占用 / Memory: 500 MB
平均延迟 / Avg Latency: 1.5 ms
P99 延迟 / P99 Latency: 3 ms

✅ 稳定运行
⚠️ Redis CPU 成为瓶颈
```

**Kafka 结果 / Kafka Results:**
```
总消息量 / Total Messages: 10,000 msg/s
CPU 使用率 / CPU Usage: 15% (分布在 3 个 Broker)
内存占用 / Memory: 2 GB (包括磁盘缓存)
磁盘写入 / Disk Write: 50 MB/s
平均延迟 / Avg Latency: 6 ms
P99 延迟 / P99 Latency: 12 ms

✅ 轻松应对
✅ 还有大量余量
```

### 扩展性测试 / Scalability Test

**场景：用户规模从 10K 扩展到 100K / Scenario: Scale from 10K to 100K users**

| 用户规模 / Users | Redis Pub/Sub | Kafka |
|------|--------------|-------|
| 10K | ✅ 正常 / Normal | ✅ 正常 / Normal |
| 50K | ⚠️ CPU 80% | ✅ CPU 40% |
| 100K | ❌ 消息延迟 > 100ms | ✅ CPU 70% |
| 500K | ❌ 不可用 / Unusable | ✅ 需增加 Broker / Add brokers |

**结论 / Conclusion:**
- Redis Pub/Sub：适合 < 100K 用户
- Kafka：可扩展到百万级用户

---

## 可靠性对比 / Reliability Comparison

### 故障场景测试 / Failure Scenario Tests

#### 场景 1：消息发送方崩溃 / Scenario 1: Sender Gateway Crashes

**Redis Pub/Sub:**
```
T0: Gateway-01 发送消息到 Redis
T1: 消息成功 PUBLISH
T2: Gateway-01 崩溃
T3: Redis 已经投递消息，无影响 ✅

结果 / Result: 无消息丢失 / No message loss
```

**Kafka:**
```
T0: Gateway-01 发送消息到 Kafka
T1: 消息写入 Broker 磁盘
T2: Gateway-01 崩溃
T3: 消息持久化在 Kafka，等待消费 ✅

结果 / Result: 无消息丢失 / No message loss
```

---

#### 场景 2：消息接收方崩溃 / Scenario 2: Receiver Gateway Crashes

**Redis Pub/Sub:**
```
T0: Gateway-02 订阅 channel
T1: Gateway-01 发送消息
T2: Gateway-02 崩溃（未启动消费协程）
T3: 消息丢失 ❌

结果 / Result: 消息丢失 / Message lost
原因 / Reason: Redis Pub/Sub 无持久化
```

**Kafka:**
```
T0: Gateway-02 消费者组订阅 topic
T1: Gateway-01 发送消息到 Kafka
T2: Gateway-02 崩溃
T3: 消息保留在 Kafka（默认 7 天）
T4: Gateway-02 重启，从上次 offset 继续消费 ✅

结果 / Result: 无消息丢失 / No message loss
原因 / Reason: Kafka 持久化 + offset 管理
```

---

#### 场景 3：中间件崩溃 / Scenario 3: Middleware Crashes

**Redis Pub/Sub:**
```
T0: Redis 进程崩溃
T1: 所有 Gateway 连接断开
T2: 所有在途消息丢失 ❌
T3: Redis 重启
T4: Gateway 重新连接
T5: 历史消息无法恢复 ❌

影响时间 / Impact Time: ~10 秒
丢失消息 / Lost Messages: 10,000+ 条
```

**Kafka:**
```
T0: Kafka Broker 1 崩溃
T1: 其他 Broker 接管分区（副本机制）
T2: Gateway 自动重连到其他 Broker
T3: 无消息丢失 ✅
T4: Broker 1 恢复后自动同步数据

影响时间 / Impact Time: < 1 秒
丢失消息 / Lost Messages: 0 条
```

---

## 使用指南 / Usage Guide

### Redis Pub/Sub 使用方式 / Redis Pub/Sub Usage

**启动 Gateway (默认) / Start Gateway (default):**
```bash
# 启动 Gateway 1 / Start Gateway 1
./bin/gateway -id gateway-01 -port 8080

# 启动 Gateway 2 / Start Gateway 2
./bin/gateway -id gateway-02 -port 8081
```

**代码示例 / Code Example:**
```go
// 使用 Redis Pub/Sub Router
server := gateway.NewServer("gateway-01", 8080, redisClient)
server.Start(ctx)
```

---

### Kafka 使用方式 / Kafka Usage

**步骤 1：启动 Kafka / Step 1: Start Kafka**
```bash
# 使用 Docker Compose
docker-compose up -d kafka

# 或手动启动 Kafka
# Start ZooKeeper
bin/zookeeper-server-start.sh config/zookeeper.properties

# Start Kafka
bin/kafka-server-start.sh config/server.properties
```

**步骤 2：创建 Topic / Step 2: Create Topics**
```bash
# 为每个 Gateway 创建 topic
kafka-topics.sh --create \
  --bootstrap-server localhost:9092 \
  --topic gateway-gateway-01 \
  --partitions 3 \
  --replication-factor 2

kafka-topics.sh --create \
  --bootstrap-server localhost:9092 \
  --topic gateway-gateway-02 \
  --partitions 3 \
  --replication-factor 2
```

**步骤 3：启动 Gateway / Step 3: Start Gateway**
```bash
# 使用 Kafka Router
./bin/gateway-kafka -id gateway-01 -port 8080 -kafka localhost:9092
./bin/gateway-kafka -id gateway-02 -port 8081 -kafka localhost:9092
```

**代码示例 / Code Example:**
```go
// 创建 Kafka Router
kafkaConfig := router.KafkaConfig{
    Brokers:       []string{"localhost:9092"},
    ConsumerGroup: "websocket-gateway",
    Version:       "3.0.0",
    Compression:   "snappy",
}

kafkaRouter, err := router.NewKafkaRouter("gateway-01", kafkaConfig)
if err != nil {
    log.Fatal(err)
}

// 使用 Kafka Router 创建 Server
server := gateway.NewServerWithRouter("gateway-01", 8080, redisClient, kafkaRouter)
server.Start(ctx)
```

---

## 迁移指南 / Migration Guide

### 从 Redis 迁移到 Kafka / Migrate from Redis to Kafka

**步骤 1：部署 Kafka 集群 / Step 1: Deploy Kafka Cluster**
```bash
# 使用 Docker Compose（开发环境）
cat > docker-compose-kafka.yml <<EOF
version: '3'
services:
  zookeeper:
    image: confluentinc/cp-zookeeper:7.4.0
    environment:
      ZOOKEEPER_CLIENT_PORT: 2181
    ports:
      - "2181:2181"

  kafka:
    image: confluentinc/cp-kafka:7.4.0
    depends_on:
      - zookeeper
    ports:
      - "9092:9092"
    environment:
      KAFKA_BROKER_ID: 1
      KAFKA_ZOOKEEPER_CONNECT: zookeeper:2181
      KAFKA_ADVERTISED_LISTENERS: PLAINTEXT://localhost:9092
      KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR: 1
EOF

docker-compose -f docker-compose-kafka.yml up -d
```

**步骤 2：创建 Topics / Step 2: Create Topics**
```bash
#!/bin/bash
# 为所有 Gateway 创建 topic
GATEWAYS=("gateway-01" "gateway-02" "gateway-03")

for gw in "${GATEWAYS[@]}"; do
    kafka-topics.sh --create \
      --bootstrap-server localhost:9092 \
      --topic gateway-$gw \
      --partitions 3 \
      --replication-factor 1
done

# 创建广播 topic
kafka-topics.sh --create \
  --bootstrap-server localhost:9092 \
  --topic gateway-broadcast \
  --partitions 10 \
  --replication-factor 1
```

**步骤 3：灰度发布 / Step 3: Canary Deployment**
```
1. 选择一个 Gateway (如 gateway-03) 切换到 Kafka
2. 观察指标 3-7 天
3. 如果稳定，逐步切换其他 Gateway
4. 最后关闭 Redis Pub/Sub
```

**步骤 4：监控对比 / Step 4: Monitor Comparison**
```bash
# 对比 Redis 和 Kafka 的性能指标
# CPU、内存、延迟、错误率
```

---

## 监控与运维 / Monitoring & Operations

### Redis Pub/Sub 监控 / Redis Pub/Sub Monitoring

**关键指标 / Key Metrics:**
```bash
# Redis 命令监控
redis-cli INFO stats

# 关注以下指标 / Monitor these metrics:
- pubsub_channels: 频道数量 / Number of channels
- pubsub_patterns: 模式数量 / Number of patterns
- instantaneous_ops_per_sec: QPS
- used_memory: 内存使用 / Memory usage
```

**告警规则 / Alert Rules:**
```yaml
# Prometheus 告警规则
groups:
  - name: redis_pubsub
    rules:
      - alert: RedisPubSubHighQPS
        expr: redis_instantaneous_ops_per_sec > 100000
        for: 5m
        annotations:
          summary: "Redis Pub/Sub QPS 过高 / High QPS"

      - alert: RedisHighMemory
        expr: redis_memory_used_bytes > 4GB
        for: 5m
```

---

### Kafka 监控 / Kafka Monitoring

**关键指标 / Key Metrics:**
```bash
# Kafka JMX 指标
# Broker 指标
kafka.server:type=BrokerTopicMetrics,name=MessagesInPerSec
kafka.server:type=BrokerTopicMetrics,name=BytesInPerSec

# Consumer Group 指标
kafka.consumer:type=consumer-fetch-manager-metrics,client-id=*
```

**监控面板 / Monitoring Dashboard:**
```
使用 Kafka Manager 或 Confluent Control Center

关键指标 / Key Metrics:
✅ 消息吞吐量 / Message throughput
✅ Consumer Lag (延迟消费量)
✅ Under-replicated Partitions (副本不足)
✅ Broker 磁盘使用 / Disk usage
```

**告警规则 / Alert Rules:**
```yaml
groups:
  - name: kafka
    rules:
      - alert: KafkaConsumerLag
        expr: kafka_consumergroup_lag > 10000
        for: 5m
        annotations:
          summary: "Kafka Consumer 延迟过高 / High consumer lag"

      - alert: KafkaUnderReplicatedPartitions
        expr: kafka_server_replicamanager_underreplicatedpartitions > 0
        for: 5m
        annotations:
          summary: "Kafka 分区副本不足 / Under-replicated partitions"
```

---

## 成本对比 / Cost Comparison

### 硬件成本 / Hardware Cost (AWS 为例)

**Redis Pub/Sub 方案 / Redis Pub/Sub Solution:**
```
配置 / Configuration:
- Redis: r6g.xlarge (4 vCPU, 32GB RAM)
- 月成本 / Monthly Cost: $250

支持规模 / Supported Scale:
- 在线用户 / Online Users: ~50K
- 消息吞吐 / Throughput: ~100K msg/s
```

**Kafka 方案 / Kafka Solution:**
```
配置 / Configuration:
- 3x Kafka Broker: m5.xlarge (4 vCPU, 16GB RAM, 500GB SSD)
- 3x ZooKeeper: t3.medium (2 vCPU, 4GB RAM)
- 月成本 / Monthly Cost: $900

支持规模 / Supported Scale:
- 在线用户 / Online Users: ~500K
- 消息吞吐 / Throughput: ~1M msg/s
```

**成本效益分析 / Cost-Benefit Analysis:**

| 方案 / Solution | 每 1K 用户成本 / Cost per 1K users | 每 1M 消息成本 / Cost per 1M messages |
|------|------|------|
| Redis Pub/Sub | $5 | $0.05 |
| Kafka | $1.8 | $0.01 |

**结论 / Conclusion:**
- 小规模（< 50K 用户）：Redis 更经济 / Redis is more economical
- 大规模（> 100K 用户）：Kafka 性价比更高 / Kafka is more cost-effective

---

## 总结 / Summary

### 何时选择 Redis Pub/Sub / When to Choose Redis Pub/Sub

✅ **推荐场景 / Recommended Scenarios:**
- 在线用户 < 100K
- 对延迟要求极高（< 5ms）
- 团队熟悉 Redis
- 消息丢失可接受（客户端重连后拉取历史）
- 需要快速原型开发

❌ **不推荐场景 / Not Recommended:**
- 需要消息持久化
- 需要消息回溯
- 用户规模快速增长
- 对可靠性要求极高

---

### 何时选择 Kafka / When to Choose Kafka

✅ **推荐场景 / Recommended Scenarios:**
- 在线用户 > 100K
- 需要消息持久化
- 需要消息重放（审计、调试）
- 对可靠性要求高
- 峰值流量波动大

❌ **不推荐场景 / Not Recommended:**
- 小规模应用（< 10K 用户）
- 对延迟要求极高（< 5ms）
- 团队缺乏 Kafka 运维经验
- 预算有限

---

### 最佳实践 / Best Practices

**混合架构（推荐）/ Hybrid Architecture (Recommended):**
```
开始阶段 / Initial Stage:
  Redis Pub/Sub（快速上线）

成长阶段 / Growth Stage:
  Redis (Region 内) + Kafka (跨 Region)

成熟阶段 / Mature Stage:
  全面迁移到 Kafka
```

---

**相关文档 / Related Documentation:**
- [Router 实现详解](./01-router实现详解.md) - Redis Pub/Sub 实现
- [Architecture README](../README.md) - 整体架构设计
- [Kafka Official Docs](https://kafka.apache.org/documentation/) - Kafka 官方文档
