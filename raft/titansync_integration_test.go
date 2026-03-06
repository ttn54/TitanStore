package raft

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	pb "titanstore/proto"

	"google.golang.org/grpc"
)

// clusterNode bundles everything needed to run a single Raft node in a test.
type clusterNode struct {
	raft       *RaftNode
	grpcSrv    *grpc.Server
	tcpSrv     *TCPServer
	walPath    string
	snapPath   string
	tcpAddr    string // host:port of the TCP client listener
}

// stopClusterNode tears down the TCP server and gRPC server for a node,
// and signals the RaftNode's background goroutines to stop.
func stopClusterNode(cn *clusterNode) {
	cn.raft.Stop()
	if cn.tcpSrv != nil {
		cn.tcpSrv.Close()
	}
	if cn.grpcSrv != nil {
		cn.grpcSrv.Stop()
	}
}

// startClusterNode spins up a Raft node with a real gRPC server and TCP client
// listener. grpcAddr and tcpAddr must be available on the host (use :0 for
// OS-assigned ports, or fixed ports with enough separation from other tests).
// walPath / snapPath should be temp files managed by the caller.
func startClusterNode(
	t *testing.T,
	id string,
	grpcPeers map[string]string,
	grpcAddr, tcpAddr string,
	walPath, snapPath string,
) *clusterNode {
	t.Helper()

	wal, err := NewFileWAL(walPath)
	if err != nil {
		t.Fatalf("[%s] NewFileWAL: %v", id, err)
	}

	node := NewRaftNode(id, grpcPeers)
	node.SetWAL(wal)
	node.SetSnapshotPath(snapPath)

	if err := node.RecoverFromWAL(); err != nil {
		t.Fatalf("[%s] RecoverFromWAL: %v", id, err)
	}

	// gRPC server
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		t.Fatalf("[%s] gRPC listen %s: %v", id, grpcAddr, err)
	}
	grpcSrv := grpc.NewServer()
	pb.RegisterRaftServiceServer(grpcSrv, node)
	go grpcSrv.Serve(lis)

	// TCP client server
	tcpSrv, err := NewTCPServer(node, tcpAddr)
	if err != nil {
		grpcSrv.Stop()
		t.Fatalf("[%s] NewTCPServer %s: %v", id, tcpAddr, err)
	}
	go tcpSrv.Serve()

	node.Start()
	time.Sleep(50 * time.Millisecond) // let server goroutines initialise

	return &clusterNode{
		raft:     node,
		grpcSrv:  grpcSrv,
		tcpSrv:   tcpSrv,
		walPath:  walPath,
		snapPath: snapPath,
		tcpAddr:  tcpSrv.listener.Addr().String(),
	}
}

// sendTCP sends one newline-terminated command to addr and returns the trimmed response.
func sendTCP(t *testing.T, addr, cmd string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("sendTCP dial %s: %v", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	fmt.Fprintf(conn, "%s\n", cmd)
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("sendTCP read from %s: %v", addr, err)
	}
	return strings.TrimSpace(line)
}

