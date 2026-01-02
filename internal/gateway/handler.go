package gateway

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"websocket-demo/internal/router"

	"github.com/gorilla/websocket"
)

const (
	// Heartbeat settings
	heartbeatInterval = 30 * time.Second
	heartbeatTimeout  = 90 * time.Second

	// Message types
	msgTypePing     = "ping"
	msgTypePong     = "pong"
	msgTypeMessage  = "message"
	msgTypeRegister = "register"
	msgTypeError    = "error"
)

// ClientMessage represents a message from the client
type ClientMessage struct {
	Type    string `json:"type"`
	To      string `json:"to,omitempty"`
	Content string `json:"content,omitempty"`
	UserID  string `json:"userId,omitempty"` // For registration
}

// ServerMessage represents a message to the client
type ServerMessage struct {
	Type    string `json:"type"`
	From    string `json:"from,omitempty"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

// handleConnection handles a WebSocket connection
func (s *Server) handleConnection(conn *websocket.Conn, connID string) {
	defer conn.Close()

	var userID string
	var wsConn *Connection

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Read messages
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("[Handler] WebSocket error: %v", err)
			}
			break
		}

		var msg ClientMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("[Handler] Failed to unmarshal message: %v", err)
			s.sendError(conn, "Invalid message format")
			continue
		}

		switch msg.Type {
		case msgTypeRegister:
			// Register the connection
			if msg.UserID == "" {
				s.sendError(conn, "UserID is required for registration")
				continue
			}

			userID = msg.UserID
			wsConn = NewConnection(connID, userID, conn)

			// Add to connection manager
			s.connMgr.Add(wsConn)

			// Register presence in Redis
			if err := s.presenceMgr.Register(ctx, userID, s.gatewayID, connID); err != nil {
				log.Printf("[Handler] Failed to register presence: %v", err)
				s.sendError(conn, "Failed to register")
				continue
			}

			log.Printf("[Handler] User %s registered on gateway %s (connID: %s)", userID, s.gatewayID, connID)

			// Send confirmation
			s.sendMessage(conn, ServerMessage{
				Type:    "registered",
				Content: "Successfully registered",
			})

			// Start heartbeat checker
			go s.heartbeatChecker(ctx, wsConn)

		case msgTypePing:
			// Update last ping time
			if wsConn != nil {
				wsConn.UpdatePing()

				// Refresh presence TTL
				if err := s.presenceMgr.Refresh(ctx, userID); err != nil {
					log.Printf("[Handler] Failed to refresh presence: %v", err)
				}
			}

			// Send pong
			s.sendMessage(conn, ServerMessage{Type: msgTypePong})

		case msgTypeMessage:
			// Route message to recipient
			if userID == "" {
				s.sendError(conn, "Not registered")
				continue
			}

			if msg.To == "" {
				s.sendError(conn, "Recipient is required")
				continue
			}

			if err := s.routeMessage(ctx, userID, msg.To, msg.Content); err != nil {
				log.Printf("[Handler] Failed to route message: %v", err)
				s.sendError(conn, "Failed to send message")
				continue
			}

			log.Printf("[Handler] Message routed: %s -> %s", userID, msg.To)

		default:
			s.sendError(conn, "Unknown message type")
		}
	}

	// Cleanup on disconnect
	if wsConn != nil {
		s.connMgr.Remove(wsConn)

		if err := s.presenceMgr.Remove(ctx, userID); err != nil {
			log.Printf("[Handler] Failed to remove presence: %v", err)
		}

		log.Printf("[Handler] User %s disconnected (connID: %s)", userID, connID)
	}
}

// routeMessage routes a message to the recipient
func (s *Server) routeMessage(ctx context.Context, from, to, content string) error {
	// Check if recipient is online
	presence, err := s.presenceMgr.Get(ctx, to)
	if err != nil {
		return err
	}

	msg := &router.Message{
		From:    from,
		To:      to,
		Content: content,
		Type:    "direct",
	}

	// Route to the appropriate gateway
	return s.router.RouteToGateway(ctx, presence.GatewayID, msg)
}

// deliverMessage delivers a message to a local connection
func (s *Server) deliverMessage(msg *router.Message) {
	conn, ok := s.connMgr.GetByUserID(msg.To)
	if !ok {
		log.Printf("[Handler] User %s not found locally", msg.To)
		return
	}

	serverMsg := ServerMessage{
		Type:    msgTypeMessage,
		From:    msg.From,
		Content: msg.Content,
	}

	s.sendMessage(conn.Conn, serverMsg)
	log.Printf("[Handler] Message delivered to %s", msg.To)
}

// sendMessage sends a message to the client
func (s *Server) sendMessage(conn *websocket.Conn, msg ServerMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("[Handler] Failed to marshal message: %v", err)
		return
	}

	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("[Handler] Failed to send message: %v", err)
	}
}

// sendError sends an error message to the client
func (s *Server) sendError(conn *websocket.Conn, errMsg string) {
	s.sendMessage(conn, ServerMessage{
		Type:  msgTypeError,
		Error: errMsg,
	})
}

// heartbeatChecker periodically checks if the connection is still alive
func (s *Server) heartbeatChecker(ctx context.Context, conn *Connection) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if time.Since(conn.GetLastPing()) > heartbeatTimeout {
				log.Printf("[Handler] Connection timeout for user %s", conn.UserID)
				conn.Close()
				return
			}

		case <-ctx.Done():
			return
		}
	}
}
