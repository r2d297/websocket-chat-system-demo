package gateway

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"websocket-demo/internal/presence"
	"websocket-demo/internal/router"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for demo
	},
}

// Server represents the WebSocket gateway server
type Server struct {
	gatewayID   string
	port        int
	connMgr     *ConnectionManager
	presenceMgr *presence.Manager
	router      *router.Router
	httpServer  *http.Server
}

// NewServer creates a new gateway server
func NewServer(gatewayID string, port int, redisClient *redis.Client) *Server {
	presenceMgr := presence.NewManager(redisClient)
	msgRouter := router.NewRouter(redisClient, gatewayID)

	return &Server{
		gatewayID:   gatewayID,
		port:        port,
		connMgr:     NewConnectionManager(),
		presenceMgr: presenceMgr,
		router:      msgRouter,
	}
}

// Start starts the server
func (s *Server) Start(ctx context.Context) error {
	// Start message router
	if err := s.router.Start(ctx, s.deliverMessage); err != nil {
		return fmt.Errorf("failed to start router: %w", err)
	}

	// Start health check routine
	go s.healthCheckLoop(ctx)

	// Setup HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/stats", s.handleStats)

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}

	log.Printf("[Server] Gateway %s starting on port %d", s.gatewayID, s.port)

	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("failed to start HTTP server: %w", err)
	}

	return nil
}

// Stop stops the server
func (s *Server) Stop(ctx context.Context) error {
	log.Printf("[Server] Shutting down gateway %s", s.gatewayID)

	// Stop router
	if err := s.router.Stop(); err != nil {
		log.Printf("[Server] Error stopping router: %v", err)
	}

	// Close all connections
	s.connMgr.ForEach(func(conn *Connection) {
		conn.Close()
	})

	// Shutdown HTTP server
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}

	return nil
}

// handleWebSocket handles WebSocket upgrade requests
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[Server] Failed to upgrade connection: %v", err)
		return
	}

	connID := uuid.New().String()
	log.Printf("[Server] New WebSocket connection: %s", connID)

	s.handleConnection(conn, connID)
}

// handleHealth handles health check requests
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "OK")
}

// handleStats handles stats requests
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"gatewayId":"%s","connections":%d}`, s.gatewayID, s.connMgr.Count())
}

// healthCheckLoop periodically checks connection health
func (s *Server) healthCheckLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			removed := s.connMgr.CheckHealth(heartbeatTimeout)
			if removed > 0 {
				log.Printf("[Server] Health check: removed %d stale connections", removed)
			}

		case <-ctx.Done():
			return
		}
	}
}
