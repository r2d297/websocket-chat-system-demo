package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"websocket-demo/internal/gateway"
	"websocket-demo/internal/router"

	"github.com/redis/go-redis/v9"
)

func main() {
	// 命令行参数 / Command line flags
	gatewayID := flag.String("id", "gateway-01", "Gateway ID")
	port := flag.Int("port", 8080, "HTTP server port")
	redisAddr := flag.String("redis", "localhost:6379", "Redis address")
	kafkaBrokers := flag.String("kafka", "localhost:9092", "Kafka brokers (comma-separated)")
	flag.Parse()

	log.Printf("Starting Gateway %s on port %d (Kafka mode)", *gatewayID, *port)

	// 创建 Redis 客户端（仅用于 Presence 管理）
	// Create Redis client (only for Presence management)
	redisClient := redis.NewClient(&redis.Options{
		Addr: *redisAddr,
	})
	defer redisClient.Close()

	// 测试 Redis 连接 / Test Redis connection
	ctx := context.Background()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	log.Println("Connected to Redis")

	// 创建 Kafka Router（替代 Redis Pub/Sub）
	// Create Kafka Router (replaces Redis Pub/Sub)
	kafkaConfig := router.KafkaConfig{
		Brokers:       []string{*kafkaBrokers},
		ConsumerGroup: "websocket-gateway",
		Version:       "3.0.0",
		ReturnErrors:  true,
		Compression:   "snappy", // 使用 Snappy 压缩 / Use Snappy compression
	}

	kafkaRouter, err := router.NewKafkaRouter(*gatewayID, kafkaConfig)
	if err != nil {
		log.Fatalf("Failed to create Kafka router: %v", err)
	}
	defer kafkaRouter.Stop()

	// 创建 Gateway 服务器（使用 Kafka Router）
	// Create Gateway server (using Kafka Router)
	server := gateway.NewServerWithRouter(*gatewayID, *port, redisClient, kafkaRouter)

	// 启动服务器 / Start server
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()

	go func() {
		if err := server.Start(serverCtx); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// 等待中断信号 / Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Println("Received shutdown signal, gracefully stopping...")

	// 优雅关闭 / Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Stop(shutdownCtx); err != nil {
		log.Printf("Error during shutdown: %v", err)
	}

	log.Println("Gateway stopped")
}
