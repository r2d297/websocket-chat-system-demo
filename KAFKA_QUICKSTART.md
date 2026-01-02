# Kafka Router 快速启动指南 / Kafka Router Quick Start Guide

> **5 分钟内启动基于 Kafka 的分布式 WebSocket Gateway**
> **Launch Kafka-based distributed WebSocket Gateway in 5 minutes**

---

## 前置要求 / Prerequisites

```bash
✅ Docker & Docker Compose 已安装 / installed
✅ Go 1.21+ 已安装 / installed
✅ 至少 4GB 可用内存 / At least 4GB available RAM
```

---

## 步骤 1: 启动 Kafka 集群 / Step 1: Start Kafka Cluster

```bash
# 启动 Kafka、ZooKeeper 和 Redis / Start Kafka, ZooKeeper and Redis
docker-compose -f docker-compose-kafka.yml up -d

# 验证服务状态 / Verify services status
docker-compose -f docker-compose-kafka.yml ps
```

**预期输出 / Expected Output:**
```
NAME                    STATUS
websocket-kafka         Up (healthy)
websocket-kafka-ui      Up
websocket-redis         Up (healthy)
websocket-zookeeper     Up
```

---

## 步骤 2: 初始化 Kafka Topics / Step 2: Initialize Kafka Topics

```bash
# 运行初始化脚本 / Run setup script
./scripts/setup-kafka.sh
```

**脚本会创建 / Script creates:**
- ✅ `gateway-gateway-01` (3 partitions)
- ✅ `gateway-gateway-02` (3 partitions)
- ✅ `gateway-gateway-03` (3 partitions)
- ✅ `gateway-broadcast` (10 partitions)

---

## 步骤 3: 编译 Kafka Gateway / Step 3: Build Kafka Gateway

```bash
# 编译支持 Kafka 的 Gateway / Build Kafka-enabled Gateway
CGO_ENABLED=0 go build -o bin/gateway-kafka cmd/gateway-kafka/main.go

# 验证编译成功 / Verify build
./bin/gateway-kafka -h
```

---

## 步骤 4: 启动多个 Gateway / Step 4: Start Multiple Gateways

**Terminal 1 - Gateway-01:**
```bash
./bin/gateway-kafka \
  -id gateway-01 \
  -port 8080 \
  -redis localhost:6379 \
  -kafka localhost:9092
```

**Terminal 2 - Gateway-02:**
```bash
./bin/gateway-kafka \
  -id gateway-02 \
  -port 8081 \
  -redis localhost:6379 \
  -kafka localhost:9092
```

**Terminal 3 - Gateway-03 (可选) / Optional:**
```bash
./bin/gateway-kafka \
  -id gateway-03 \
  -port 8082 \
  -redis localhost:6379 \
  -kafka localhost:9092
```

---

## 步骤 5: 测试跨网关消息 / Step 5: Test Cross-Gateway Messaging

**Terminal 4 - 客户端 Alice (连接 Gateway-01):**
```bash
./bin/client -gateway ws://localhost:8080/ws -user alice
```

**Terminal 5 - 客户端 Bob (连接 Gateway-02):**
```bash
./bin/client -gateway ws://localhost:8081/ws -user bob
```

**在 Alice 的终端输入 / In Alice's terminal, type:**
```
send bob Hello from Alice via Kafka!
```

**在 Bob 的终端应该看到 / Bob's terminal should show:**
```
📩 Message from alice: Hello from Alice via Kafka!
```

✅ **成功！消息通过 Kafka 跨网关传递！**
✅ **Success! Message routed across gateways via Kafka!**

---

## 可视化监控 / Visual Monitoring

### Kafka UI
```bash
打开浏览器 / Open browser:
http://localhost:8090

查看内容 / View:
✅ Topics 列表 / Topics list
✅ 消息流量 / Message throughput
✅ Consumer Groups 状态 / Consumer groups status
✅ 实时消息 / Live messages
```

### Gateway 统计 / Gateway Statistics
```bash
# Gateway-01 统计 / Statistics
curl http://localhost:8080/stats

# Gateway-02 统计 / Statistics
curl http://localhost:8081/stats
```

---

## 性能测试 / Performance Testing

### 压力测试 / Load Test

```bash
# 创建 100 个客户端连接 / Create 100 client connections
for i in {1..100}; do
  ./bin/client -gateway ws://localhost:8080/ws -user user-$i &
done

# 观察 Kafka UI 中的消息吞吐量 / Observe message throughput in Kafka UI
```

### 查看 Kafka 性能指标 / View Kafka Performance Metrics

```bash
# 查看 topic 详情 / View topic details
docker exec websocket-kafka kafka-topics \
  --describe \
  --bootstrap-server localhost:9092 \
  --topic gateway-gateway-01

# 查看消费者组延迟 / View consumer group lag
docker exec websocket-kafka kafka-consumer-groups \
  --describe \
  --bootstrap-server localhost:9092 \
  --group websocket-gateway
```

---

## 故障测试 / Failover Testing

### 测试 Gateway 崩溃恢复 / Test Gateway Crash Recovery

```bash
# 1. 杀掉 Gateway-01 / Kill Gateway-01
pkill -f "gateway-kafka.*gateway-01"

# 2. Alice 的连接会断开 / Alice's connection will drop
# 3. Alice 重连到 Gateway-02 / Alice reconnects to Gateway-02
./bin/client -gateway ws://localhost:8081/ws -user alice

# 4. Bob 仍然可以给 Alice 发消息 / Bob can still send messages to Alice
# 在 Bob 的终端 / In Bob's terminal:
send alice You're back!
```

