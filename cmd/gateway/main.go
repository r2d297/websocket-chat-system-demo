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

	"github.com/redis/go-redis/v9"
)

func main() {
	// Parse command-line flags
	gatewayID := flag.String("id", "", "Gateway ID (required)")
	port := flag.Int("port", 8080, "HTTP port")
	redisAddr := flag.String("redis", "localhost:6379", "Redis address")
	flag.Parse()

	if *gatewayID == "" {
		log.Fatal("Gateway ID is required (use -id flag)")
	}

	// Create Redis client
	redisClient := redis.NewClient(&redis.Options{
		Addr: *redisAddr,
	})

	ctx := context.Background()

	// Test Redis connection
	if err := redisClient.Ping(ctx).Err(); err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}

	log.Println("Connected to Redis")

	// Create and start server
	server := gateway.NewServer(*gatewayID, *port, redisClient)

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Received shutdown signal")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := server.Stop(shutdownCtx); err != nil {
			log.Printf("Error during shutdown: %v", err)
		}

		os.Exit(0)
	}()

	// Start server (blocks)
	if err := server.Start(ctx); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
