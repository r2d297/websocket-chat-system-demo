# WhatsApp-like System Implementation Plan

## Phase 1: Foundation - Message Persistence (Week 1-2)

### Overview
Add PostgreSQL for durable message storage with message acknowledgments and idempotency support.

### Prerequisites
```bash
# Install PostgreSQL
brew install postgresql@15

# Install pgx driver (already in go.mod via sarama dependencies)
go get github.com/jackc/pgx/v5
go get github.com/jackc/pgx/v5/pgxpool
```

### Step 1.1: Database Setup

**Create database and schema:**

```bash
# Start PostgreSQL
brew services start postgresql@15

# Create database
createdb whatsapp_demo

# Apply schema
psql whatsapp_demo < schema/001_initial_schema.sql
```

**File: `schema/001_initial_schema.sql`**

```sql
-- Users table
CREATE TABLE users (
    user_id VARCHAR(64) PRIMARY KEY,
    username VARCHAR(255) UNIQUE NOT NULL,
    created_at TIMESTAMP DEFAULT NOW(),
    last_seen TIMESTAMP
);

-- Messages table
CREATE TABLE messages (
    message_id VARCHAR(64) PRIMARY KEY,
    from_user_id VARCHAR(64) REFERENCES users(user_id),
    to_user_id VARCHAR(64) REFERENCES users(user_id),
    content TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT NOW(),
    expires_at TIMESTAMP DEFAULT NOW() + INTERVAL '30 days'
);

-- Message delivery tracking
CREATE TABLE message_delivery (
    message_id VARCHAR(64) REFERENCES messages(message_id),
    recipient_id VARCHAR(64) REFERENCES users(user_id),
    status VARCHAR(20) DEFAULT 'sent',
    delivered_at TIMESTAMP,
    read_at TIMESTAMP,
    PRIMARY KEY (message_id, recipient_id)
);

-- Indexes
CREATE INDEX idx_messages_to ON messages(to_user_id, created_at DESC);
CREATE INDEX idx_messages_from ON messages(from_user_id, created_at DESC);
CREATE INDEX idx_delivery_status ON message_delivery(recipient_id, status);
CREATE INDEX idx_messages_expires ON messages(expires_at);

-- Auto-create user on insert if not exists
CREATE OR REPLACE FUNCTION ensure_user_exists()
RETURNS TRIGGER AS $$
BEGIN
    INSERT INTO users (user_id, username)
    VALUES (NEW.from_user_id, NEW.from_user_id)
    ON CONFLICT (user_id) DO NOTHING;
    
    IF NEW.to_user_id IS NOT NULL THEN
        INSERT INTO users (user_id, username)
        VALUES (NEW.to_user_id, NEW.to_user_id)
        ON CONFLICT (user_id) DO NOTHING;
    END IF;
    
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ensure_users_before_message
BEFORE INSERT ON messages
FOR EACH ROW
EXECUTE FUNCTION ensure_user_exists();
```

### Step 1.2: Message Store Implementation

**File: `internal/storage/message_store.go`**