**关键观察 / Key Observations:**
- ✅ Kafka 中的消息没有丢失 / Messages in Kafka not lost
- ✅ Consumer Group 自动重新平衡 / Consumer group auto-rebalanced
- ✅ 其他 Gateway 不受影响 / Other gateways unaffected

---

## Kafka vs Redis 性能对比 / Performance Comparison

### 延迟测试 / Latency Test

**Redis Pub/Sub:**
```bash
# 启动 Redis 版本 / Start Redis version
./bin/gateway -id gateway-01 -port 8080

# 测试延迟 (通常 1-2ms)
# Test latency (typically 1-2ms)
```

**Kafka:**
```bash
# 启动 Kafka 版本 / Start Kafka version
./bin/gateway-kafka -id gateway-01 -port 8080 -kafka localhost:9092

# 测试延迟 (通常 5-10ms)
# Test latency (typically 5-10ms)
```

### 吞吐量测试 / Throughput Test

```bash
# 使用 wrk 进行压测 / Load test with wrk
# 需要先安装 wrk: brew install wrk

# 测试 WebSocket 升级性能 / Test WebSocket upgrade performance
wrk -t 10 -c 100 -d 30s http://localhost:8080/ws
```

---

## 常见问题 / Troubleshooting

### Q1: Kafka 启动失败 / Kafka fails to start

**问题 / Problem:**
```
ERROR Error while creating ephemeral at /brokers/ids/1
```

**解决方案 / Solution:**
```bash
# 清理 Kafka 数据 / Clean Kafka data
docker-compose -f docker-compose-kafka.yml down -v
docker-compose -f docker-compose-kafka.yml up -d
./scripts/setup-kafka.sh
```

---

### Q2: Consumer Lag 过高 / High Consumer Lag

**问题 / Problem:**
```
Consumer group lag > 10000 messages
```

**解决方案 / Solution:**
```bash
# 1. 检查 Gateway 是否正常运行 / Check if Gateway is running
ps aux | grep gateway-kafka

# 2. 增加 consumer 线程数 (修改代码) / Increase consumer threads (modify code)
# 3. 添加更多 Gateway 实例 / Add more Gateway instances
# 4. 增加 topic partitions / Increase topic partitions
docker exec websocket-kafka kafka-topics \
  --alter \
  --bootstrap-server localhost:9092 \
  --topic gateway-gateway-01 \
  --partitions 6
```

---

### Q3: 消息重复消费 / Duplicate Message Consumption

**问题 / Problem:**
客户端收到重复消息 / Client receives duplicate messages

**原因 / Reason:**
Gateway 重启后从旧的 offset 开始消费 / Gateway restarted and consumed from old offset

**解决方案 / Solution:**
```bash
# 重置 consumer group offset 到最新 / Reset consumer group offset to latest
docker exec websocket-kafka kafka-consumer-groups \
  --bootstrap-server localhost:9092 \
  --group websocket-gateway \
  --reset-offsets \
  --to-latest \
  --all-topics \
  --execute
```

---

### Q4: Kafka UI 无法访问 / Cannot access Kafka UI

**问题 / Problem:**
```
http://localhost:8090 无法打开 / cannot open
```

**解决方案 / Solution:**
```bash
# 检查容器状态 / Check container status
docker logs websocket-kafka-ui

# 重启 Kafka UI / Restart Kafka UI
docker restart websocket-kafka-ui
```

---

## 生产环境部署建议 / Production Deployment Recommendations

### 1. Kafka 集群配置 / Kafka Cluster Configuration

```yaml
# 生产环境至少 3 个 Broker / At least 3 brokers in production
# 每个 topic 至少 2 个副本 / At least 2 replicas per topic
# 使用 SSD 存储 / Use SSD storage

kafka:
  brokers: 3
  replication-factor: 3
  min-insync-replicas: 2
  storage:
    type: ssd
    size: 500GB per broker
```

### 2. 监控告警 / Monitoring & Alerting

```bash
# 使用 Prometheus + Grafana / Use Prometheus + Grafana
# 关键指标 / Key metrics:
- kafka_server_brokertopicmetrics_messagesin_total
- kafka_server_brokertopicmetrics_bytesin_total
- kafka_consumergroup_lag
- kafka_server_replicamanager_underreplicatedpartitions
```

### 3. 安全配置 / Security Configuration

```bash
# 启用 SASL/SCRAM 认证 / Enable SASL/SCRAM authentication
# 启用 SSL/TLS 加密 / Enable SSL/TLS encryption
# 配置 ACL 权限控制 / Configure ACL permissions
```

---

## 下一步 / Next Steps

✅ **阅读详细对比文档 / Read detailed comparison:**
- [Kafka vs Redis 完整对比](./docs/KAFKA_VS_REDIS.md)

✅ **查看架构文档 / View architecture docs:**
- [Router 实现详解](./docs/01-router实现详解.md)
- [系统架构总览](./README.md)

✅ **探索高级特性 / Explore advanced features:**
- 混合架构（Region 内 Redis + 跨 Region Kafka）
- 消息持久化与重放
- 多数据中心部署

---

## 资源链接 / Resource Links

- 📚 [Apache Kafka 官方文档](https://kafka.apache.org/documentation/)
- 📚 [Confluent Kafka 最佳实践](https://docs.confluent.io/platform/current/kafka/deployment.html)
- 📚 [Sarama Go Client](https://github.com/IBM/sarama)
- 📚 [本项目 GitHub](https://github.com/your-repo/websocket-demo)

---

**Happy Scaling! 🚀**
