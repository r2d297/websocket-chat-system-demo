#!/bin/bash

# Distributed WebSocket Gateway Demo Test Script

set -e

echo "========================================="
echo "WebSocket Gateway Demo - Quick Test"
echo "========================================="
echo ""

# Check if Redis is running
echo "1. Checking Redis..."
if ! docker ps | grep -q redis; then
    echo "   Starting Redis..."
    docker-compose up -d
    sleep 3
else
    echo "   ✓ Redis is running"
fi

echo ""
echo "2. Building binaries..."
go build -o bin/gateway cmd/gateway/main.go
go build -o bin/client cmd/client/main.go
echo "   ✓ Build complete"

echo ""
echo "========================================="
echo "Demo Instructions:"
echo "========================================="
echo ""
echo "In separate terminals, run:"
echo ""
echo "Terminal 1 - Gateway 1:"
echo "  ./bin/gateway -id gateway-01 -port 8080"
echo ""
echo "Terminal 2 - Gateway 2:"
echo "  ./bin/gateway -id gateway-02 -port 8081"
echo ""
echo "Terminal 3 - Client Alice (connected to Gateway 1):"
echo "  ./bin/client -user alice -gateway ws://localhost:8080/ws"
echo ""
echo "Terminal 4 - Client Bob (connected to Gateway 2):"
echo "  ./bin/client -user bob -gateway ws://localhost:8081/ws"
echo ""
echo "========================================="
echo "Test Cross-Gateway Messaging:"
echo "========================================="
echo ""
echo "In Alice's terminal, type:"
echo "  send bob Hello from Alice!"
echo ""
echo "In Bob's terminal, type:"
echo "  send alice Hi Alice, I'm on a different gateway!"
echo ""
echo "You should see messages delivered across different gateways!"
echo ""
echo "========================================="
echo "Monitoring:"
echo "========================================="
echo ""
echo "Check gateway stats:"
echo "  curl http://localhost:8080/stats"
echo "  curl http://localhost:8081/stats"
echo ""
echo "Check Redis presence:"
echo "  redis-cli HGETALL presence:alice"
echo "  redis-cli HGETALL presence:bob"
echo ""
