package gateway

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Connection represents a WebSocket connection
type Connection struct {
	ID       string
	UserID   string
	Conn     *websocket.Conn
	LastPing time.Time
	mu       sync.Mutex
}

// NewConnection creates a new connection
func NewConnection(id, userID string, conn *websocket.Conn) *Connection {
	return &Connection{
		ID:       id,
		UserID:   userID,
		Conn:     conn,
		LastPing: time.Now(),
	}
}

// UpdatePing updates the last ping time
func (c *Connection) UpdatePing() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastPing = time.Now()
}

// GetLastPing returns the last ping time
func (c *Connection) GetLastPing() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.LastPing
}

// Send sends a message to the connection
func (c *Connection) Send(messageType int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Conn.WriteMessage(messageType, data)
}

// Close closes the connection
func (c *Connection) Close() error {
	return c.Conn.Close()
}

// ConnectionManager manages all active WebSocket connections
type ConnectionManager struct {
	// Bidirectional mappings
	userToConn sync.Map // userID -> *Connection
	connToUser sync.Map // connID -> *Connection

	mu sync.RWMutex
}

// NewConnectionManager creates a new connection manager
func NewConnectionManager() *ConnectionManager {
	return &ConnectionManager{}
}

// Add adds a new connection
func (cm *ConnectionManager) Add(conn *Connection) {
	cm.userToConn.Store(conn.UserID, conn)
	cm.connToUser.Store(conn.ID, conn)
}

// Remove removes a connection
func (cm *ConnectionManager) Remove(conn *Connection) {
	cm.userToConn.Delete(conn.UserID)
	cm.connToUser.Delete(conn.ID)
}

// GetByUserID gets a connection by user ID
func (cm *ConnectionManager) GetByUserID(userID string) (*Connection, bool) {
	val, ok := cm.userToConn.Load(userID)
	if !ok {
		return nil, false
	}
	return val.(*Connection), true
}

// GetByConnID gets a connection by connection ID
func (cm *ConnectionManager) GetByConnID(connID string) (*Connection, bool) {
	val, ok := cm.connToUser.Load(connID)
	if !ok {
		return nil, false
	}
	return val.(*Connection), true
}

// Count returns the number of active connections
func (cm *ConnectionManager) Count() int {
	count := 0
	cm.userToConn.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}

// ForEach iterates over all connections
func (cm *ConnectionManager) ForEach(fn func(*Connection)) {
	cm.userToConn.Range(func(_, value interface{}) bool {
		fn(value.(*Connection))
		return true
	})
}

// CheckHealth checks all connections and removes stale ones
func (cm *ConnectionManager) CheckHealth(timeout time.Duration) int {
	removed := 0
	now := time.Now()

	var toRemove []*Connection

	cm.userToConn.Range(func(_, value interface{}) bool {
		conn := value.(*Connection)
		if now.Sub(conn.GetLastPing()) > timeout {
			toRemove = append(toRemove, conn)
		}
		return true
	})

	for _, conn := range toRemove {
		conn.Close()
		cm.Remove(conn)
		removed++
	}

	return removed
}
