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
