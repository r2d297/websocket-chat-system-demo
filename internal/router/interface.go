package router

import "context"

// RouterInterface defines the interface for message routing
// RouterInterface 定义消息路由的接口
type RouterInterface interface {
	// Start starts the router with a message handler
	// 使用消息处理器启动路由器
	Start(ctx context.Context, handler MessageHandler) error

	// Stop gracefully stops the router
	// 优雅地停止路由器
	Stop() error

	// RouteToGateway routes a message to a specific gateway
	// 将消息路由到特定的 Gateway
	RouteToGateway(ctx context.Context, targetGatewayID string, msg *Message) error

	// BroadcastToAllGateways broadcasts a message to all gateways
	// 向所有 Gateway 广播消息
	BroadcastToAllGateways(ctx context.Context, msg *Message) error
}

// Ensure Router implements RouterInterface
var _ RouterInterface = (*Router)(nil)

// Ensure KafkaRouter implements RouterInterface
var _ RouterInterface = (*KafkaRouter)(nil)
