# Distributed WebSocket Gateway Demo

A production-grade distributed WebSocket architecture demo in Go, demonstrating:

- **Multi-Gateway Architecture**: Multiple gateway instances with load balancing
- **Redis Presence Management**: User online status with CAS (Compare-And-Set) updates
- **Cross-Gateway Message Routing**: Redis Pub/Sub for inter-gateway communication
- **Heartbeat & Health Checks**: Automatic detection and cleanup of stale connections
- **Graceful Failure Recovery**: Clients automatically reconnect and recover state

## Architecture

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│  Client A   │     │  Client B   │     │  Client C   │
└──────┬──────┘     └──────┬──────┘     └──────┬──────┘
       │                   │                   │
       │ WebSocket         │ WebSocket         │ WebSocket
       │                   │                   │
       ▼                   ▼                   ▼
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│ Gateway-01  │────▶│ Gateway-02  │────▶│ Gateway-03  │
│  :8080      │     │  :8081      │     │  :8082      │
└─────┬───────┘     └─────┬───────┘     └─────┬───────┘
      │                   │                   │
      │         Redis Pub/Sub (Routing)       │
      └───────────────┬───────────────────────┘
                      │
                      ▼
              ┌──────────────┐
              │    Redis     │
              │  (Presence)  │
              └──────────────┘
```

### Key Components

1. **Connection Manager** (`internal/gateway/connection.go`)
   - Maintains bidirectional mappings: `userId ↔ connection`
   - Thread-safe with `sync.Map`
   - Automatic cleanup of stale connections

2. **Presence Manager** (`internal/presence/presence.go`)
   - Redis-backed user presence tracking
   - CAS updates with timestamp to handle race conditions
   - TTL-based automatic cleanup (90s, refreshed every 30s)

3. **Message Router** (`internal/router/router.go`)
   - Redis Pub/Sub for cross-gateway communication
   - Each gateway subscribes to `gateway:{gatewayId}` channel
   - Delivers messages to local connections only

4. **WebSocket Handler** (`internal/gateway/handler.go`)
   - User registration and authentication flow
   - Message routing logic
   - Heartbeat handling

## Quick Start

### Prerequisites

- Go 1.21+
- Docker & Docker Compose (for Redis)

### 1. Start Redis

```bash
docker-compose up -d
```

### 2. Build the Project

```bash
# Build gateway server
go build -o bin/gateway cmd/gateway/main.go

# Build client
go build -o bin/client cmd/client/main.go
```

### 3. Start Multiple Gateway Instances

Terminal 1:
```bash
./bin/gateway -id gateway-01 -port 8080
```

Terminal 2:
```bash
./bin/gateway -id gateway-02 -port 8081
```

Terminal 3:
```bash
./bin/gateway -id gateway-03 -port 8082
```

### 4. Connect Clients

Terminal 4 (User Alice):
```bash
./bin/client -user alice -gateway ws://localhost:8080/ws
```

Terminal 5 (User Bob):
```bash
./bin/client -user bob -gateway ws://localhost:8081/ws
```

Terminal 6 (User Charlie):
```bash
./bin/client -user charlie -gateway ws://localhost:8082/ws
```

### 5. Send Messages

In Alice's terminal:
```
> send bob Hello from Alice!
```

In Bob's terminal:
```
> send alice Hi Alice, I'm on a different gateway!
```

## Testing Cross-Gateway Routing

### Scenario 1: Basic Cross-Gateway Messaging

1. Alice connects to Gateway-01 (port 8080)
2. Bob connects to Gateway-02 (port 8081)
3. Alice sends message to Bob
4. Gateway-01 queries Redis for Bob's location
5. Gateway-01 publishes to `gateway:gateway-02` channel
6. Gateway-02 receives message and delivers to Bob

### Scenario 2: Gateway Failure Recovery

1. Alice connects to Gateway-01
2. Kill Gateway-01 process (Ctrl+C)
3. Client auto-detects disconnect
4. Restart client, it connects to Gateway-02
5. Alice's presence is updated in Redis
6. Messages now route to Gateway-02

Test this:
```bash
# Terminal 1: Start Alice on Gateway-01
./bin/client -user alice -gateway ws://localhost:8080/ws

# Terminal 2: Kill Gateway-01
# (Press Ctrl+C in the gateway terminal)

# Terminal 1: Restart Alice on Gateway-02
./bin/client -user alice -gateway ws://localhost:8081/ws