```go
package storage

import (
    "context"
    "fmt"
    "time"
    
    "github.com/jackc/pgx/v5/pgxpool"
)

type Message struct {
    MessageID  string    `json:"messageId"`
    From       string    `json:"from"`
    To         string    `json:"to"`
    Content    string    `json:"content"`
    CreatedAt  time.Time `json:"createdAt"`
    Status     string    `json:"status"`
}

type MessageStore struct {
    pool *pgxpool.Pool
}

func NewMessageStore(ctx context.Context, connString string) (*MessageStore, error) {
    pool, err := pgxpool.New(ctx, connString)
    if err != nil {
        return nil, fmt.Errorf("failed to create connection pool: %w", err)
    }
    
    // Test connection
    if err := pool.Ping(ctx); err != nil {
        return nil, fmt.Errorf("failed to ping database: %w", err)
    }
    
    return &MessageStore{pool: pool}, nil
}

func (ms *MessageStore) Close() {
    ms.pool.Close()
}

// Save persists a message to the database
func (ms *MessageStore) Save(ctx context.Context, msg *Message) error {
    tx, err := ms.pool.Begin(ctx)
    if err != nil {
        return fmt.Errorf("failed to begin transaction: %w", err)
    }
    defer tx.Rollback(ctx)
    
    // Insert message
    _, err = tx.Exec(ctx, `
        INSERT INTO messages (message_id, from_user_id, to_user_id, content)
        VALUES ($1, $2, $3, $4)
        ON CONFLICT (message_id) DO NOTHING
    `, msg.MessageID, msg.From, msg.To, msg.Content)
    
    if err != nil {
        return fmt.Errorf("failed to insert message: %w", err)
    }
    
    // Insert delivery tracking
    _, err = tx.Exec(ctx, `
        INSERT INTO message_delivery (message_id, recipient_id, status)
        VALUES ($1, $2, 'sent')
        ON CONFLICT (message_id, recipient_id) DO NOTHING
    `, msg.MessageID, msg.To)
    
    if err != nil {
        return fmt.Errorf("failed to insert delivery record: %w", err)
    }
    
    return tx.Commit(ctx)
}

// GetUndeliveredMessages fetches messages that haven't been delivered to a user
func (ms *MessageStore) GetUndeliveredMessages(ctx context.Context, userID string, limit int) ([]*Message, error) {
    rows, err := ms.pool.Query(ctx, `
        SELECT m.message_id, m.from_user_id, m.to_user_id, m.content, m.created_at, md.status
        FROM messages m
        JOIN message_delivery md ON m.message_id = md.message_id
        WHERE md.recipient_id = $1 AND md.status IN ('sent', 'delivered')
        ORDER BY m.created_at ASC
        LIMIT $2
    `, userID, limit)
    
    if err != nil {
        return nil, fmt.Errorf("failed to query undelivered messages: %w", err)
    }
    defer rows.Close()
    
    messages := make([]*Message, 0)
    for rows.Next() {
        msg := &Message{}
        err := rows.Scan(&msg.MessageID, &msg.From, &msg.To, &msg.Content, &msg.CreatedAt, &msg.Status)
        if err != nil {
            return nil, fmt.Errorf("failed to scan message: %w", err)
        }
        messages = append(messages, msg)
    }
    
    return messages, nil
}

// MarkDelivered updates the delivery status to 'delivered'
func (ms *MessageStore) MarkDelivered(ctx context.Context, messageID, recipientID string) error {
    _, err := ms.pool.Exec(ctx, `
        UPDATE message_delivery
        SET status = 'delivered', delivered_at = NOW()
        WHERE message_id = $1 AND recipient_id = $2
    `, messageID, recipientID)
    
    return err
}

// MarkRead updates the delivery status to 'read'
func (ms *MessageStore) MarkRead(ctx context.Context, messageID, recipientID string) error {
    _, err := ms.pool.Exec(ctx, `
        UPDATE message_delivery
        SET status = 'read', read_at = NOW()
        WHERE message_id = $1 AND recipient_id = $2
    `, messageID, recipientID)
    
    return err
}

// DeleteExpiredMessages removes messages older than 30 days
func (ms *MessageStore) DeleteExpiredMessages(ctx context.Context) (int64, error) {
    result, err := ms.pool.Exec(ctx, `
        DELETE FROM messages WHERE expires_at < NOW()
    `)
    
    if err != nil {
        return 0, err
    }
    
    return result.RowsAffected(), nil
}
```

### Step 1.3: Update Gateway Server

**File: `internal/gateway/server.go` (modifications)**

