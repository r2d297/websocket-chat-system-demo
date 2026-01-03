# WhatsApp-like System Design

This document outlines the design for scaling the current WebSocket demo into a WhatsApp-like messaging system.

## Requirements Summary

### Functional Requirements
- ✅ Group chats with up to 100 participants
- ✅ Send/receive messages in real-time
- ✅ Offline message storage (up to 30 days)
- ✅ Media message support (images, videos, documents)

### Non-Functional Requirements
- ✅ Message delivery latency < 500ms for online users
- ✅ Guaranteed message delivery
- ✅ Support billions of users with high throughput
- ✅ Minimal server-side message storage
- ✅ Fault tolerance and resilience

## Current Architecture Gaps

| Feature | Current State | Required State | Gap |
|---------|--------------|----------------|-----|
| Message persistence | ❌ None | ✅ 30-day storage | Need message store |
| User-to-user only | ✅ 1:1 | ✅ Groups (100) | Need group logic |
| Text messages | ✅ Yes | ✅ Text + Media | Need media service |
| In-memory delivery | ✅ Yes | ✅ Offline queue | Need message queue |
| Basic routing | ✅ Yes | ✅ Ack/delivery status | Need receipts |
| Single region | ✅ Yes | ✅ Multi-region | Need federation |

## High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Client Layer                                    │
│  (Mobile/Web) - WebSocket connections + Media upload/download               │
└────────┬────────────────────────────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                         API Gateway / Load Balancer                          │
│  - WebSocket connection routing                                             │
│  - Health checks & service discovery                                        │
└────────┬────────────────────────────────────────────────────────────────────┘
         │
         ├──────────────┬──────────────┬──────────────┬──────────────┐
         ▼              ▼              ▼              ▼              ▼
┌──────────────┐ ┌──────────────┐ ┌──────────────┐ ┌──────────────┐ ┌──────────────┐
│  Gateway-01  │ │  Gateway-02  │ │  Gateway-03  │ │  Gateway-N   │ │  Gateway-K   │
│  (Enhanced)  │ │  (Enhanced)  │ │  (Enhanced)  │ │  (Region A)  │ │  (Region B)  │
└──────┬───────┘ └──────┬───────┘ └──────┬───────┘ └──────┬───────┘ └──────┬───────┘
       │                │                │                │                │
       └────────────────┴────────────────┴────────────────┴────────────────┘
                                    │
         ┌──────────────────────────┼──────────────────────────┐
         ▼                          ▼                          ▼
┌─────────────────┐      ┌─────────────────┐      ┌─────────────────┐
│  Kafka Cluster  │      │  Redis Cluster  │      │   PostgreSQL    │
│  (Message Bus)  │      │  (Presence +    │      │  (Message Store │
│                 │      │   Metadata)     │      │   + User Data)  │
└─────────────────┘      └─────────────────┘      └─────────────────┘
                                                            │
         ┌──────────────────────────────────────────────────┘
         ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                         Supporting Services                                  │
├─────────────────────────────────────────────────────────────────────────────┤
│  • Media Service (S3/CDN) - Upload/download images, videos, docs           │
│  • Message Processor - Async message processing & delivery retries         │
│  • Notification Service - Push notifications for offline users             │
│  • Analytics Service - Delivery metrics, latency monitoring                │
└─────────────────────────────────────────────────────────────────────────────┘
```

## Detailed Component Design

### 1. Enhanced Gateway Server

**New responsibilities:**
- Handle group message fanout (1 message → N recipients)
- Message acknowledgment tracking (sent → delivered → read)
- Offline message queuing
- Media message handling
- Message ordering guarantees

**Key changes to current implementation:**

```go
// internal/gateway/message_types.go
type Message struct {
    MessageID   string    `json:"messageId"`   // UUID for idempotency
    From        string    `json:"from"`
    To          []string  `json:"to"`          // Support multiple recipients
    GroupID     string    `json:"groupId"`     // Optional for group chats
    Content     string    `json:"content"`
    MediaURL    string    `json:"mediaUrl"`    // S3/CDN URL
    MediaType   string    `json:"mediaType"`   // image, video, document
    Timestamp   int64     `json:"timestamp"`
    Status      string    `json:"status"`      // sent, delivered, read
}

