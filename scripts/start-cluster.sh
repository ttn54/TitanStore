#!/bin/bash
# Script to start a 3-node TitanStore cluster

echo "🚀 Starting TitanStore 3-Node Cluster..."

# Kill any existing processes on these ports
lsof -ti:5001,5002,5003 | xargs kill -9 2>/dev/null

# Start each node in background
echo "Starting node1 on port 5001..."
go run cmd/server/main.go -id=node1 -port=5001 > logs/node1.log 2>&1 &
NODE1_PID=$!

echo "Starting node2 on port 5002..."
go run cmd/server/main.go -id=node2 -port=5002 > logs/node2.log 2>&1 &
NODE2_PID=$!

echo "Starting node3 on port 5003..."
go run cmd/server/main.go -id=node3 -port=5003 > logs/node3.log 2>&1 &
NODE3_PID=$!

echo ""
echo "✅ Cluster started!"
echo "   Node 1 (PID $NODE1_PID): localhost:5001"
echo "   Node 2 (PID $NODE2_PID): localhost:5002"
echo "   Node 3 (PID $NODE3_PID): localhost:5003"
echo ""
echo "📝 Logs are in logs/ directory"
echo "🛑 To stop: ./scripts/stop-cluster.sh"
echo ""
echo "⏳ Waiting for leader election..."
sleep 3

# Tail all logs
echo "=== Recent Logs ==="
tail -n 5 logs/node*.log
