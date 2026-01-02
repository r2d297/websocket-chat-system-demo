# WARP.md

This file provides guidance to WARP (warp.dev) when working with code in this repository.

## Common Commands

### Build Commands
```bash
# Build Redis Pub/Sub gateway (default)
go build -o bin/gateway cmd/gateway/main.go

# Build Kafka-based gateway
CGO_ENABLED=0 go build -o bin/gateway-kafka cmd/gateway-kafka/main.go

# Build client
go build -o bin/client cmd/client/main.go

# Build all binaries
go build -o bin/gateway cmd/gateway/main.go && \
go build -o bin/gateway-kafka cmd/gateway-kafka/main.go && \
go build -o bin/client cmd/client/main.go
```

### Running Services

#### Redis Pub/Sub Mode (Default)
```bash
# Start Redis
docker-compose up -d

# Start multiple gateway instances (in separate terminals)
./bin/gateway -id gateway-01 -port 8080
./bin/gateway -id gateway-02 -port 8081
./bin/gateway -id gateway-03 -port 8082

# Connect clients
./bin/client -user alice -gateway ws://localhost:8080/ws
./bin/client -user bob -gateway ws://localhost:8081/ws
```

#### Kafka Mode
```bash
# Start Kafka cluster (includes Kafka, ZooKeeper, Redis, and Kafka UI)
docker-compose -f docker-compose-kafka.yml up -d

# Initialize Kafka topics
./scripts/setup-kafka.sh

# Start Kafka gateways (in separate terminals)
./bin/gateway-kafka -id gateway-01 -port 8080 -redis localhost:6379 -kafka localhost:9092
./bin/gateway-kafka -id gateway-02 -port 8081 -redis localhost:6379 -kafka localhost:9092
./bin/gateway-kafka -id gateway-03 -port 8082 -redis localhost:6379 -kafka localhost:9092

# Access Kafka UI for monitoring
# Open browser: http://localhost:8090
```

### Testing
```bash
# Run automated demo setup (builds and provides instructions)
./test-demo.sh

# Run specific test scripts
go run test-redis.go        # Test Redis connectivity
go run test-messaging.go    # Test message routing
go run test-failover.go     # Test failover scenarios
```

### Monitoring & Debugging
```bash
# Check gateway health
curl http://localhost:8080/health
curl http://localhost:8081/health

# Get gateway statistics
curl http://localhost:8080/stats
curl http://localhost:8081/stats

# Inspect Redis presence data
redis-cli HGETALL presence:alice
redis-cli HGETALL presence:bob
redis-cli KEYS "presence:*"

# Monitor Kafka (when using Kafka mode)
docker exec websocket-kafka kafka-topics --list --bootstrap-server localhost:9092
docker exec websocket-kafka kafka-consumer-groups --describe --bootstrap-server localhost:9092 --group websocket-gateway
```

### Cleanup
```bash
# Stop and remove Redis containers
docker-compose down

# Stop and remove Kafka containers
docker-compose -f docker-compose-kafka.yml down

# Remove built binaries
rm -rf bin/
```

## Architecture Overview

### High-Level Design
This is a **distributed WebSocket gateway system** that enables real-time messaging across multiple gateway instances. Key characteristics:
- Multiple gateway instances can run simultaneously with no shared in-memory state
- Users connect to any gateway and can message users on any other gateway
- Gateway selection is transparent to users—messages route automatically
- Two routing implementations: Redis Pub/Sub (simpler) and Kafka (production-grade)

### Core Components

#### 1. Gateway Server (`internal/gateway/server.go`)
The main server lifecycle manager that:
- Orchestrates the router, connection manager, and presence manager
- Provides HTTP endpoints: `/ws` (WebSocket), `/health`, `/stats`
- Runs health checks every 60s to cleanup stale connections
- Supports pluggable routing via `RouterInterface`
- Two constructor patterns:
  - `NewServer()` - Creates gateway with Redis Pub/Sub router (default)
  - `NewServerWithRouter()` - Creates gateway with custom router (e.g., Kafka)

