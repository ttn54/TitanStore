package raft

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"strings"
	"time"
)

// TCPServer is a lightweight text-protocol server that lets external clients
// query and mutate the local node's state machine.
//
// Protocol (newline-terminated):
//
//	Request:  GET <key>\n
//	          SET <key> <value>\n
//	          DELETE <key>\n
//	Response: VALUE <value>\n        — GET found
//	          NOT_FOUND\n            — GET miss
//	          OK\n                   — SET/DELETE accepted by leader
//	          ERR NOT_LEADER <addr>\n — forwarded to leader address
//	          ERR <reason>\n         — malformed request
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

	const idleTimeout = 5 * time.Minute

	for {
		// Reset the read deadline before every blocking Scan call.
		conn.SetReadDeadline(time.Now().Add(idleTimeout))
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, " ", 3)
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
		case "SET":
			if len(parts) < 3 || strings.TrimSpace(parts[1]) == "" {
				response = "ERR usage: SET <key> <value>"
			} else {
				response = s.propose(fmt.Sprintf("SET %s %s", parts[1], parts[2]))
			}
		case "DELETE":
			if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
				response = "ERR usage: DELETE <key>"
			} else {
				response = s.propose(fmt.Sprintf("DELETE %s", strings.TrimSpace(parts[1])))
			}
		default:
			response = fmt.Sprintf("ERR unknown command %q", cmd)
		}

		fmt.Fprintf(writer, "%s\n", response)
		writer.Flush()
	}
	if err := scanner.Err(); err != nil {
		log.Printf("TCP client read error (%s): %v", conn.RemoteAddr(), err)
	}
}

// propose submits command to the Raft log if this node is leader.
// Returns "OK", "ERR NOT_LEADER <addr>", or "ERR NO_LEADER".
func (s *TCPServer) propose(command string) string {
	err := s.node.Propose(command)
	if err == nil {
		return "OK"
	}
	if addr, ok := s.node.GetLeaderClientAddr(); ok {
		return fmt.Sprintf("ERR NOT_LEADER %s", addr)
	}
	return "ERR NO_LEADER"
}
