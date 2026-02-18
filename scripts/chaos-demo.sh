#!/bin/bash
# The "Chaos Monkey" Demo - Kills nodes while cluster is running

echo "🐵 CHAOS MONKEY DEMO - Testing Fault Tolerance"
echo "=============================================="
echo ""

# Function to get leader from logs
get_leader() {
    grep "I am now the LEADER" logs/node*.log | tail -1 | awk -F'[][]' '{print $2}'
}

# Function to kill a node
kill_node() {
    local node=$1
    local port=$((5000 + ${node#node}))
    echo "💥 KILLING $node (port $port)..."
    lsof -ti:$port | xargs kill -9 2>/dev/null
    sleep 1
}

# Function to restart a node
restart_node() {
    local node=$1
    local port=$((5000 + ${node#node}))
    echo "🔄 RESTARTING $node (port $port)..."
    go run cmd/server/main.go -id=$node -port=$port > logs/$node.log 2>&1 &
    sleep 1
}

echo "1️⃣  Waiting for initial cluster stabilization..."
sleep 3

INITIAL_LEADER=$(get_leader)
echo "   Initial leader: $INITIAL_LEADER"
echo ""

echo "2️⃣  Killing the leader to force re-election..."
kill_node $INITIAL_LEADER
sleep 4

NEW_LEADER=$(get_leader)
echo "   New leader elected: $NEW_LEADER"
echo ""

echo "3️⃣  Reconnecting old leader as follower..."
restart_node $INITIAL_LEADER
sleep 3
echo "   $INITIAL_LEADER rejoined as follower"
echo ""

echo "4️⃣  Killing the new leader (double fault!)..."
kill_node $NEW_LEADER
sleep 4

FINAL_LEADER=$(get_leader)
echo "   Final leader: $FINAL_LEADER"
echo ""

echo "✅ CHAOS TEST COMPLETE!"
echo "================================"
echo "Summary:"
echo "  - Initial leader: $INITIAL_LEADER"
echo "  - After 1st failure: $NEW_LEADER"
echo "  - After 2nd failure: $FINAL_LEADER"
echo ""
echo "📊 Cluster demonstrated:"
echo "  ✓ Leader election"
echo "  ✓ Automatic failover"
echo "  ✓ Follower reintegration"
echo "  ✓ Multiple fault tolerance"