// internal/gateway/handler.go - Enhanced handler
func (s *Server) handleMessage(ctx context.Context, msg *ClientMessage) error {
    // 1. Persist message to database first (durability guarantee)
    if err := s.messageStore.Save(ctx, msg); err != nil {
        return err
    }
    
    // 2. Determine recipients (1:1 or group)
    recipients := s.getRecipients(msg)
    
    // 3. For each recipient, check presence and route
    for _, recipientID := range recipients {
        if online, gatewayID := s.presenceMgr.IsOnline(recipientID); online {
            // Online: route immediately
            s.router.RouteToGateway(ctx, gatewayID, msg)
        } else {
            // Offline: queue for later delivery
            s.offlineQueue.Enqueue(recipientID, msg)
        }
    }
    
    // 4. Send ACK back to sender
    return s.sendAck(msg.From, msg.MessageID, "sent")
}
```

### 2. Message Persistence Layer

**Database Schema (PostgreSQL):**

```sql
-- Users table
CREATE TABLE users (
    user_id VARCHAR(64) PRIMARY KEY,
    username VARCHAR(255) UNIQUE NOT NULL,
    created_at TIMESTAMP DEFAULT NOW(),
    last_seen TIMESTAMP
);

-- Groups table
CREATE TABLE groups (
    group_id VARCHAR(64) PRIMARY KEY,
    group_name VARCHAR(255),
    created_by VARCHAR(64) REFERENCES users(user_id),
    created_at TIMESTAMP DEFAULT NOW(),
    max_participants INT DEFAULT 100
);

-- Group members table
CREATE TABLE group_members (
    group_id VARCHAR(64) REFERENCES groups(group_id),
    user_id VARCHAR(64) REFERENCES users(user_id),
    joined_at TIMESTAMP DEFAULT NOW(),
    PRIMARY KEY (group_id, user_id)
);

-- Messages table (partitioned by date for 30-day retention)
CREATE TABLE messages (
    message_id VARCHAR(64) PRIMARY KEY,
    from_user_id VARCHAR(64) REFERENCES users(user_id),
    group_id VARCHAR(64) REFERENCES groups(group_id),
    content TEXT,
    media_url VARCHAR(512),
    media_type VARCHAR(50),
    created_at TIMESTAMP DEFAULT NOW(),
    expires_at TIMESTAMP DEFAULT NOW() + INTERVAL '30 days'
) PARTITION BY RANGE (created_at);

-- Message recipients table (for tracking delivery status)
CREATE TABLE message_recipients (
    message_id VARCHAR(64) REFERENCES messages(message_id),
    recipient_id VARCHAR(64) REFERENCES users(user_id),
    status VARCHAR(20) DEFAULT 'sent',  -- sent, delivered, read
    delivered_at TIMESTAMP,
    read_at TIMESTAMP,
    PRIMARY KEY (message_id, recipient_id)
);

-- Indexes for performance
CREATE INDEX idx_messages_recipient ON message_recipients(recipient_id, status);
CREATE INDEX idx_messages_created ON messages(created_at);
CREATE INDEX idx_messages_expires ON messages(expires_at);
```

**Implementation:**

```go
// internal/storage/message_store.go
type MessageStore struct {
    db *pgxpool.Pool
}

func (ms *MessageStore) Save(ctx context.Context, msg *Message) error {
    tx, err := ms.db.Begin(ctx)
    if err != nil {
        return err
    }
    defer tx.Rollback(ctx)
    
    // Insert message
    _, err = tx.Exec(ctx, `
        INSERT INTO messages (message_id, from_user_id, group_id, content, media_url, media_type)
        VALUES ($1, $2, $3, $4, $5, $6)
    `, msg.MessageID, msg.From, msg.GroupID, msg.Content, msg.MediaURL, msg.MediaType)
    
    if err != nil {
        return err
    }
    
    // Insert recipients with 'sent' status
    for _, recipientID := range msg.To {
        _, err = tx.Exec(ctx, `
            INSERT INTO message_recipients (message_id, recipient_id, status)
            VALUES ($1, $2, 'sent')
        `, msg.MessageID, recipientID)
        
        if err != nil {
            return err
        }
    }
    
    return tx.Commit(ctx)
}

