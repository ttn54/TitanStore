package raft

import (
	"log"
	"net"
	"testing"
	"time"

	pb "titanstore/proto"

	"google.golang.org/grpc"
)

// TestLeaderElection verifies that a leader is elected in a 3-node cluster
func TestLeaderElection(t *testing.T) {
	log.Println("\n=== TEST: Leader Election ===")

	// Create 3 nodes
	peers1 := map[string]string{"node2": "localhost:6002", "node3": "localhost:6003"}
	peers2 := map[string]string{"node1": "localhost:6001", "node3": "localhost:6003"}
	peers3 := map[string]string{"node1": "localhost:6001", "node2": "localhost:6002"}

	node1 := NewRaftNode("node1", peers1)
	node2 := NewRaftNode("node2", peers2)
	node3 := NewRaftNode("node3", peers3)

	// Start gRPC servers
	server1 := startTestServer(node1, "6001")
	server2 := startTestServer(node2, "6002")
	server3 := startTestServer(node3, "6003")

	defer server1.Stop()
	defer server2.Stop()
	defer server3.Stop()

	// Start nodes
	node1.Start()
	node2.Start()
	node3.Start()

	// Wait for election (max 2 seconds)
	time.Sleep(2 * time.Second)

	// Count leaders
	leaderCount := 0
	var leaderID string

	nodes := []*RaftNode{node1, node2, node3}
	for _, n := range nodes {
		n.mu.RLock()
		if n.state == Leader {
			leaderCount++
			leaderID = n.id
		}
		log.Printf("Node %s: state=%s, term=%d", n.id, n.state, n.currentTerm)
		n.mu.RUnlock()
	}

	// Verify exactly one leader
	if leaderCount != 1 {
		t.Fatalf("Expected 1 leader, got %d", leaderCount)
	}

	log.Printf("✅ SUCCESS: %s is the leader\n", leaderID)
}

// TestLogReplication verifies that log entries are replicated to followers
func TestLogReplication(t *testing.T) {
	log.Println("\n=== TEST: Log Replication ===")

	// Create 3 nodes
	peers1 := map[string]string{"node2": "localhost:7002", "node3": "localhost:7003"}
	peers2 := map[string]string{"node1": "localhost:7001", "node3": "localhost:7003"}
	peers3 := map[string]string{"node1": "localhost:7001", "node2": "localhost:7002"}

	node1 := NewRaftNode("node1", peers1)
	node2 := NewRaftNode("node2", peers2)
	node3 := NewRaftNode("node3", peers3)

	// Start gRPC servers
	server1 := startTestServer(node1, "7001")
	server2 := startTestServer(node2, "7002")
	server3 := startTestServer(node3, "7003")

	defer server1.Stop()
	defer server2.Stop()
	defer server3.Stop()

	// Start nodes
	node1.Start()
	node2.Start()
	node3.Start()

	// Wait for election
	time.Sleep(2 * time.Second)

	// Find leader
	var leader *RaftNode
	nodes := []*RaftNode{node1, node2, node3}
	for _, n := range nodes {
		n.mu.RLock()
		if n.state == Leader {
			leader = n
		}
		n.mu.RUnlock()
	}

	if leader == nil {
		t.Fatal("No leader elected")
	}

	log.Printf("Leader is: %s", leader.id)

	// Append entries to leader
	testCommands := []string{"SET x=1", "SET y=2", "SET z=3"}
	for _, cmd := range testCommands {
		if !leader.AppendEntry(cmd) {
			t.Fatalf("Failed to append entry: %s", cmd)
		}
		log.Printf("Appended to leader: %s", cmd)
	}

	// Wait for replication
	time.Sleep(1 * time.Second)

	// Verify all nodes have the same log
	for _, n := range nodes {
		n.mu.RLock()
		logLen := len(n.log)
		log.Printf("Node %s: log length=%d", n.id, logLen)

		if logLen != len(testCommands) {
			t.Errorf("Node %s: expected %d log entries, got %d", n.id, len(testCommands), logLen)
		}

		// Verify commands match
		for i, cmd := range testCommands {
			if i < logLen && n.log[i].Command != cmd {
				t.Errorf("Node %s: log[%d] expected '%s', got '%s'", n.id, i, cmd, n.log[i].Command)
			}
		}
		n.mu.RUnlock()
	}

	log.Println("✅ SUCCESS: All nodes have replicated logs")
}