// findLeaderNode returns the clusterNode that is currently the Raft leader,
// polling up to timeout. Fails the test if no leader emerges in time.
func findLeaderNode(t *testing.T, nodes []*clusterNode, timeout time.Duration) *clusterNode {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, cn := range nodes {
			if cn.raft.IsLeader() {
				return cn
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no leader elected within timeout")
	return nil
}

// TestTitanSync_Integration is the end-to-end smoke test that validates
// the full TitanStore API as TitanSync would use it:
//
//  1. Three-node cluster starts and elects a leader.
//  2. A JSON payload is written via TCP SET to the leader.
//  3. The value is readable from all nodes via TCP GET (after replication).
//  4. The leader is killed — the remaining two nodes elect a new leader.
//  5. The JSON payload is still readable from the new leader via TCP GET.
//  6. A third node restarts from its WAL — the key is present after recovery.
func TestTitanSync_Integration(t *testing.T) {
	// --- temp files ---
	makeTemp := func(suffix string) string {
		f, err := os.CreateTemp("", "titansync-smoke-*"+suffix)
		if err != nil {
			t.Fatalf("temp file: %v", err)
		}
		f.Close()
		return f.Name()
	}

	wal1, wal2, wal3 := makeTemp(".wal"), makeTemp(".wal"), makeTemp(".wal")
	snap1, snap2, snap3 := wal1+".snap", wal2+".snap", wal3+".snap"
	defer func() {
		for _, p := range []string{wal1, wal2, wal3, snap1, snap2, snap3} {
			os.Remove(p)
		}
	}()

	// --- build peer maps using OS-assigned ports to avoid conflicts ---
	// We use :0 for everything and extract the real addresses after binding.
	// gRPC peers must be known before starting, so we pre-bind the listeners.
	grpcLis := func(t *testing.T) net.Listener {
		t.Helper()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("pre-bind: %v", err)
		}
		return l
	}

	l1, l2, l3 := grpcLis(t), grpcLis(t), grpcLis(t)
	addr1, addr2, addr3 := l1.Addr().String(), l2.Addr().String(), l3.Addr().String()
	// Close listeners — startClusterNodeFromListener will rebind them through grpc.NewServer.
	// Actually we pass them directly to avoid TOCTOU. Use a variant that accepts a pre-bound listener.
	l1.Close()
	l2.Close()
	l3.Close()

	peers1 := map[string]string{"n2": addr2, "n3": addr3}
	peers2 := map[string]string{"n1": addr1, "n3": addr3}
	peers3 := map[string]string{"n1": addr1, "n2": addr2}

	cn1 := startClusterNode(t, "n1", peers1, addr1, "127.0.0.1:0", wal1, snap1)
	cn2 := startClusterNode(t, "n2", peers2, addr2, "127.0.0.1:0", wal2, snap2)
	cn3 := startClusterNode(t, "n3", peers3, addr3, "127.0.0.1:0", wal3, snap3)
	defer stopClusterNode(cn1)
	defer stopClusterNode(cn2)
	defer stopClusterNode(cn3)

	allNodes := []*clusterNode{cn1, cn2, cn3}

	// --- Step 1: wait for leader election ---
	t.Log("Step 1: waiting for leader election...")
	leader := findLeaderNode(t, allNodes, 5*time.Second)
	t.Logf("  leader elected: %s", leader.raft.id)

	// --- Step 2: write JSON payload via TCP (as TitanSync would) ---
	const jsonPayload = `{"name":"zen","ts":1741276800}`
	t.Log("Step 2: writing JSON payload via TCP SET...")
	resp := sendTCP(t, leader.tcpAddr, "SET myfile "+jsonPayload)
	if resp != "OK" {
		t.Fatalf("SET myfile: want OK, got %q", resp)
	}

	// Allow replication to all followers
	time.Sleep(300 * time.Millisecond)

	// --- Step 3: verify all nodes return the value via GET ---
	t.Log("Step 3: verifying GET from all nodes...")
	for _, cn := range allNodes {
		got := sendTCP(t, cn.tcpAddr, "GET myfile")
		want := "VALUE " + jsonPayload
		if got != want {
			t.Errorf("[%s] GET myfile: want %q, got %q", cn.raft.id, want, got)
		}
	}
	t.Log("  all nodes agree on the value ✅")

	// --- Step 4: kill the leader ---
	t.Logf("Step 4: killing leader %s...", leader.raft.id)
	stopClusterNode(leader)

	remaining := make([]*clusterNode, 0, 2)
	for _, cn := range allNodes {
		if cn != leader {
			remaining = append(remaining, cn)
		}
	}

	// --- Step 5: wait for new leader from remaining nodes ---
	t.Log("Step 5: waiting for new leader from remaining nodes...")
	newLeader := findLeaderNode(t, remaining, 5*time.Second)
	t.Logf("  new leader: %s", newLeader.raft.id)

	// --- Step 6: verify data survived leader failover ---
	t.Log("Step 6: verifying data survives leader failover...")
	got := sendTCP(t, newLeader.tcpAddr, "GET myfile")
	want := "VALUE " + jsonPayload
	if got != want {
		t.Fatalf("GET after failover: want %q, got %q", want, got)
	}
	t.Log("  data intact after leader failover ✅")

	// --- Step 7: WAL recovery check — restart a follower from its WAL ---
	// Find a surviving follower (not the new leader)
	var follower *clusterNode
	for _, cn := range remaining {
		if cn != newLeader {
			follower = cn
			break
		}
	}
	if follower == nil {
		t.Fatal("could not find a follower node for recovery check")
	}

	t.Logf("Step 7: WAL recovery check — bouncing follower %s...", follower.raft.id)
	stopClusterNode(follower)

	// Determine which peer map to use for the restarted node
	var recoverPeers map[string]string
	switch follower.raft.id {
	case "n1":
		recoverPeers = peers1
	case "n2":
		recoverPeers = peers2
	default:
		recoverPeers = peers3
	}

	// Restart from the same WAL and snapshot files on a fresh port.
	restarted := startClusterNode(t,
		follower.raft.id,
		recoverPeers,
		"127.0.0.1:0", // fresh OS-assigned port
		"127.0.0.1:0",
		follower.walPath,
		follower.snapPath,
	)
	defer stopClusterNode(restarted)

	time.Sleep(300 * time.Millisecond)

	got = sendTCP(t, restarted.tcpAddr, "GET myfile")
	if got != want {
		t.Fatalf("GET after WAL recovery: want %q, got %q", want, got)
	}
	t.Log("  data intact after WAL recovery ✅")
	t.Log("✅ TitanSync integration smoke test PASSED")
}
