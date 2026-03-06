package raft

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// dialTCP retries until the server is ready.
func dialTCP(t *testing.T, addr string) net.Conn {
	t.Helper()
	var conn net.Conn
	var err error
	for i := 0; i < 10; i++ {
		conn, err = net.Dial("tcp", addr)
		if err == nil {
			return conn
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("could not connect to %s: %v", addr, err)
	return nil
}

func TestTCPServer_GET(t *testing.T) {
	node := NewRaftNode("tcp-test", map[string]string{})
	// Pre-populate the dataStore directly for the test
	node.dataStore["color"] = "blue"
	node.dataStore["count"] = "42"

	srv, err := NewTCPServer(node, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewTCPServer: %v", err)
	}
	defer srv.Close()
	go srv.Serve()

	addr := srv.listener.Addr().String()
	conn := dialTCP(t, addr)
	defer conn.Close()

	tests := []struct {
		send string
		want string
	}{
		{"GET color\n", "VALUE blue"},
		{"GET count\n", "VALUE 42"},
		{"GET missing\n", "NOT_FOUND"},
		{"GET\n", "ERR usage: GET <key>"},
		{"UNKNOWN cmd\n", `ERR unknown command "UNKNOWN"`},
	}

	reader := bufio.NewReader(conn)
	for _, tc := range tests {
		fmt.Fprint(conn, tc.send)
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading response for %q: %v", tc.send, err)
		}
		got := strings.TrimSpace(line)
		if got != tc.want {
			t.Errorf("send=%q: want %q, got %q", strings.TrimSpace(tc.send), tc.want, got)
		}
	}
	t.Log("✅ TCP GET handler working correctly")
}

// TestTCPServer_SET_Leader verifies that SET on a leader node returns OK.
func TestTCPServer_SET_Leader(t *testing.T) {
	node := NewRaftNode("leader-node", map[string]string{})
	// Manually promote to leader state so AppendEntry succeeds
	node.mu.Lock()
	node.state = Leader
	node.currentTerm = 1
	node.mu.Unlock()

	srv, err := NewTCPServer(node, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewTCPServer: %v", err)
	}
	defer srv.Close()
	go srv.Serve()

	conn := dialTCP(t, srv.listener.Addr().String())
	defer conn.Close()
	reader := bufio.NewReader(conn)

	// SET should return OK
	fmt.Fprint(conn, "SET city vancouver\n")
	resp, _ := reader.ReadString('\n')
	if strings.TrimSpace(resp) != "OK" {
		t.Fatalf("expected OK, got %q", strings.TrimSpace(resp))
	}

	// DELETE should return OK
	fmt.Fprint(conn, "DELETE city\n")
	resp, _ = reader.ReadString('\n')
	if strings.TrimSpace(resp) != "OK" {
		t.Fatalf("expected OK for DELETE, got %q", strings.TrimSpace(resp))
	}

	t.Log("✅ SET and DELETE on leader return OK")
}

// TestTCPServer_SET_Follower verifies that SET on a follower returns ERR NOT_LEADER.
func TestTCPServer_SET_Follower(t *testing.T) {
	node := NewRaftNode("follower-node", map[string]string{"leader-node": "localhost:5001"})
	// Stays as Follower with a known leader
	node.mu.Lock()
	node.leaderId = "leader-node"
	node.mu.Unlock()

	srv, err := NewTCPServer(node, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewTCPServer: %v", err)
	}
	defer srv.Close()
	go srv.Serve()

	conn := dialTCP(t, srv.listener.Addr().String())
	defer conn.Close()
	reader := bufio.NewReader(conn)

	fmt.Fprint(conn, "SET key val\n")
	resp, _ := reader.ReadString('\n')
	got := strings.TrimSpace(resp)
	if !strings.HasPrefix(got, "ERR NOT_LEADER") {
		t.Fatalf("expected ERR NOT_LEADER ..., got %q", got)
	}
	t.Logf("✅ Follower correctly redirects: %s", got)
}

// TestTCPServer_Redirect_ReturnsTCPAddr verifies that ERR NOT_LEADER returns the
// leader's TCP client address (not its gRPC address) so TitanSync can reconnect.
func TestTCPServer_Redirect_ReturnsTCPAddr(t *testing.T) {
	node := NewRaftNode("follower-node", map[string]string{"leader-node": "localhost:5001"})
	node.SetPeerClientAddr("leader-node", "localhost:6001")
	node.mu.Lock()
	node.leaderId = "leader-node"
	node.mu.Unlock()

	srv, err := NewTCPServer(node, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewTCPServer: %v", err)
	}
	defer srv.Close()
	go srv.Serve()

	conn := dialTCP(t, srv.listener.Addr().String())
	defer conn.Close()
	reader := bufio.NewReader(conn)

	fmt.Fprint(conn, "SET key val\n")
	resp, _ := reader.ReadString('\n')
	got := strings.TrimSpace(resp)
	// Must redirect to TCP client address, not gRPC address
	if got != "ERR NOT_LEADER localhost:6001" {
		t.Fatalf("expected ERR NOT_LEADER localhost:6001, got %q", got)
	}
	t.Log("✅ Redirect returns TCP client address")
}

// TestParsePeersFlag_Valid verifies correct parsing of the --peers flag format.
// Format: id:grpcHost:grpcPort:tcpPort,...
func TestParsePeersFlag_Valid(t *testing.T) {
	input := "node2:localhost:5002:6002,node3:localhost:5003:6003"
	grpcPeers, tcpPeers, err := ParsePeersFlag(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// gRPC peers
	if grpcPeers["node2"] != "localhost:5002" {
		t.Errorf("node2 gRPC: want localhost:5002, got %q", grpcPeers["node2"])
	}
	if grpcPeers["node3"] != "localhost:5003" {
		t.Errorf("node3 gRPC: want localhost:5003, got %q", grpcPeers["node3"])
	}

	// TCP client peers
	if tcpPeers["node2"] != "localhost:6002" {
		t.Errorf("node2 TCP: want localhost:6002, got %q", tcpPeers["node2"])
	}
	if tcpPeers["node3"] != "localhost:6003" {
		t.Errorf("node3 TCP: want localhost:6003, got %q", tcpPeers["node3"])
	}
	t.Log("✅ ParsePeersFlag correctly splits gRPC and TCP addresses")
}

// TestParsePeersFlag_Empty verifies that an empty string returns empty maps.
func TestParsePeersFlag_Empty(t *testing.T) {
	grpcPeers, tcpPeers, err := ParsePeersFlag("")
	if err != nil {
		t.Fatalf("unexpected error on empty input: %v", err)
	}
	if len(grpcPeers) != 0 || len(tcpPeers) != 0 {
		t.Fatalf("expected empty maps, got %v / %v", grpcPeers, tcpPeers)
	}
	t.Log("✅ Empty --peers flag handled correctly")
}

// TestParsePeersFlag_Malformed verifies that a bad entry returns an error.
func TestParsePeersFlag_Malformed(t *testing.T) {
	_, _, err := ParsePeersFlag("node2:localhost:5002") // missing TCP port field
	if err == nil {
		t.Fatal("expected error for malformed input, got nil")
	}
	t.Logf("✅ Malformed input rejected: %v", err)
}

// TestGetLeaderClientAddr_WithClientAddr verifies that GetLeaderClientAddr
// returns the TCP client address when one is registered for the leader.
func TestGetLeaderClientAddr_WithClientAddr(t *testing.T) {
	node := NewRaftNode("follower", map[string]string{"leader": "localhost:5001"})
	node.SetPeerClientAddr("leader", "localhost:6001")
	node.mu.Lock()
	node.leaderId = "leader"
	node.mu.Unlock()

	addr, ok := node.GetLeaderClientAddr()
	if !ok {
		t.Fatal("expected ok=true, got false")
	}
	if addr != "localhost:6001" {
		t.Errorf("want localhost:6001, got %q", addr)
	}
	t.Log("✅ GetLeaderClientAddr returns TCP address")
}

// TestGetLeaderClientAddr_FallbackToGRPC verifies that GetLeaderClientAddr falls
// back to the gRPC address when no TCP client address is registered.
func TestGetLeaderClientAddr_FallbackToGRPC(t *testing.T) {
	node := NewRaftNode("follower", map[string]string{"leader": "localhost:5001"})
	// No SetPeerClientAddr call — no TCP addr registered
	node.mu.Lock()
	node.leaderId = "leader"
	node.mu.Unlock()

	addr, ok := node.GetLeaderClientAddr()
	if !ok {
		t.Fatal("expected ok=true, got false")
	}
	if addr != "localhost:5001" {
		t.Errorf("want fallback localhost:5001, got %q", addr)
	}
	t.Log("✅ GetLeaderClientAddr falls back to gRPC address")
}