```go
// Add MessageStore field
type Server struct {
    gatewayID   string
    port        int
    connMgr     *ConnectionManager
    presenceMgr *presence.Manager
    router      router.RouterInterface
    messageStore *storage.MessageStore  // NEW
    httpServer  *http.Server
}

// Update NewServer constructor
func NewServer(gatewayID string, port int, redisClient *redis.Client, messageStore *storage.MessageStore) *Server {
    presenceMgr := presence.NewManager(redisClient)
    msgRouter := router.NewRouter(redisClient, gatewayID)

    return &Server{
        gatewayID:    gatewayID,
        port:         port,
        connMgr:      NewConnectionManager(),
        presenceMgr:  presenceMgr,
        router:       msgRouter,
        messageStore: messageStore,  // NEW
    }
}
```

### Step 1.4: Update Message Handler

**File: `internal/gateway/handler.go` (modifications)**

```go
import (
    "github.com/google/uuid"
    "websocket-demo/internal/storage"
)

// Update ClientMessage to include MessageID
type ClientMessage struct {
    Type      string `json:"type"`
    MessageID string `json:"messageId,omitempty"`  // NEW
    To        string `json:"to,omitempty"`
    Content   string `json:"content,omitempty"`
    UserID    string `json:"userId,omitempty"`
}

// Update ServerMessage to include MessageID
type ServerMessage struct {
    Type      string `json:"type"`
    MessageID string `json:"messageId,omitempty"`  // NEW
    From      string `json:"from,omitempty"`
    Content   string `json:"content,omitempty"`
    Error     string `json:"error,omitempty"`
    Status    string `json:"status,omitempty"`      // NEW (sent, delivered, read)
}

// Update message handling in handleConnection
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
    
    // Generate message ID if not provided
    if msg.MessageID == "" {
        msg.MessageID = uuid.New().String()
    }

    // NEW: Persist message first
    storeMsg := &storage.Message{
        MessageID: msg.MessageID,
        From:      userID,
        To:        msg.To,
        Content:   msg.Content,
    }
    
    if err := s.messageStore.Save(ctx, storeMsg); err != nil {
        log.Printf("[Handler] Failed to save message: %v", err)
        s.sendError(conn, "Failed to save message")
        continue
    }
    
    // Send ACK back to sender
    s.sendMessage(conn, ServerMessage{
        Type:      "ack",
        MessageID: msg.MessageID,
        Status:    "sent",
    })

    // Route message
    if err := s.routeMessage(ctx, userID, msg.To, msg.MessageID, msg.Content); err != nil {
        log.Printf("[Handler] Failed to route message: %v", err)
        // Message already saved, will be delivered when recipient comes online
        continue
    }

    log.Printf("[Handler] Message routed: %s -> %s (ID: %s)", userID, msg.To, msg.MessageID)
```

### Step 1.5: Update Router to Include MessageID

**File: `internal/router/router.go` (modifications)**

```go
// Update Message struct
type Message struct {
    MessageID string `json:"messageId"`  // NEW
    From      string `json:"from"`
    To        string `json:"to"`
    Content   string `json:"content"`
    Type      string `json:"type"`
}
```

### Step 1.6: Offline Message Delivery

**File: `internal/gateway/handler.go` (add new function)**

```go
// deliverOfflineMessages sends queued messages when user comes online
func (s *Server) deliverOfflineMessages(ctx context.Context, userID string) {
    messages, err := s.messageStore.GetUndeliveredMessages(ctx, userID, 100)
    if err != nil {
        log.Printf("[Handler] Failed to fetch offline messages: %v", err)
        return
    }
    
    conn, ok := s.connMgr.GetByUserID(userID)
    if !ok {
        log.Printf("[Handler] User %s not connected", userID)
        return
    }
    
    log.Printf("[Handler] Delivering %d offline messages to %s", len(messages), userID)
    
    for _, msg := range messages {
        serverMsg := ServerMessage{
            Type:      msgTypeMessage,
            MessageID: msg.MessageID,
            From:      msg.From,
            Content:   msg.Content,
        }
        
        s.sendMessage(conn.Conn, serverMsg)
        
        // Mark as delivered
        if err := s.messageStore.MarkDelivered(ctx, msg.MessageID, userID); err != nil {
            log.Printf("[Handler] Failed to mark message %s as delivered: %v", msg.MessageID, err)
        }
        
        // Small delay to avoid overwhelming the client
        time.Sleep(10 * time.Millisecond)
    }
}

// Call this after successful registration
case msgTypeRegister:
    // ... existing registration code ...
    
    // NEW: Deliver offline messages
    go s.deliverOfflineMessages(ctx, userID)
```

