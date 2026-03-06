package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	pb "titanstore/proto"
	"titanstore/raft"

	"google.golang.org/grpc"
)

func main() {
	nodeID := flag.String("id", "node1", "Node ID")
	port := flag.String("port", "5001", "gRPC port to listen on")
	clientPort := flag.String("client-port", "", "TCP client listener port (e.g. 6001); empty = disabled")
	advertiseClientAddr := flag.String("advertise-client-addr", "", "Advertised TCP client address for this node (e.g. localhost:6001)")
	peersFlag := flag.String("peers", "", "Comma-separated peer list: id:host:grpcPort:tcpPort,... (e.g. node2:localhost:5002:6002,node3:localhost:5003:6003)")
	flag.Parse()

	grpcPeers, tcpPeers, err := raft.ParsePeersFlag(*peersFlag)
	if err != nil {
		log.Fatalf("Invalid --peers flag: %v", err)
	}

	log.Printf("Starting TitanStore Raft Node: %s on port %s", *nodeID, *port)
	log.Printf("Peers: %v", grpcPeers)

	node := raft.NewRaftNode(*nodeID, grpcPeers)
	for id, addr := range tcpPeers {
		node.SetPeerClientAddr(id, addr)
	}

	// --- WAL boot sequence ---
	walPath := fmt.Sprintf("disk_%s.wal", *nodeID)
	wal, err := raft.NewFileWAL(walPath)
	if err != nil {
		log.Fatalf("Failed to open WAL at %s: %v", walPath, err)
	}
	node.SetWAL(wal)

	if err := node.RecoverFromWAL(); err != nil {
		log.Fatalf("WAL recovery failed: %v", err)
	}

	lis, err := net.Listen("tcp", ":"+*port)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterRaftServiceServer(grpcServer, node)

	node.Start()

	// --- TCP client listener (GET / SET / DELETE) ---
	var tcpSrv *raft.TCPServer
	if *clientPort != "" {
		var tcpErr error
		tcpSrv, tcpErr = raft.NewTCPServer(node, ":"+*clientPort)
		if tcpErr != nil {
			log.Fatalf("Failed to start TCP client listener: %v", tcpErr)
		}
		// Register this node's own TCP client address so peers can redirect to it.
		if *advertiseClientAddr != "" {
			node.SetPeerClientAddr(*nodeID, *advertiseClientAddr)
		}
		go tcpSrv.Serve()
	}

	go func() {
		log.Printf("gRPC server listening on :%s", *port)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("Failed to serve: %v", err)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\nShutting down gracefully...")
	grpcServer.GracefulStop()
	if tcpSrv != nil {
		tcpSrv.Close()
	}
	if err := wal.Close(); err != nil {
		log.Printf("WAL close error: %v", err)
	}
}
