#!/bin/bash

# Kafka 环境初始化脚本 / Kafka Environment Setup Script
# 用途：创建必要的 topics / Purpose: Create necessary topics

set -e

echo "🚀 Setting up Kafka for WebSocket Gateway..."

# 等待 Kafka 就绪 / Wait for Kafka to be ready
echo "⏳ Waiting for Kafka to be ready..."
sleep 10

# Kafka broker 地址 / Kafka broker address
KAFKA_BROKER="localhost:9092"

# 检查 Kafka 是否可用 / Check if Kafka is available
if ! docker exec websocket-kafka kafka-broker-api-versions --bootstrap-server $KAFKA_BROKER > /dev/null 2>&1; then
    echo "❌ Kafka is not ready. Please run: docker-compose -f docker-compose-kafka.yml up -d"
    exit 1
fi

echo "✅ Kafka is ready!"

# 定义 Gateway IDs / Define Gateway IDs
GATEWAYS=("gateway-01" "gateway-02" "gateway-03")

# 为每个 Gateway 创建 topic / Create topic for each Gateway
for gw in "${GATEWAYS[@]}"; do
    TOPIC="gateway-$gw"

    echo "📝 Creating topic: $TOPIC"

    docker exec websocket-kafka kafka-topics --create \
        --bootstrap-server $KAFKA_BROKER \
        --topic $TOPIC \
        --partitions 3 \
        --replication-factor 1 \
        --if-not-exists \
        --config retention.ms=604800000 \
        --config segment.ms=86400000 \
        --config compression.type=snappy

    echo "✅ Topic $TOPIC created successfully"
done

# 创建广播 topic / Create broadcast topic
echo "📝 Creating broadcast topic: gateway-broadcast"
docker exec websocket-kafka kafka-topics --create \
    --bootstrap-server $KAFKA_BROKER \
    --topic gateway-broadcast \
    --partitions 10 \
    --replication-factor 1 \
    --if-not-exists \
    --config retention.ms=86400000 \
    --config compression.type=snappy

echo "✅ Broadcast topic created successfully"

# 列出所有 topics / List all topics
echo ""
echo "📋 All topics:"
docker exec websocket-kafka kafka-topics --list \
    --bootstrap-server $KAFKA_BROKER

# 显示 topic 详情 / Show topic details
echo ""
echo "📊 Topic details:"
for gw in "${GATEWAYS[@]}"; do
    TOPIC="gateway-$gw"
    docker exec websocket-kafka kafka-topics --describe \
        --bootstrap-server $KAFKA_BROKER \
        --topic $TOPIC
done

echo ""
echo "🎉 Kafka setup completed!"
echo ""
echo "📌 Next steps:"
echo "1. Build Kafka-enabled Gateway:"
echo "   CGO_ENABLED=0 go build -o bin/gateway-kafka cmd/gateway-kafka/main.go"
echo ""
echo "2. Start Gateway with Kafka:"
echo "   ./bin/gateway-kafka -id gateway-01 -port 8080 -kafka localhost:9092"
echo ""
echo "3. Access Kafka UI:"
echo "   http://localhost:8090"
echo ""