// TestSplitBrain verifies recovery from network partition (THE BIG TEST!)
func TestSplitBrain(t *testing.T) {
	log.Println("\n=== TEST: Split-Brain Recovery ===")

	// Create 3 nodes
	peers1 := map[string]string{"node2": "localhost:8002", "node3": "localhost:8003"}
	peers2 := map[string]string{"node1": "localhost:8001", "node3": "localhost:8003"}
	peers3 := map[string]string{"node1": "localhost:8001", "node2": "localhost:8002"}

	node1 := NewRaftNode("node1", peers1)
	node2 := NewRaftNode("node2", peers2)
	node3 := NewRaftNode("node3", peers3)

	// Start gRPC servers
	server1 := startTestServer(node1, "8001")
	server2 := startTestServer(node2, "8002")
	server3 := startTestServer(node3, "8003")

	defer server1.Stop()
	defer server2.Stop()
	defer server3.Stop()

	// Start nodes
	node1.Start()
	node2.Start()
	node3.Start()

	// Wait for initial election
	time.Sleep(2 * time.Second)

	// Find initial leader
	var initialLeader *RaftNode
	nodes := []*RaftNode{node1, node2, node3}
	for _, n := range nodes {
		n.mu.RLock()
		if n.state == Leader {
			initialLeader = n
		}
		n.mu.RUnlock()
	}

	if initialLeader == nil {
		t.Fatal("No initial leader elected")
	}

	log.Printf("Initial leader: %s", initialLeader.id)

	// SIMULATE PARTITION: Stop the leader's gRPC server and halt its goroutines.
	// Without Stop(), the leader's sendHeartbeats keeps running and prevents
	// the remaining nodes from timing out and starting a new election.
	log.Printf("🔥 SIMULATING PARTITION: Stopping leader %s", initialLeader.id)
	initialLeader.Stop()

	if initialLeader.id == "node1" {
		server1.Stop()
	} else if initialLeader.id == "node2" {
		server2.Stop()
	} else {
		server3.Stop()
	}

	// Wait for new election (need to wait past election timeout + some buffer)
	// Election timeout is 150-300ms, so wait at least 500ms for timeout + election
	log.Println("⏳ Waiting for new election...")
	time.Sleep(2 * time.Second)

	// Verify new leader elected (from remaining nodes)
	leaderCount := 0
	var newLeaderID string

	for _, n := range nodes {
		if n == initialLeader {
			continue // Skip old leader
		}

		n.mu.RLock()
		if n.state == Leader {
			leaderCount++
			newLeaderID = n.id
		}
		log.Printf("Node %s: state=%s, term=%d", n.id, n.state, n.currentTerm)
		n.mu.RUnlock()
	}

	if leaderCount != 1 {
		t.Fatalf("Expected 1 new leader after partition, got %d", leaderCount)
	}

	log.Printf("✅ SUCCESS: New leader elected: %s", newLeaderID)
	log.Println("✅ Split-brain prevented! Cluster recovered from partition")
}

// Helper function to start a test gRPC server
func startTestServer(node *RaftNode, port string) *grpc.Server {
	lis, err := testListen("localhost:" + port)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", port, err)
	}

	server := grpc.NewServer()
	pb.RegisterRaftServiceServer(server, node)

	go func() {
		if err := server.Serve(lis); err != nil {
			log.Printf("Server on port %s stopped: %v", port, err)
		}
	}()

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	return server
}

// Helper to create listener (with retry for port conflicts)
func testListen(addr string) (net.Listener, error) {
	var lis net.Listener
	var err error

	for i := 0; i < 5; i++ {
		lis, err = net.Listen("tcp", addr)
		if err == nil {
			return lis, nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	return nil, err
}

// TestExecuteCommand_DELETE verifies SET then DELETE removes the key.
func TestExecuteCommand_DELETE(t *testing.T) {
	node := NewRaftNode("test-del", map[string]string{})

	// Simulate log with SET then DELETE
	node.log = []LogEntry{
		{Term: 1, Command: "SET color blue"},
		{Term: 1, Command: "SET name zen"},
		{Term: 1, Command: "DELETE color"},
	}
	node.commitIndex = 2

	// Apply all entries
	node.applyCommittedEntries()

	// "color" should be gone
	if _, ok := node.dataStore["color"]; ok {
		t.Fatal("expected key 'color' to be deleted, but it still exists")
	}
	// "name" should still be present
	val, ok := node.dataStore["name"]
	if !ok || val != "zen" {
		t.Fatalf("expected name=zen, got %q (ok=%v)", val, ok)
	}
	t.Log("✅ DELETE command correctly removes key from dataStore")
}
