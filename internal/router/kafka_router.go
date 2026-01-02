package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/IBM/sarama"
)

// KafkaRouter implements message routing using Kafka
// Kafka Router 使用 Kafka 实现消息路由
type KafkaRouter struct {
	producer  sarama.SyncProducer      // Kafka 生产者 / Kafka producer
	consumer  sarama.ConsumerGroup     // Kafka 消费者组 / Kafka consumer group
	gatewayID string                   // 本 Gateway 的唯一 ID / This Gateway's unique ID
	handler   MessageHandler           // 本地消息处理回调 / Local message handler callback
	ctx       context.Context          // Context for lifecycle management
	cancel    context.CancelFunc       // Cancel function
	wg        sync.WaitGroup           // Wait group for goroutines
	brokers   []string                 // Kafka broker 地址列表 / Kafka broker addresses
}

// KafkaConfig holds Kafka-specific configuration
// KafkaConfig 保存 Kafka 特定配置
type KafkaConfig struct {
	Brokers       []string      // Kafka broker 地址 / Kafka broker addresses (e.g., ["localhost:9092"])
	ConsumerGroup string        // 消费者组 ID / Consumer group ID
	Version       string        // Kafka 版本 / Kafka version (e.g., "3.0.0")
	ReturnErrors  bool          // 是否返回错误 / Whether to return errors
	Compression   string        // 压缩算法 / Compression codec ("none", "gzip", "snappy", "lz4", "zstd")
}

// NewKafkaRouter creates a new Kafka-based router
// 创建一个新的基于 Kafka 的路由器
func NewKafkaRouter(gatewayID string, config KafkaConfig) (*KafkaRouter, error) {
	// 解析 Kafka 版本 / Parse Kafka version
	version, err := sarama.ParseKafkaVersion(config.Version)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Kafka version: %w", err)
	}

	// 配置生产者 / Configure producer
	producerConfig := sarama.NewConfig()
	producerConfig.Version = version
	producerConfig.Producer.RequiredAcks = sarama.WaitForAll // 等待所有副本确认 / Wait for all replicas
	producerConfig.Producer.Retry.Max = 3                     // 重试 3 次 / Retry 3 times
	producerConfig.Producer.Return.Successes = true
	producerConfig.Producer.Return.Errors = config.ReturnErrors
	producerConfig.Producer.Partitioner = sarama.NewHashPartitioner // 使用 hash 分区保证顺序 / Use hash partitioner for ordering

	// 设置压缩算法 / Set compression codec
	switch config.Compression {
	case "gzip":
		producerConfig.Producer.Compression = sarama.CompressionGZIP
	case "snappy":
		producerConfig.Producer.Compression = sarama.CompressionSnappy
	case "lz4":
		producerConfig.Producer.Compression = sarama.CompressionLZ4
	case "zstd":
		producerConfig.Producer.Compression = sarama.CompressionZSTD
	default:
		producerConfig.Producer.Compression = sarama.CompressionNone
	}

	// 创建生产者 / Create producer
	producer, err := sarama.NewSyncProducer(config.Brokers, producerConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kafka producer: %w", err)
	}

	// 配置消费者 / Configure consumer
	consumerConfig := sarama.NewConfig()
	consumerConfig.Version = version
	consumerConfig.Consumer.Group.Rebalance.Strategy = sarama.BalanceStrategyRoundRobin
	consumerConfig.Consumer.Offsets.Initial = sarama.OffsetNewest // 从最新消息开始 / Start from latest
	consumerConfig.Consumer.Return.Errors = config.ReturnErrors

	// 创建消费者组 / Create consumer group
	consumer, err := sarama.NewConsumerGroup(config.Brokers, config.ConsumerGroup, consumerConfig)
	if err != nil {
		producer.Close()
		return nil, fmt.Errorf("failed to create Kafka consumer group: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &KafkaRouter{
		producer:  producer,
		consumer:  consumer,
		gatewayID: gatewayID,
		ctx:       ctx,
		cancel:    cancel,
		brokers:   config.Brokers,
	}, nil
}

// Start starts the Kafka router and begins consuming messages
// 启动 Kafka 路由器并开始消费消息
func (r *KafkaRouter) Start(ctx context.Context, handler MessageHandler) error {
	r.handler = handler

	// 获取本 Gateway 的 topic / Get this Gateway's topic
	topic := r.getGatewayTopic(r.gatewayID)

	log.Printf("[KafkaRouter] Starting consumer for topic: %s", topic)

	// 启动消费者协程 / Start consumer goroutine
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		consumerHandler := &kafkaConsumerHandler{
			handler: handler,
			router:  r,
		}

		for {
			// 消费消息（会自动重连）/ Consume messages (auto-reconnects)
			err := r.consumer.Consume(r.ctx, []string{topic}, consumerHandler)
			if err != nil {
				log.Printf("[KafkaRouter] Consumer error: %v", err)
			}

			// 检查是否应该退出 / Check if should exit
			select {
			case <-r.ctx.Done():
				log.Println("[KafkaRouter] Context cancelled, stopping consumer")
				return
			default:
				// 出错后等待 1 秒重试 / Wait 1 second before retry
				time.Sleep(1 * time.Second)
			}
		}
	}()

	// 监听消费者错误 / Monitor consumer errors
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for err := range r.consumer.Errors() {
			log.Printf("[KafkaRouter] Consumer group error: %v", err)
		}
	}()

	log.Printf("[KafkaRouter] Started consuming from topic: %s", topic)
	return nil
}