func (ms *MessageStore) GetUndeliveredMessages(ctx context.Context, userID string) ([]*Message, error) {
    rows, err := ms.db.Query(ctx, `
        SELECT m.message_id, m.from_user_id, m.content, m.media_url, m.media_type, m.created_at
        FROM messages m
        JOIN message_recipients mr ON m.message_id = mr.message_id
        WHERE mr.recipient_id = $1 AND mr.status = 'sent'
        ORDER BY m.created_at ASC
        LIMIT 1000
    `, userID)
    
    // Parse and return messages
    // ...
}
```

### 3. Offline Message Queue (Kafka-based)

**Topic Design:**
- `offline-messages-{userID}` - One topic per user for ordered delivery
- Partitioned by user_id hash for scalability

**Implementation:**

```go
// internal/queue/offline_queue.go
type OfflineQueue struct {
    producer sarama.SyncProducer
    store    *MessageStore
}

func (oq *OfflineQueue) Enqueue(recipientID string, msg *Message) error {
    topic := fmt.Sprintf("offline-messages-%s", recipientID)
    
    data, err := json.Marshal(msg)
    if err != nil {
        return err
    }
    
    _, _, err = oq.producer.SendMessage(&sarama.ProducerMessage{
        Topic: topic,
        Key:   sarama.StringEncoder(recipientID),
        Value: sarama.ByteEncoder(data),
    })
    
    return err
}

// Background worker to process offline messages when user comes online
func (oq *OfflineQueue) ProcessOnUserConnect(ctx context.Context, userID string) error {
    // 1. Fetch from database (last 30 days)
    messages, err := oq.store.GetUndeliveredMessages(ctx, userID)
    if err != nil {
        return err
    }
    
    // 2. Deliver messages in order
    for _, msg := range messages {
        if err := oq.deliverToUser(ctx, userID, msg); err != nil {
            // Log but continue with next message
            log.Printf("Failed to deliver message %s: %v", msg.MessageID, err)
        }
    }
    
    return nil
}
```

### 4. Media Service

**Architecture:**
- Upload: Client → Gateway → S3 (pre-signed URL)
- Download: Client ← CDN ← S3
- Metadata stored in message database

**Flow:**

```go
// internal/media/service.go
type MediaService struct {
    s3Client *s3.Client
    cdnURL   string
}

func (ms *MediaService) GenerateUploadURL(ctx context.Context, userID, fileType string) (*UploadInfo, error) {
    // Generate unique file key
    fileKey := fmt.Sprintf("media/%s/%s/%s", userID, time.Now().Format("2006-01-02"), uuid.New().String())
    
    // Create pre-signed PUT URL (15 min expiry)
    presignClient := s3.NewPresignClient(ms.s3Client)
    presignResult, err := presignClient.PresignPutObject(ctx, &s3.PutObjectInput{
        Bucket:      aws.String("whatsapp-media-bucket"),
        Key:         aws.String(fileKey),
        ContentType: aws.String(fileType),
    }, s3.WithPresignExpires(15*time.Minute))
    
    return &UploadInfo{
        UploadURL: presignResult.URL,
        MediaURL:  fmt.Sprintf("%s/%s", ms.cdnURL, fileKey),
        ExpiresAt: time.Now().Add(15 * time.Minute),
    }, nil
}

// Client uploads directly to S3, then sends message with media_url
```

### 5. Group Chat Implementation

**Group message fanout strategies:**

```go
// internal/group/manager.go
type GroupManager struct {
    db           *pgxpool.Pool
    cache        *redis.Client
    maxFanout    int // 100 for groups
}

func (gm *GroupManager) GetGroupMembers(ctx context.Context, groupID string) ([]string, error) {
    // Try cache first
    cacheKey := fmt.Sprintf("group:%s:members", groupID)
    if members, err := gm.cache.SMembers(ctx, cacheKey).Result(); err == nil {
        return members, nil
    }
    
    // Cache miss: fetch from DB
    rows, err := gm.db.Query(ctx, `
        SELECT user_id FROM group_members WHERE group_id = $1
    `, groupID)
    
    // Parse members and cache
    members := []string{}
    // ... parse rows
    
    // Cache for 5 minutes
    gm.cache.SAdd(ctx, cacheKey, members)
    gm.cache.Expire(ctx, cacheKey, 5*time.Minute)
    
    return members, nil
}

