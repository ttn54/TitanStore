#!/bin/bash
# Script to stop the TitanStore cluster

echo "🛑 Stopping TitanStore cluster..."

# Kill processes on cluster ports
lsof -ti:5001,5002,5003 | xargs kill -9 2>/dev/null

echo "✅ Cluster stopped!"