// Stop gracefully stops the Kafka router
// 优雅地停止 Kafka 路由器
func (r *KafkaRouter) Stop() error {
	log.Println("[KafkaRouter] Stopping router...")

	// 取消 context / Cancel context
	r.cancel()

	// 等待所有 goroutine 结束 / Wait for all goroutines to finish
	r.wg.Wait()

	// 关闭消费者 / Close consumer
	if err := r.consumer.Close(); err != nil {
		log.Printf("[KafkaRouter] Error closing consumer: %v", err)
	}

	// 关闭生产者 / Close producer
	if err := r.producer.Close(); err != nil {
		log.Printf("[KafkaRouter] Error closing producer: %v", err)
	}

	log.Println("[KafkaRouter] Router stopped")
	return nil
}

// RouteToGateway sends a message to a specific gateway via Kafka
// 通过 Kafka 将消息发送到特定的 Gateway
func (r *KafkaRouter) RouteToGateway(ctx context.Context, targetGatewayID string, msg *Message) error {
	// 计算目标 topic / Calculate target topic
	topic := r.getGatewayTopic(targetGatewayID)

	// 序列化消息 / Serialize message
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// 创建 Kafka 消息 / Create Kafka message
	kafkaMsg := &sarama.ProducerMessage{
		Topic: topic,
		Key:   sarama.StringEncoder(msg.To), // 使用目标用户 ID 作为 key，保证同一用户的消息有序
		                                       // Use target userId as key to ensure ordering for same user
		Value: sarama.ByteEncoder(data),
		Headers: []sarama.RecordHeader{
			{
				Key:   []byte("from_gateway"),
				Value: []byte(r.gatewayID),
			},
			{
				Key:   []byte("timestamp"),
				Value: []byte(fmt.Sprintf("%d", time.Now().Unix())),
			},
		},
	}

	// 发送消息 / Send message
	partition, offset, err := r.producer.SendMessage(kafkaMsg)
	if err != nil {
		return fmt.Errorf("failed to send message to Kafka: %w", err)
	}

	log.Printf("[KafkaRouter] Routed message from %s to %s via gateway %s (partition=%d, offset=%d)",
		msg.From, msg.To, targetGatewayID, partition, offset)

	return nil
}

// BroadcastToAllGateways broadcasts a message to all gateways
// 向所有 Gateway 广播消息
func (r *KafkaRouter) BroadcastToAllGateways(ctx context.Context, msg *Message) error {
	// 使用特殊的广播 topic / Use special broadcast topic
	topic := "gateway-broadcast"

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	kafkaMsg := &sarama.ProducerMessage{
		Topic: topic,
		Value: sarama.ByteEncoder(data),
		Headers: []sarama.RecordHeader{
			{
				Key:   []byte("from_gateway"),
				Value: []byte(r.gatewayID),
			},
			{
				Key:   []byte("broadcast"),
				Value: []byte("true"),
			},
		},
	}

	partition, offset, err := r.producer.SendMessage(kafkaMsg)
	if err != nil {
		return fmt.Errorf("failed to broadcast message to Kafka: %w", err)
	}

	log.Printf("[KafkaRouter] Broadcast message from %s to all gateways (partition=%d, offset=%d)",
		msg.From, partition, offset)

	return nil
}

// getGatewayTopic returns the Kafka topic name for a gateway
// 返回 Gateway 的 Kafka topic 名称
func (r *KafkaRouter) getGatewayTopic(gatewayID string) string {
	return fmt.Sprintf("gateway-%s", gatewayID)
}

// kafkaConsumerHandler implements sarama.ConsumerGroupHandler
// kafkaConsumerHandler 实现 sarama.ConsumerGroupHandler 接口
type kafkaConsumerHandler struct {
	handler MessageHandler
	router  *KafkaRouter
}

// Setup is called when a new session is created
// 在创建新会话时调用
func (h *kafkaConsumerHandler) Setup(sarama.ConsumerGroupSession) error {
	log.Println("[KafkaRouter] Consumer group session started")
	return nil
}

// Cleanup is called when a session is closed
// 在会话关闭时调用
func (h *kafkaConsumerHandler) Cleanup(sarama.ConsumerGroupSession) error {
	log.Println("[KafkaRouter] Consumer group session closed")
	return nil
}

// ConsumeClaim processes messages from a partition
// 处理分区中的消息
func (h *kafkaConsumerHandler) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for {
		select {
		case msg := <-claim.Messages():
			if msg == nil {
				return nil
			}

			// 反序列化消息 / Deserialize message
			var routedMsg Message
			if err := json.Unmarshal(msg.Value, &routedMsg); err != nil {
				log.Printf("[KafkaRouter] Failed to unmarshal message: %v", err)
				// 标记为已处理，跳过错误消息 / Mark as processed, skip bad message
				session.MarkMessage(msg, "")
				continue
			}

			// 检查是否是从自己发出的消息（避免循环）/ Check if from self (avoid loops)
			for _, header := range msg.Headers {
				if string(header.Key) == "from_gateway" && string(header.Value) == h.router.gatewayID {
					// 跳过自己发出的消息 / Skip messages from self
					session.MarkMessage(msg, "")
					continue
				}
			}

			log.Printf("[KafkaRouter] Received message for delivery: from=%s to=%s (partition=%d, offset=%d)",
				routedMsg.From, routedMsg.To, msg.Partition, msg.Offset)

			// 调用处理器 / Call handler
			if h.handler != nil {
				h.handler(&routedMsg)
			}

			// 标记消息已处理 / Mark message as processed
			session.MarkMessage(msg, "")

		case <-session.Context().Done():
			return nil
		}
	}
}

// GetMetrics returns Kafka-specific metrics
// 返回 Kafka 特定的指标
func (r *KafkaRouter) GetMetrics() map[string]interface{} {
	return map[string]interface{}{
		"gateway_id": r.gatewayID,
		"brokers":    r.brokers,
		"topic":      r.getGatewayTopic(r.gatewayID),
	}
}
