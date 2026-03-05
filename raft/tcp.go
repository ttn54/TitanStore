package raft

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"strings"
)

// TCPServer is a lightweight text-protocol server that lets external clients
// query the local node's state machine.
//
// Protocol (newline-terminated):
//
//	Request:  GET <key>\n
//	Response: VALUE <value>\n   — key found
//	          NOT_FOUND\n       — key absent
//	          ERR <reason>\n    — malformed request
type TCPServer struct {
	node     *RaftNode
	listener net.Listener
}

// NewTCPServer creates and binds the TCP listener on addr (e.g. ":6001").
func NewTCPServer(node *RaftNode, addr string) (*TCPServer, error) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &TCPServer{node: node, listener: lis}, nil
}

// Serve accepts connections in a loop. Call in a goroutine.
func (s *TCPServer) Serve() {
	log.Printf("TCP client listener on %s", s.listener.Addr())
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			// listener was closed — stop accepting
			return
		}
		go s.handleConn(conn)
	}
}

// Close shuts the TCP listener down.
func (s *TCPServer) Close() error {
	return s.listener.Close()
}

func (s *TCPServer) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	writer := bufio.NewWriter(conn)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, " ", 2)
		cmd := strings.ToUpper(parts[0])

		var response string
		switch cmd {
		case "GET":
			if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
				response = "ERR usage: GET <key>"
			} else {
				key := strings.TrimSpace(parts[1])
				if val, ok := s.node.GetValue(key); ok {
					response = fmt.Sprintf("VALUE %s", val)
				} else {
					response = "NOT_FOUND"
				}
			}
		default:
			response = fmt.Sprintf("ERR unknown command %q", cmd)
		}

		fmt.Fprintf(writer, "%s\n", response)
		writer.Flush()
	}
}
