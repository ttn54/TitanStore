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
	port := flag.String("port", "5001", "Port to listen on")
	clientPort := flag.String("client-port", "", "TCP client listener port (e.g. 6001); empty = disabled")
	flag.Parse()

	peers := make(map[string]string)
	allNodes := map[string]string{
		"node1": "localhost:5001",
		"node2": "localhost:5002",
		"node3": "localhost:5003",
	}

	for id, addr := range allNodes {
		if id != *nodeID {
			peers[id] = addr
		}
	}

	log.Printf("Starting TitanStore Raft Node: %s on port %s", *nodeID, *port)
	log.Printf("Peers: %v", peers)

	node := raft.NewRaftNode(*nodeID, peers)

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
