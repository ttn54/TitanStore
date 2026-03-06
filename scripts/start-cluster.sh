#!/bin/bash
# Script to start a 3-node TitanStore cluster

echo "🚀 Starting TitanStore 3-Node Cluster..."

# Kill any existing processes on these ports
lsof -ti:5001,5002,5003,6001,6002,6003 | xargs kill -9 2>/dev/null

PEERS_NODE1="node2:localhost:5002:6002,node3:localhost:5003:6003"
PEERS_NODE2="node1:localhost:5001:6001,node3:localhost:5003:6003"
PEERS_NODE3="node1:localhost:5001:6001,node2:localhost:5002:6002"

# Start each node in background
echo "Starting node1 on gRPC :5001, TCP client :6001..."
go run cmd/server/main.go \
  -id=node1 -port=5001 -client-port=6001 \
  -advertise-client-addr=localhost:6001 \
  -peers="$PEERS_NODE1" > logs/node1.log 2>&1 &
NODE1_PID=$!

echo "Starting node2 on gRPC :5002, TCP client :6002..."
go run cmd/server/main.go \
  -id=node2 -port=5002 -client-port=6002 \
  -advertise-client-addr=localhost:6002 \
  -peers="$PEERS_NODE2" > logs/node2.log 2>&1 &
NODE2_PID=$!

echo "Starting node3 on gRPC :5003, TCP client :6003..."
go run cmd/server/main.go \
  -id=node3 -port=5003 -client-port=6003 \
  -advertise-client-addr=localhost:6003 \
  -peers="$PEERS_NODE3" > logs/node3.log 2>&1 &
NODE3_PID=$!

echo ""
echo "✅ Cluster started!"
echo "   Node 1 (PID $NODE1_PID): gRPC localhost:5001  TCP localhost:6001"
echo "   Node 2 (PID $NODE2_PID): gRPC localhost:5002  TCP localhost:6002"
echo "   Node 3 (PID $NODE3_PID): gRPC localhost:5003  TCP localhost:6003"
echo ""
echo "📝 Logs are in logs/ directory"
echo "🛑 To stop: ./scripts/stop-cluster.sh"
echo ""
echo "⏳ Waiting for leader election..."
sleep 3

# Tail all logs
echo "=== Recent Logs ==="
tail -n 5 logs/node*.log
