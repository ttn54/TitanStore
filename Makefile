.PHONY: all proto build test run clean cluster-start cluster-stop chaos

# Build everything
all: proto build

# Generate protobuf code
proto:
	@echo "📦 Generating protobuf code..."
	@mkdir -p proto
	protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		proto/raft.proto
	@echo "✅ Protobuf code generated"

# Build server binary
build:
	@echo "🔨 Building server..."
	go build -o bin/titanstore-server cmd/server/main.go
	@echo "✅ Server built: bin/titanstore-server"

# Run tests
test:
	@echo "🧪 Running tests..."
	go test -v -race ./raft/
	@echo "✅ Tests complete"

# Run tests with coverage
test-coverage:
	@echo "🧪 Running tests with coverage..."
	go test -v -race -coverprofile=coverage.out ./raft/
	go tool cover -html=coverage.out -o coverage.html
	@echo "✅ Coverage report: coverage.html"

# Start 3-node cluster
cluster-start:
	@mkdir -p logs
	@chmod +x scripts/start-cluster.sh
	@./scripts/start-cluster.sh

# Stop cluster
cluster-stop:
	@chmod +x scripts/stop-cluster.sh
	@./scripts/stop-cluster.sh

# Run chaos demo
chaos:
	@chmod +x scripts/chaos-demo.sh
	@./scripts/chaos-demo.sh

# Watch logs
logs:
	@tail -f logs/node*.log

# Clean build artifacts
clean:
	rm -rf bin/ logs/ coverage.out coverage.html
	@echo "✅ Cleaned"

# Install dependencies
deps:
	@echo "📥 Installing dependencies..."
	go mod download
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	@echo "✅ Dependencies installed"

# Quick start (proto + cluster)
quickstart: proto cluster-start
	@echo ""
	@echo "🎉 TitanStore is running!"
	@echo "   Run 'make logs' to watch logs"
	@echo "   Run 'make chaos' for chaos demo"
	@echo "   Run 'make cluster-stop' to stop"
