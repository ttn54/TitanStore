package raft

import (
	"fmt"
	"strings"
)

// ParsePeersFlag parses the --peers flag value into two maps:
//   - grpcAddrs: peerID → "host:grpcPort"
//   - tcpAddrs:  peerID → "host:tcpClientPort"
//
// Expected format (comma-separated):
//
//	node2:localhost:5002:6002,node3:localhost:5003:6003
//
// Each entry has four colon-separated fields: id, host, grpcPort, tcpPort.
// An empty string is valid and returns empty maps.
func ParsePeersFlag(flag string) (grpcAddrs map[string]string, tcpAddrs map[string]string, err error) {
	grpcAddrs = make(map[string]string)
	tcpAddrs = make(map[string]string)

	if strings.TrimSpace(flag) == "" {
		return
	}

	for _, entry := range strings.Split(flag, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// Split on ":" — expect exactly 4 fields: id, host, grpcPort, tcpPort
		parts := strings.SplitN(entry, ":", 4)
		if len(parts) != 4 {
			err = fmt.Errorf("peers: invalid entry %q: want id:host:grpcPort:tcpPort", entry)
			return nil, nil, err
		}
		id := parts[0]
		host := parts[1]
		grpcPort := parts[2]
		tcpPort := parts[3]

		if id == "" || host == "" || grpcPort == "" || tcpPort == "" {
			err = fmt.Errorf("peers: empty field in entry %q", entry)
			return nil, nil, err
		}

		grpcAddrs[id] = host + ":" + grpcPort
		tcpAddrs[id] = host + ":" + tcpPort
	}
	return
}