# Terminal 3: Bob sends message to Alice
./bin/client -user bob -gateway ws://localhost:8082/ws
> send alice Are you still there?

# Alice receives the message on Gateway-02!
```

## Message Protocol

### Client → Server

**Register:**
```json
{
  "type": "register",
  "userId": "alice"
}
```

**Heartbeat:**
```json
{
  "type": "ping"
}
```

**Send Message:**
```json
{
  "type": "message",
  "to": "bob",
  "content": "Hello Bob!"
}
```

### Server → Client

**Registration Confirmation:**
```json
{
  "type": "registered",
  "content": "Successfully registered"
}
```

**Incoming Message:**
```json
{
  "type": "message",
  "from": "alice",
  "content": "Hello Bob!"
}
```

**Error:**
```json
{
  "type": "error",
  "error": "User not found"
}
```

## Configuration

### Gateway Server Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-id` | (required) | Unique gateway identifier |
| `-port` | 8080 | HTTP/WebSocket port |
| `-redis` | localhost:6379 | Redis address |

### Client Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-user` | (required) | User ID |
| `-gateway` | ws://localhost:8080/ws | Gateway WebSocket URL |

### Timing Constants

| Constant | Value | Description |
|----------|-------|-------------|
| Heartbeat Interval | 30s | Client sends ping every 30s |
| Presence TTL | 90s | Redis key expires after 90s (3x heartbeat) |
| Health Check | 60s | Gateway scans for stale connections |

## Monitoring

### Gateway Stats Endpoint

```bash
curl http://localhost:8080/stats
```

Response:
```json
{
  "gatewayId": "gateway-01",
  "connections": 5
}
```

### Redis Presence Inspection

```bash
# Check user presence
redis-cli HGETALL presence:alice

# List all online users
redis-cli KEYS "presence:*"
```

## Advanced Scenarios

### Load Balancing with Nginx

`nginx.conf`:
```nginx
upstream websocket_gateways {
    least_conn;
    server localhost:8080;
    server localhost:8081;
    server localhost:8082;
}

server {
    listen 80;

    location /ws {
        proxy_pass http://websocket_gateways;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
    }
}
```

### Horizontal Scaling

Gateway instances are stateless (business logic), so you can:

1. Add more instances on-demand
2. Use autoscaling based on connection count
3. Deploy across multiple regions
4. No coordination needed between instances

## Design Highlights

### 1. CAS Updates in Presence

The Lua script ensures atomic updates with timestamp checking:

```lua
local current_ts = redis.call('HGET', key, 'ts')
if current_ts and tonumber(current_ts) > new_ts then
    return 0  -- Reject stale update
end
```

This prevents race conditions when:
- User disconnects from Gateway-01
- Immediately reconnects to Gateway-02
- Gateway-01's cleanup runs late

### 2. Bidirectional Connection Mapping

```go
userToConn sync.Map  // userId → *Connection
connToUser sync.Map  // connId → *Connection
```

Why both?
- `userToConn`: Fast message delivery lookup
- `connToUser`: Fast cleanup on disconnect

### 3. Local State = Ephemeral Cache

Gateway only stores:
- Active TCP connections (can't be shared)
- `userId → connection` mapping (rebuilds on reconnect)

All business state lives in Redis/DB.

## Troubleshooting

### Client can't connect

```bash
# Check gateway is running
curl http://localhost:8080/health

# Check Redis
docker ps | grep redis
redis-cli ping
```

### Messages not delivered

```bash
# Check user presence
redis-cli HGETALL presence:alice

# Check gateway logs for routing errors
# Look for "[Router] Routed message" logs
```

### Connections keep timing out

Increase heartbeat frequency or TTL in:
- `internal/gateway/handler.go`: `heartbeatInterval`, `heartbeatTimeout`
- `internal/presence/presence.go`: `presenceTTL`

## Project Structure

```
websocket-demo/
├── cmd/
│   ├── gateway/main.go        # Gateway server entry point
│   └── client/main.go         # Test client
├── internal/
│   ├── gateway/
│   │   ├── server.go          # HTTP server & lifecycle
│   │   ├── connection.go      # Connection management
│   │   └── handler.go         # WebSocket message handling
│   ├── presence/
│   │   └── presence.go        # Redis presence manager
│   └── router/
│       └── router.go          # Message routing (Pub/Sub)
├── docker-compose.yml         # Redis setup
├── go.mod
└── README.md
```

## License

MIT