// Handler modification for group messages
func (s *Server) handleGroupMessage(ctx context.Context, msg *ClientMessage) error {
    // 1. Validate group membership
    members, err := s.groupMgr.GetGroupMembers(ctx, msg.GroupID)
    if err != nil {
        return err
    }
    
    // 2. Check sender is member
    if !contains(members, msg.From) {
        return errors.New("sender not in group")
    }
    
    // 3. Set recipients (exclude sender)
    msg.To = filter(members, func(m string) bool { return m != msg.From })
    
    // 4. Process as multi-recipient message
    return s.handleMessage(ctx, msg)
}
```

### 6. Message Delivery Guarantees

**Acknowledgment flow:**

```
Client A                Gateway-01              Database              Gateway-02              Client B
   │                        │                       │                      │                      │
   │─── Send Message ──────▶│                       │                      │                      │
   │                        │─── Persist ──────────▶│                      │                      │
   │                        │◀──── Saved ───────────│                      │                      │
   │◀─── ACK: sent ─────────│                       │                      │                      │
   │                        │                       │                      │                      │
   │                        │───── Route to Gateway-02 ─────────────────▶│                      │
   │                        │                       │                      │─── Deliver ────────▶│
   │                        │                       │                      │◀─── ACK ────────────│
   │                        │◀──── Delivered ACK ───────────────────────────│                      │
   │◀─── Status: delivered ─│                       │                      │                      │
   │                        │─── Update status ────▶│                      │                      │
   │                        │                       │                      │                      │
   │                        │                       │                      │◀─── Read ───────────│
   │◀─── Status: read ──────│◀────────────── Read receipt ─────────────────│                      │
```

**Implementation:**

```go
// internal/gateway/ack_tracker.go
type AckTracker struct {
    store *MessageStore
}

func (at *AckTracker) MarkDelivered(ctx context.Context, messageID, recipientID string) error {
    _, err := at.store.db.Exec(ctx, `
        UPDATE message_recipients 
        SET status = 'delivered', delivered_at = NOW()
        WHERE message_id = $1 AND recipient_id = $2
    `, messageID, recipientID)
    
    // Send delivery receipt to sender
    go at.notifySender(ctx, messageID, recipientID, "delivered")
    
    return err
}

func (at *AckTracker) MarkRead(ctx context.Context, messageID, recipientID string) error {
    _, err := at.store.db.Exec(ctx, `
        UPDATE message_recipients 
        SET status = 'read', read_at = NOW()
        WHERE message_id = $1 AND recipient_id = $2
    `, messageID, recipientID)
    
    // Send read receipt to sender
    go at.notifySender(ctx, messageID, recipientID, "read")
    
    return err
}
```

## Scalability Considerations

### 1. Connection Distribution
- **Current:** ~10K connections per gateway
- **Target:** 1M+ connections per gateway with optimizations
- **Approach:** 
  - Use epoll/kqueue for efficient I/O
  - Reduce per-connection memory overhead
  - Implement connection pooling

### 2. Message Throughput
- **Target:** 1M messages/second globally
- **Approach:**
  - Kafka partitioning (1000+ partitions)
  - Database sharding by user_id
  - Batch processing for offline messages

### 3. Database Scaling
- **Partitioning:** By date (monthly partitions, auto-drop after 30 days)
- **Sharding:** By user_id for horizontal scaling
- **Replication:** Master-slave for read replicas

```sql
-- Automatic partition creation and cleanup
CREATE OR REPLACE FUNCTION create_monthly_partitions()
RETURNS void AS $$
DECLARE
    start_date DATE;
    end_date DATE;
    partition_name TEXT;
