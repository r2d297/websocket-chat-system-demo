package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/redis/go-redis/v9"
)

// Message represents a routable message
type Message struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Content string `json:"content"`
	Type    string `json:"type"` // "direct", "broadcast"
}

// MessageHandler is called when a message is received for local delivery
type MessageHandler func(msg *Message)

// Router handles message routing between gateways using Redis Pub/Sub
type Router struct {
	redis     *redis.Client
	gatewayID string
	pubsub    *redis.PubSub
	handler   MessageHandler
	done      chan struct{}
}

// NewRouter creates a new message router
func NewRouter(redisClient *redis.Client, gatewayID string) *Router {
	return &Router{
		redis:     redisClient,
		gatewayID: gatewayID,
		done:      make(chan struct{}),
	}
}

// Start begins listening for messages on this gateway's channel
func (r *Router) Start(ctx context.Context, handler MessageHandler) error {
	r.handler = handler

	// Subscribe to this gateway's channel
	channel := r.getGatewayChannel(r.gatewayID)
	r.pubsub = r.redis.Subscribe(ctx, channel)

	// Wait for subscription confirmation
	_, err := r.pubsub.Receive(ctx)
	if err != nil {
		return fmt.Errorf("failed to subscribe: %w", err)
	}

	log.Printf("[Router] Subscribed to channel: %s", channel)

	// Start message processing loop
	go r.processMessages(ctx)

	return nil
}

// Stop stops the router
func (r *Router) Stop() error {
	close(r.done)
	if r.pubsub != nil {
		return r.pubsub.Close()
	}
	return nil
}

// RouteToGateway sends a message to a specific gateway
func (r *Router) RouteToGateway(ctx context.Context, targetGatewayID string, msg *Message) error {
	channel := r.getGatewayChannel(targetGatewayID)

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	err = r.redis.Publish(ctx, channel, data).Err()
	if err != nil {
		return fmt.Errorf("failed to publish message: %w", err)
	}

	log.Printf("[Router] Routed message from %s to %s via gateway %s", msg.From, msg.To, targetGatewayID)

	return nil
}

// BroadcastToAllGateways sends a message to all gateways
func (r *Router) BroadcastToAllGateways(ctx context.Context, msg *Message) error {
	channel := "gateway:broadcast"

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	err = r.redis.Publish(ctx, channel, data).Err()
	if err != nil {
		return fmt.Errorf("failed to broadcast message: %w", err)
	}

	log.Printf("[Router] Broadcast message from %s to all gateways", msg.From)

	return nil
}

// processMessages processes incoming messages from the pub/sub channel
func (r *Router) processMessages(ctx context.Context) {
	ch := r.pubsub.Channel()

	for {
		select {
		case msg := <-ch:
			if msg == nil {
				continue
			}

			var routedMsg Message
			if err := json.Unmarshal([]byte(msg.Payload), &routedMsg); err != nil {
				log.Printf("[Router] Failed to unmarshal message: %v", err)
				continue
			}

			log.Printf("[Router] Received message for delivery: from=%s to=%s", routedMsg.From, routedMsg.To)

			// Deliver to local connections
			if r.handler != nil {
				r.handler(&routedMsg)
			}

		case <-r.done:
			log.Println("[Router] Stopped message processing")
			return

		case <-ctx.Done():
			log.Println("[Router] Context cancelled, stopping")
			return
		}
	}
}

// getGatewayChannel returns the Redis channel name for a gateway
func (r *Router) getGatewayChannel(gatewayID string) string {
	return fmt.Sprintf("gateway:%s", gatewayID)
}
