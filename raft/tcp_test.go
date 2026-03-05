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