#### 2. Connection Manager (`internal/gateway/connection.go`)
Manages active WebSocket connections using **bidirectional mappings**:
- `userToConn sync.Map`: Fast lookup for message delivery (userId → Connection)
- `connToUser sync.Map`: Fast cleanup on disconnect (connId → Connection)
- All operations are thread-safe using `sync.Map`
- Each `Connection` tracks last ping time for health monitoring

**Why bidirectional?** When a message arrives for a user, we need O(1) lookup by userId. When a connection closes, we need O(1) cleanup by connId without iterating all connections.

#### 3. Presence Manager (`internal/presence/presence.go`)
Redis-backed user presence system that answers "where is user X?":
- Stores: `{userId → {gatewayId, connId, timestamp}}`
- Uses Lua script for **Compare-And-Set (CAS)** updates to handle race conditions
- TTL: 90s (3x heartbeat interval of 30s)
- Prevents stale updates: Rejects updates with older timestamps

**Critical for cross-gateway routing:** When Gateway-01 needs to send a message to a user, it queries presence to find which gateway owns that connection, then routes the message there.

**CAS importance:** Without CAS, this race occurs:
1. User disconnects from Gateway-01 (at T=100)
2. User reconnects to Gateway-02 (at T=101)
3. Gateway-01's cleanup runs late (at T=102) and overwrites Gateway-02's entry
4. Messages now route to dead Gateway-01

The Lua script rejects Gateway-01's stale update by comparing timestamps.

#### 4. Message Router (`internal/router/`)
Routes messages between gateway instances. **Two implementations via `RouterInterface`:**

**Redis Pub/Sub Router** (`router.go`):
- Each gateway subscribes to `gateway:{gatewayId}` channel
- To send to Gateway-02, publish to `gateway:gateway-02`
- Simple, low-latency, good for small-to-medium scale
- Pattern: Point-to-point via unique channels per gateway

**Kafka Router** (`kafka_router.go`):
- Each gateway subscribes to `gateway-{gatewayId}` topic (3 partitions)
- Uses hash partitioning on recipient userId to maintain message ordering
- Features: Message persistence, replay capability, higher throughput
- Better for production: handles backpressure, provides durability