BEGIN
    FOR i IN 0..2 LOOP  -- Create 3 months ahead
        start_date := DATE_TRUNC('month', NOW() + (i || ' month')::INTERVAL);
        end_date := start_date + INTERVAL '1 month';
        partition_name := 'messages_' || TO_CHAR(start_date, 'YYYY_MM');
        
        EXECUTE format('
            CREATE TABLE IF NOT EXISTS %I PARTITION OF messages
            FOR VALUES FROM (%L) TO (%L)
        ', partition_name, start_date, end_date);
    END LOOP;
END;
$$ LANGUAGE plpgsql;

-- Auto-cleanup old partitions
CREATE OR REPLACE FUNCTION drop_old_partitions()
RETURNS void AS $$
DECLARE
    partition_name TEXT;
BEGIN
    FOR partition_name IN 
        SELECT tablename FROM pg_tables 
        WHERE schemaname = 'public' 
        AND tablename LIKE 'messages_%'
        AND tablename < 'messages_' || TO_CHAR(NOW() - INTERVAL '30 days', 'YYYY_MM')
    LOOP
        EXECUTE format('DROP TABLE IF EXISTS %I', partition_name);
    END LOOP;
END;
$$ LANGUAGE plpgsql;
```

### 4. Redis Optimization
- **Presence:** Use Redis Cluster with hash slots
- **Caching:** LRU eviction for group membership
- **TTL:** Aggressive expiration for stale data

### 5. Multi-Region Support
- **Active-Active:** Deploy gateways in multiple regions
- **Data locality:** Route users to nearest region
- **Cross-region:** Use Kafka MirrorMaker for federation

## Performance Targets

| Metric | Target | Measurement |
|--------|--------|-------------|
| Message delivery (online) | < 500ms | P99 latency |
| Message delivery (offline) | < 2s after connect | Time to first message |
| Throughput per gateway | 50K msgs/sec | Sustained load |
| Database write latency | < 50ms | P95 latency |
| Group fanout (100 users) | < 1s | Total delivery time |
| Connection capacity | 1M per gateway | With 8-core machine |

## Fault Tolerance

### 1. Gateway Failure
- **Detection:** Heartbeat timeout (30s)
- **Recovery:** Clients reconnect to another gateway
- **State:** No state loss (presence in Redis)

### 2. Database Failure
- **Master failure:** Automatic failover to replica
- **Replica failure:** Route reads to other replicas
- **Data loss:** WAL replication (RPO < 1 second)

### 3. Kafka Failure
- **Broker failure:** Automatic leader election
- **Message loss:** Replication factor 3
- **Consumer failure:** Consumer group rebalancing

### 4. Network Partition
- **Detection:** Split-brain prevention with consensus
- **Recovery:** Merge conflict resolution
- **Guarantee:** At-least-once delivery (deduplication by message_id)

## Migration Path from Current Demo

### Phase 1: Foundation (Week 1-2)
- [ ] Add PostgreSQL message persistence
- [ ] Implement message acknowledgments
- [ ] Add message_id for idempotency

### Phase 2: Offline Support (Week 3-4)
- [ ] Implement offline message queue
- [ ] Add delivery status tracking
- [ ] Build offline message processor

### Phase 3: Groups (Week 5-6)
- [ ] Add group tables and APIs
- [ ] Implement group message fanout
- [ ] Add group membership caching

### Phase 4: Media (Week 7-8)
- [ ] Integrate S3 for media storage
- [ ] Implement pre-signed URL generation
- [ ] Add CDN for media delivery

### Phase 5: Scale (Week 9-10)
- [ ] Database partitioning and sharding
- [ ] Load testing and optimization
- [ ] Multi-region deployment

### Phase 6: Production-Ready (Week 11-12)
- [ ] Monitoring and alerting
- [ ] Rate limiting and abuse prevention
- [ ] Documentation and runbooks

## Capacity Planning

### For 1 Billion Users
- **Assumptions:**
  - 10% daily active users (100M DAU)
  - Each user sends 50 messages/day
  - Average message size: 1KB
  - Peak traffic: 3x average

- **Requirements:**
  - **Messages/day:** 5 billion
  - **Messages/second (avg):** ~60K/sec
  - **Messages/second (peak):** ~180K/sec
  - **Storage/day:** 5TB
  - **Storage/month:** 150TB (before compression)

- **Infrastructure:**
  - **Gateways:** ~1000 instances (100K connections each)
  - **Kafka:** 100 brokers (1000 partitions)
  - **PostgreSQL:** 50 shards (20M users per shard)
  - **Redis:** 20 clusters (5M users per cluster)
  - **S3:** Unlimited (pay per GB)

## Cost Estimation (Monthly)

| Component | Units | Cost/Unit | Total |
|-----------|-------|-----------|-------|
| EC2 (Gateways) | 1000 × c6g.2xlarge | $100 | $100K |
| RDS (PostgreSQL) | 50 × db.r6g.4xlarge | $800 | $40K |
| ElastiCache (Redis) | 20 × cache.r6g.2xlarge | $400 | $8K |
| MSK (Kafka) | 100 brokers | $200 | $20K |
| S3 (Media) | 150TB | $0.023/GB | $3.5K |
| CloudFront (CDN) | 500TB egress | $0.085/GB | $42K |
| **Total** | | | **~$215K/month** |

**Cost per DAU:** $0.00215/user/month

## Next Steps

1. Review and validate design with stakeholders
2. Set up development environment
3. Begin Phase 1 implementation
4. Establish testing and monitoring infrastructure
5. Plan gradual rollout strategy