### Step 1.7: Update Main Entry Point

**File: `cmd/gateway/main.go` (modifications)**

```go
import (
    "websocket-demo/internal/storage"
)

func main() {
    // ... existing flag parsing ...
    
    // NEW: Add database connection string flag
    dbConnString := flag.String("db", "postgres://localhost/whatsapp_demo?sslmode=disable", "PostgreSQL connection string")
    flag.Parse()
    
    // ... existing code ...
    
    // NEW: Create message store
    messageStore, err := storage.NewMessageStore(ctx, *dbConnString)
    if err != nil {
        log.Fatalf("Failed to create message store: %v", err)
    }
    defer messageStore.Close()
    
    log.Println("Connected to PostgreSQL")
    
    // NEW: Pass message store to server
    server := gateway.NewServer(*gatewayID, *port, redisClient, messageStore)
    
    // ... rest of existing code ...
}
```

### Step 1.8: Testing

**Create test script: `test-persistence.sh`**

```bash
#!/bin/bash

echo "Testing Message Persistence"
echo "============================"

# Start services
echo "1. Starting PostgreSQL..."
brew services start postgresql@15

echo "2. Setting up database..."
createdb whatsapp_demo 2>/dev/null || true
psql whatsapp_demo < schema/001_initial_schema.sql

echo "3. Starting Redis..."
docker-compose up -d

echo "4. Building gateway with persistence..."
go build -o bin/gateway-persist cmd/gateway/main.go

echo "5. Starting gateway..."
./bin/gateway-persist -id gateway-01 -port 8080 -db "postgres://localhost/whatsapp_demo?sslmode=disable" &
GATEWAY_PID=$!
sleep 2

echo "6. Testing offline message delivery..."
echo "   - Start client Bob (will disconnect)"
echo "   - Send message from Alice to Bob while Bob is offline"
echo "   - Bob reconnects and should receive the message"

echo ""
echo "Manual test steps:"
echo "  Terminal 1: ./bin/client -user bob -gateway ws://localhost:8080/ws"
echo "  (Then disconnect Bob)"
echo ""
echo "  Terminal 2: ./bin/client -user alice -gateway ws://localhost:8080/ws"
echo "  alice> send bob Hello offline Bob!"
echo ""
echo "  Terminal 1: ./bin/client -user bob -gateway ws://localhost:8080/ws"
echo "  (Bob should receive the offline message)"

# Cleanup
trap "kill $GATEWAY_PID 2>/dev/null" EXIT
wait
```

### Step 1.9: Verification Queries

```sql
-- Check messages
SELECT * FROM messages ORDER BY created_at DESC LIMIT 10;

-- Check delivery status
SELECT m.message_id, m.from_user_id, m.to_user_id, m.content, md.status, md.delivered_at
FROM messages m
JOIN message_delivery md ON m.message_id = md.message_id
ORDER BY m.created_at DESC;

-- Check undelivered messages for a user
SELECT COUNT(*) 
FROM message_delivery 
WHERE recipient_id = 'bob' AND status = 'sent';
```

### Success Criteria

- ✅ Messages are persisted to PostgreSQL
- ✅ Messages receive unique IDs
- ✅ Sender receives ACK confirmation
- ✅ Offline messages are delivered when user reconnects
- ✅ Delivery status is tracked (sent → delivered)
- ✅ Messages expire after 30 days
- ✅ No duplicate messages (idempotency)

### Next Phase Preview

Phase 2 will add:
- Read receipts
- Message delivery retries
- Batch offline message delivery
- Kafka-based offline queue