**Routing flow:**
1. User sends message: `alice → bob`
2. Gateway-01 (alice's gateway) queries presence: "Where is bob?"
3. Presence responds: "bob is on Gateway-02"
4. Gateway-01 calls `router.RouteToGateway("gateway-02", message)`
5. Gateway-02 receives via Pub/Sub/Kafka and calls `deliverMessage()`
6. Gateway-02 delivers to bob's local WebSocket connection

#### 5. WebSocket Handler (`internal/gateway/handler.go`)
Implements the WebSocket protocol and business logic:

**Message types:**
- `register` - User registers with userId after connection
- `ping` - Heartbeat from client every 30s
- `pong` - Response to ping
- `message` - User-to-user message with `to` and `content`
- `error` - Server error response

**Registration flow:**
1. Client connects → receives random connId
2. Client sends `{"type":"register","userId":"alice"}`
3. Gateway adds to ConnectionManager
4. Gateway registers in Redis presence
5. Gateway starts heartbeat checker goroutine

**Message routing flow:**
1. Alice sends `{"type":"message","to":"bob","content":"hi"}`
2. Handler calls `routeMessage(alice, bob, hi)`
3. Query presence for bob → get target gatewayId
4. Call `router.RouteToGateway(targetGatewayId, message)`

**Heartbeat mechanism:**
- Client sends ping every 30s
- Gateway updates connection.LastPing and refreshes Redis TTL
- Separate goroutine checks if LastPing > 90s (timeout)
- If timeout, close connection and cleanup

### Data Flow for Cross-Gateway Messaging

**Scenario:** Alice (Gateway-01, port 8080) → Bob (Gateway-02, port 8081)

```
1. Alice's WebSocket → Gateway-01
   {"type":"message","to":"bob","content":"hello"}

2. Gateway-01 handler.go:routeMessage()
   - presence.Get("bob") → {gatewayId: "gateway-02"}

3. Gateway-01 router.RouteToGateway("gateway-02", msg)
   Redis: PUBLISH gateway:gateway-02 '{"from":"alice","to":"bob","content":"hello"}'
   OR
   Kafka: PRODUCE gateway-gateway-02 '{"from":"alice","to":"bob",...}'

4. Gateway-02 router.processMessages()
   - Receives message from Redis/Kafka subscription

5. Gateway-02 server.deliverMessage(msg)
   - connMgr.GetByUserID("bob") → find local connection
   - conn.Send('{"type":"message","from":"alice","content":"hello"}')

6. Bob's WebSocket ← Gateway-02
```

### Key Design Patterns

#### Stateless Business Logic
Gateways store only **ephemeral connection state** (WebSocket TCP connections + userId mappings). All business state lives in Redis:
- Presence: Redis hashes with TTL
- Connection ownership: Implied by presence
- No inter-gateway coordination needed

This enables:
- Horizontal scaling: Add more gateways anytime
- Fault tolerance: If gateway dies, users reconnect elsewhere
- Zero-config clustering: No leader election, no gossip protocol

#### Plugin Architecture
`RouterInterface` decouples routing from business logic:
```go
type RouterInterface interface {
    Start(ctx, handler) error
    RouteToGateway(ctx, gatewayId, msg) error
    BroadcastToAllGateways(ctx, msg) error
    Stop() error
}
```

To add new routing (e.g., NATS, RabbitMQ):
1. Implement `RouterInterface`
2. Use `NewServerWithRouter()`
3. Zero changes to gateway/handler/presence code

#### Graceful Degradation
- Heartbeat timeout (90s) is 3x heartbeat interval (30s) for network hiccups
- Health check loop (60s) provides background cleanup
- Redis presence TTL auto-expires dead connections
- Client implements reconnection logic

## Key Implementation Details

### Why sync.Map for ConnectionManager?
- Standard `map + sync.RWMutex` would serialize all reads
- `sync.Map` optimizes for read-heavy workloads (message delivery)
- Trade-off: Slightly more memory, much better concurrent reads

### Why Lua script for presence updates?
- Redis doesn't have native CAS primitives
- Lua script executes atomically on Redis server
- Alternative (WATCH/MULTI/EXEC) requires multiple round-trips and retry logic

### Gateway ID must be unique
Each gateway needs a unique ID for:
- Presence records (`presence:alice → {gatewayId: "gateway-01"}`)
- Router subscriptions (`gateway:gateway-01` channel/topic)

Use hostname, container ID, or explicit `-id` flag. Never reuse IDs across concurrent instances.

### Heartbeat timing constants
| Constant | Value | Purpose |
|----------|-------|---------|
| Client ping interval | 30s | Keep connection alive |
| Heartbeat timeout | 90s | Detect dead connections |
| Health check interval | 60s | Background cleanup |
| Presence TTL | 90s | Auto-cleanup dead presence |

Relationships: TTL = timeout = 3 × interval (allows 2 missed heartbeats)

## Project Context

### This is a demo/learning project
Focus is on demonstrating distributed systems concepts:
- Cross-gateway message routing
- Presence management with CAS
- Race condition handling
- Graceful failover

Not production-ready: Missing auth, rate limiting, metrics, observability, etc.

### Related documentation
- `README.md` - User-facing quick start and protocol docs
- `KAFKA_QUICKSTART.md` - Step-by-step Kafka setup
- `KAFKA_IMPLEMENTATION_SUMMARY.md` - Kafka vs Redis comparison
- `FAILOVER_TEST_RESULTS.md` - Failover testing scenarios
- `docs/0X-*.md` - Deep dives into each component (Chinese + English)

### Language notes
Code comments are bilingual (English / Chinese) to support Chinese-speaking contributors. When adding comments, follow the existing pattern:
```go
// English explanation
// 中文解释
```
