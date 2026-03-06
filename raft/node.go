package raft

import (
	"context"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

	pb "titanstore/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// NodeState represents the three possible states in Raft
type NodeState int

const (
	Follower NodeState = iota
	Candidate
	Leader
)

func (s NodeState) String() string {
	switch s {
	case Follower:
		return "FOLLOWER"
	case Candidate:
		return "CANDIDATE"
	case Leader:
		return "LEADER"
	default:
		return "UNKNOWN"
	}
}

// LogEntry represents a command in the replicated log
type LogEntry struct {
	Term    int32
	Command string
}

// RaftNode represents a single node in the Raft cluster
type RaftNode struct {
	pb.UnimplementedRaftServiceServer

	// Persistent state (would be on disk in production)
	mu          sync.RWMutex
	id          string
	currentTerm int32
	votedFor    string
	log         []LogEntry

	// Volatile state on all servers
	commitIndex int32
	lastApplied int32
	state       NodeState

	// Volatile state on leaders
	nextIndex  map[string]int32
	matchIndex map[string]int32

	// Cluster configuration
	peers           map[string]string
	peerClientAddrs map[string]string // TCP client address per peer (id → host:port)

	// Election timer tracking
	lastHeartbeat time.Time
	dataStore     map[string]string
	leaderId      string

	// Persistence
	wal          WAL
	snapshotPath string
	snapshotIndex int32
	snapshotTerm  int32

	// Lifecycle
	stopCh chan struct{} // closed by Stop() to halt background goroutines
}

// NewRaftNode creates a new Raft node
func NewRaftNode(id string, peers map[string]string) *RaftNode {
	node := &RaftNode{
		id:            id,
		currentTerm:   0,
		votedFor:      "",
		log:           make([]LogEntry, 0),
		commitIndex:   -1,
		lastApplied:   -1,
		state:         Follower,
		nextIndex:     make(map[string]int32),
		matchIndex:    make(map[string]int32),
		peers:           peers,
		peerClientAddrs: make(map[string]string),
		dataStore:       make(map[string]string),
		leaderId:        "",
		lastHeartbeat:   time.Now(),
		snapshotIndex:   -1,
		stopCh:          make(chan struct{}),
	}

	return node
}

// SetWAL attaches a Write-Ahead Log to the node.
// Must be called before Start().
func (rn *RaftNode) SetWAL(w WAL) {
	rn.wal = w
}

// SetSnapshotPath sets the file path used by TakeSnapshot and RecoverFromWAL.
// Must be called before Start().
func (rn *RaftNode) SetSnapshotPath(path string) {
	rn.snapshotPath = path
}

// TakeSnapshot serialises the current state machine to disk, then truncates
// the WAL so it only retains the current term/vote record.
// Must NOT be called while rn.mu is held by the caller.
func (rn *RaftNode) TakeSnapshot() error {
	rn.mu.Lock()

	if rn.snapshotPath == "" {
		rn.mu.Unlock()
		return nil
	}

	// Copy all state needed for the snapshot while holding the lock.
	snap := Snapshot{
		SnapshotIndex: rn.commitIndex,
		SnapshotTerm:  rn.currentTerm,
		CurrentTerm:   rn.currentTerm,
		VotedFor:      rn.votedFor,
		DataStore:     make(map[string]string, len(rn.dataStore)),
	}
	for k, v := range rn.dataStore {
		snap.DataStore[k] = v
	}
	if rn.commitIndex >= 0 && int(rn.commitIndex) < len(rn.log) {
		snap.SnapshotTerm = rn.log[rn.commitIndex].Term
	}

	rn.mu.Unlock()

	// Write snapshot atomically (temp + rename) — outside the lock is safe
	// because snapshotPath is immutable after SetSnapshotPath.
	if err := WriteSnapshot(rn.snapshotPath, snap); err != nil {
		return err
	}

	// Truncate WAL and re-persist term/vote as the sole surviving record.
	if rn.wal != nil {
		if err := rn.wal.Truncate(); err != nil {
			return err
		}
		if err := rn.wal.AppendTermVote(snap.CurrentTerm, snap.VotedFor); err != nil {
			return err
		}
	}

	rn.mu.Lock()
	rn.snapshotIndex = snap.SnapshotIndex
	rn.snapshotTerm = snap.SnapshotTerm
	rn.mu.Unlock()

	log.Printf("[%s] Snapshot taken at index=%d term=%d (%d keys)",
		rn.id, snap.SnapshotIndex, snap.SnapshotTerm, len(snap.DataStore))
	return nil
}

// SetPeerClientAddr registers the TCP client address for a peer node.
// Must be called before Start(). Safe to call multiple times.
func (rn *RaftNode) SetPeerClientAddr(peerID, addr string) {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	rn.peerClientAddrs[peerID] = addr
}

// GetLeaderClientAddr returns the TCP client address of the known leader.
// Falls back to the gRPC peer address when no TCP address is registered.
// Returns ("", false) when the leader is unknown.
func (rn *RaftNode) GetLeaderClientAddr() (string, bool) {
	rn.mu.RLock()
	defer rn.mu.RUnlock()
	if rn.leaderId == "" {
		return "", false
	}
	if addr, ok := rn.peerClientAddrs[rn.leaderId]; ok {
		return addr, true
	}
	// Fallback: return gRPC address so old behaviour is preserved.
	if addr, ok := rn.peers[rn.leaderId]; ok {
		return addr, true
	}
	return "", false
}

// persistTermVote writes the current term and votedFor to the WAL.
// MUST be called while rn.mu is held (Lock, not RLock).
// Errors are logged but not fatal — the in-memory state is already mutated.
func (rn *RaftNode) persistTermVote() {
	if rn.wal == nil {
		return
	}
	if err := rn.wal.AppendTermVote(rn.currentTerm, rn.votedFor); err != nil {
		log.Printf("[%s] WAL term/vote write failed (term=%d voted=%q): %v",
			rn.id, rn.currentTerm, rn.votedFor, err)
	}
}

// GetValue returns the value for key from the in-memory state machine.
// Safe for concurrent reads.
func (rn *RaftNode) GetValue(key string) (string, bool) {
	rn.mu.RLock()
	defer rn.mu.RUnlock()
	val, ok := rn.dataStore[key]
	return val, ok
}

// GetLeaderAddr returns the gRPC address of the known leader, or ("", false)
// when the leader is unknown. Safe for concurrent reads.
func (rn *RaftNode) GetLeaderAddr() (string, bool) {
	rn.mu.RLock()
	defer rn.mu.RUnlock()
	if rn.leaderId == "" {
		return "", false
	}
	addr, ok := rn.peers[rn.leaderId]
	if !ok {
		// This node itself is the leader
		addr = rn.peers[rn.id]
	}
	return addr, rn.leaderId != ""
}

// IsLeader reports whether this node is currently the Raft leader.
func (rn *RaftNode) IsLeader() bool {
	rn.mu.RLock()
	defer rn.mu.RUnlock()
	return rn.state == Leader
}

// RecoverFromWAL rebuilds rn.log, rn.dataStore, currentTerm, and votedFor.
// If a snapshot file exists it is loaded first; only WAL entries after
// snapshotIndex are then replayed on top.
// Must be called after SetWAL (and SetSnapshotPath if using snapshots) and before Start().
func (rn *RaftNode) RecoverFromWAL() error {
	// --- Step 1: load snapshot if one exists ---
	if rn.snapshotPath != "" {
		snap, err := ReadSnapshot(rn.snapshotPath)
		if err != nil {
			return err
		}
		if snap != nil {
			rn.snapshotIndex = snap.SnapshotIndex
			rn.snapshotTerm = snap.SnapshotTerm
			rn.currentTerm = snap.CurrentTerm
			rn.votedFor = snap.VotedFor
			rn.commitIndex = snap.SnapshotIndex
			rn.lastApplied = snap.SnapshotIndex
			for k, v := range snap.DataStore {
				rn.dataStore[k] = v
			}
			log.Printf("[%s] Snapshot loaded: index=%d term=%d %d keys",
				rn.id, snap.SnapshotIndex, snap.SnapshotTerm, len(snap.DataStore))
		}
	}

	if rn.wal == nil {
		return nil
	}

	// --- Step 2: replay WAL records that follow the snapshot ---
	records, err := rn.wal.ReadAll()
	if err != nil {
		return err
	}
	if len(records) == 0 {
		log.Printf("[%s] WAL empty — nothing to replay", rn.id)
		return nil
	}

	log.Printf("[%s] WAL found %d records — replaying...", rn.id, len(records))

	lastCommitIndex := rn.commitIndex
	lastTerm := rn.currentTerm
	lastVotedFor := rn.votedFor

	for _, rec := range records {
		switch rec.Type {
		case RecordTypeEntry:
			// Skip entries already covered by the snapshot.
			if rec.Index <= rn.snapshotIndex {
				continue
			}
			entry := LogEntry{Term: rec.Term, Command: rec.Command}
			for int32(len(rn.log)) <= rec.Index {
				rn.log = append(rn.log, LogEntry{})
			}
			rn.log[rec.Index] = entry
		case RecordTypeCommit:
			if rec.CommitIndex > lastCommitIndex {
				lastCommitIndex = rec.CommitIndex
			}
		case RecordTypeTermVote:
			lastTerm = rec.Term
			lastVotedFor = rec.VotedFor
		}
	}

	if lastTerm > 0 || lastVotedFor != "" {
		rn.currentTerm = lastTerm
		rn.votedFor = lastVotedFor
	}

	if lastCommitIndex > rn.lastApplied {
		rn.commitIndex = lastCommitIndex
		for rn.lastApplied < rn.commitIndex {
			rn.lastApplied++
			if int(rn.lastApplied) < len(rn.log) {
				rn.executeCommand(rn.lastApplied, rn.log[rn.lastApplied].Command)
			}
		}
	}

	log.Printf("[%s] WAL recovery done: %d log entries, commitIndex=%d, %d keys restored",
		rn.id, len(rn.log), rn.commitIndex, len(rn.dataStore))
	return nil
}

// Start begins the Raft consensus protocol
func (rn *RaftNode) Start() {
	log.Printf("[%s] Starting Raft node in %s state", rn.id, rn.state)
	rn.lastHeartbeat = time.Now()
	go rn.runElectionTimer()
}

// Stop signals all background goroutines to exit.
// Call after removing the node from the cluster or in tests when simulating a crash.
func (rn *RaftNode) Stop() {
	select {
	case <-rn.stopCh:
		// already stopped
	default:
		close(rn.stopCh)
	}
}

// runElectionTimer manages election timeout and triggers elections
func (rn *RaftNode) runElectionTimer() {
	for {
		timeoutDuration := rn.getElectionTimeout()

		select {
		case <-rn.stopCh:
			return
		case <-time.After(50 * time.Millisecond):
		}

		rn.mu.Lock()
		currentState := rn.state
		timeSinceHeartbeat := time.Since(rn.lastHeartbeat)
		rn.mu.Unlock()

		if currentState != Leader && timeSinceHeartbeat > timeoutDuration {
			log.Printf("[%s] Election timeout! Starting election...", rn.id)
			rn.startElection()
		}
	}
}

// getElectionTimeout returns a randomized election timeout to prevent split votes
func (rn *RaftNode) getElectionTimeout() time.Duration {
	return time.Duration(150+rand.Intn(150)) * time.Millisecond
}

// resetElectionTimer resets the election timer
func (rn *RaftNode) resetElectionTimer() {
	rn.lastHeartbeat = time.Now()
}

// startElection initiates a new election
func (rn *RaftNode) startElection() {
	rn.mu.Lock()

	rn.state = Candidate
	rn.currentTerm++
	rn.votedFor = rn.id
	rn.leaderId = "" // leader unknown during an election
	rn.lastHeartbeat = time.Now()
	currentTerm := rn.currentTerm
	rn.persistTermVote() // persist before releasing lock

	log.Printf("[%s] Starting election for term %d", rn.id, currentTerm)

	lastLogIndex := int32(len(rn.log) - 1)
	lastLogTerm := int32(0)
	if lastLogIndex >= 0 {
		lastLogTerm = rn.log[lastLogIndex].Term
	}

	rn.mu.Unlock()

	votes := 1
	votesNeeded := (len(rn.peers)+1)/2 + 1
	var voteMutex sync.Mutex
	for peerID, peerAddr := range rn.peers {
		go func(id, addr string) {
			vote := rn.requestVoteFromPeer(addr, currentTerm, lastLogIndex, lastLogTerm)

			voteMutex.Lock()
			if vote {
				votes++
				log.Printf("[%s] Received vote from %s (total: %d/%d)", rn.id, id, votes, votesNeeded)
			}

			if votes >= votesNeeded {
				rn.mu.Lock()
				if rn.state == Candidate && rn.currentTerm == currentTerm {
					log.Printf("[%s] 🎉 WON ELECTION for term %d!", rn.id, currentTerm)
					rn.becomeLeader()
				}
				rn.mu.Unlock()
			}
			voteMutex.Unlock()
		}(peerID, peerAddr)
	}
}

// becomeLeader transitions node to Leader state
func (rn *RaftNode) becomeLeader() {
	rn.state = Leader
	rn.leaderId = rn.id

	log.Printf("[%s] 👑 I am now the LEADER for term %d", rn.id, rn.currentTerm)

	lastLogIndex := int32(len(rn.log))
	for peerID := range rn.peers {
		rn.nextIndex[peerID] = lastLogIndex
		rn.matchIndex[peerID] = -1
	}
	go rn.sendHeartbeats()
}

// sendHeartbeats sends periodic heartbeats to all followers
func (rn *RaftNode) sendHeartbeats() {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-rn.stopCh:
			return
		case <-ticker.C:
		}

		rn.mu.RLock()
		if rn.state != Leader {
			rn.mu.RUnlock()
			return
		}
		term := rn.currentTerm
		rn.mu.RUnlock()

		for peerID, peerAddr := range rn.peers {
			go rn.sendAppendEntries(peerID, peerAddr, term)
		}
	}
}

// sendAppendEntries sends log entries or heartbeat to a peer
func (rn *RaftNode) sendAppendEntries(peerID, peerAddr string, term int32) {
	rn.mu.RLock()
	nextIdx := rn.nextIndex[peerID]
	prevLogIndex := nextIdx - 1
	prevLogTerm := int32(0)
	if prevLogIndex >= 0 && prevLogIndex < int32(len(rn.log)) {
		prevLogTerm = rn.log[prevLogIndex].Term
	}

	entries := []*pb.LogEntry{}
	if nextIdx < int32(len(rn.log)) {
		for i := nextIdx; i < int32(len(rn.log)); i++ {
			entries = append(entries, &pb.LogEntry{
				Term:    rn.log[i].Term,
				Command: rn.log[i].Command,
			})
		}
	}

	commitIndex := rn.commitIndex
	rn.mu.RUnlock()

	conn, err := grpc.Dial(peerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return
	}
	defer conn.Close()

	client := pb.NewRaftServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	req := &pb.AppendEntriesRequest{
		Term:         term,
		LeaderId:     rn.id,
		PrevLogIndex: prevLogIndex,
		PrevLogTerm:  prevLogTerm,
		Entries:      entries,
		LeaderCommit: commitIndex,
	}

	resp, err := client.AppendEntries(ctx, req)
	if err != nil {
		return
	}

	rn.mu.Lock()
	defer rn.mu.Unlock()

	if resp.Term > rn.currentTerm {
		rn.currentTerm = resp.Term
		rn.state = Follower
		rn.votedFor = ""
		rn.persistTermVote()
		log.Printf("[%s] Stepping down - discovered higher term %d", rn.id, resp.Term)
		return
	}

	if resp.Success {
		if len(entries) > 0 {
			rn.nextIndex[peerID] = nextIdx + int32(len(entries))
			rn.matchIndex[peerID] = rn.nextIndex[peerID] - 1
			log.Printf("[%s] Successfully replicated %d entries to %s", rn.id, len(entries), peerID)
			rn.updateCommitIndex()
		}
	} else {
		if rn.nextIndex[peerID] > 0 {
			rn.nextIndex[peerID]--
		}
	}
}

// updateCommitIndex updates the commit index based on majority replication
func (rn *RaftNode) updateCommitIndex() {
	for n := int32(len(rn.log) - 1); n > rn.commitIndex; n-- {
		if rn.log[n].Term != rn.currentTerm {
			continue
		}

		replicatedCount := 1
		for _, matchIdx := range rn.matchIndex {
			if matchIdx >= n {
				replicatedCount++
			}
		}

		majority := (len(rn.peers)+1)/2 + 1
		if replicatedCount >= majority {
			rn.commitIndex = n
			log.Printf("[%s] Advanced commit index to %d", rn.id, n)
			rn.applyCommittedEntries()
			break
		}
	}
}

// executeCommand applies a single command string to the in-memory dataStore.
// Caller must hold rn.mu.Lock().
func (rn *RaftNode) executeCommand(index int32, cmd string) {
	parts := strings.SplitN(cmd, " ", 3)
	switch strings.ToUpper(parts[0]) {
	case "SET":
		if len(parts) == 3 {
			rn.dataStore[parts[1]] = parts[2]
			log.Printf("[%s] Applied entry %d: SET %s", rn.id, index, parts[1])
		}
	case "DELETE":
		if len(parts) >= 2 {
			delete(rn.dataStore, parts[1])
			log.Printf("[%s] Applied entry %d: DELETE %s", rn.id, index, parts[1])
		}
	}
}

// applyCommittedEntries applies committed log entries to the state machine.
// Caller must hold rn.mu.Lock().
func (rn *RaftNode) applyCommittedEntries() {
	for rn.lastApplied < rn.commitIndex {
		rn.lastApplied++
		rn.executeCommand(rn.lastApplied, rn.log[rn.lastApplied].Command)
	}
	if rn.wal != nil && rn.lastApplied >= 0 {
		if err := rn.wal.AppendCommit(rn.lastApplied); err != nil {
			log.Printf("[%s] WAL commit write failed: %v", rn.id, err)
		}
	}
}

// requestVoteFromPeer sends a RequestVote RPC to a peer
func (rn *RaftNode) requestVoteFromPeer(peerAddr string, term, lastLogIndex, lastLogTerm int32) bool {
	conn, err := grpc.Dial(peerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return false
	}
	defer conn.Close()

	client := pb.NewRaftServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	req := &pb.VoteRequest{
		Term:         term,
		CandidateId:  rn.id,
		LastLogIndex: lastLogIndex,
		LastLogTerm:  lastLogTerm,
	}

	resp, err := client.RequestVote(ctx, req)
	if err != nil {
		return false
	}

	if resp.Term > term {
		rn.mu.Lock()
		if resp.Term > rn.currentTerm {
			rn.currentTerm = resp.Term
			rn.state = Follower
			rn.votedFor = ""
			rn.persistTermVote()
		}
		rn.mu.Unlock()
		return false
	}

	return resp.VoteGranted
}

// RequestVote RPC handler
func (rn *RaftNode) RequestVote(ctx context.Context, req *pb.VoteRequest) (*pb.VoteResponse, error) {
	rn.mu.Lock()
	defer rn.mu.Unlock()

	log.Printf("[%s] Received RequestVote from %s for term %d (current term: %d)",
		rn.id, req.CandidateId, req.Term, rn.currentTerm)

	if req.Term < rn.currentTerm {
		return &pb.VoteResponse{Term: rn.currentTerm, VoteGranted: false}, nil
	}

	if req.Term > rn.currentTerm {
		rn.currentTerm = req.Term
		rn.state = Follower
		rn.votedFor = ""
		rn.persistTermVote()
	}

	voteGranted := false
	if rn.votedFor == "" || rn.votedFor == req.CandidateId {
		lastLogIndex := int32(len(rn.log) - 1)
		lastLogTerm := int32(0)
		if lastLogIndex >= 0 {
			lastLogTerm = rn.log[lastLogIndex].Term
		}

		logOk := req.LastLogTerm > lastLogTerm ||
			(req.LastLogTerm == lastLogTerm && req.LastLogIndex >= lastLogIndex)

		if logOk {
			rn.votedFor = req.CandidateId
			voteGranted = true
			rn.resetElectionTimer()
			rn.persistTermVote()
			log.Printf("[%s] Granted vote to %s for term %d", rn.id, req.CandidateId, req.Term)
		}
	}

	return &pb.VoteResponse{Term: rn.currentTerm, VoteGranted: voteGranted}, nil
}

// AppendEntries RPC handler
func (rn *RaftNode) AppendEntries(ctx context.Context, req *pb.AppendEntriesRequest) (*pb.AppendEntriesResponse, error) {
	rn.mu.Lock()
	defer rn.mu.Unlock()

	if req.Term < rn.currentTerm {
		return &pb.AppendEntriesResponse{Term: rn.currentTerm, Success: false}, nil
	}

	if req.Term >= rn.currentTerm {
		rn.currentTerm = req.Term
		rn.state = Follower
		rn.votedFor = ""
		rn.leaderId = req.LeaderId
		rn.persistTermVote()
		rn.resetElectionTimer()
	}

	if req.PrevLogIndex >= 0 {
		if req.PrevLogIndex >= int32(len(rn.log)) {
			return &pb.AppendEntriesResponse{Term: rn.currentTerm, Success: false}, nil
		}
		if rn.log[req.PrevLogIndex].Term != req.PrevLogTerm {
			return &pb.AppendEntriesResponse{Term: rn.currentTerm, Success: false}, nil
		}
	}

	if len(req.Entries) > 0 {
		insertIndex := req.PrevLogIndex + 1
		for i, entry := range req.Entries {
			idx := insertIndex + int32(i)
			le := LogEntry{Term: entry.Term, Command: entry.Command}

			if rn.wal != nil {
				if err := rn.wal.AppendEntry(idx, le); err != nil {
					log.Printf("[%s] WAL write failed for entry %d: %v", rn.id, idx, err)
					return &pb.AppendEntriesResponse{Term: rn.currentTerm, Success: false}, nil
				}
			}

			if idx < int32(len(rn.log)) {
				rn.log[idx] = le
			} else {
				rn.log = append(rn.log, le)
			}
		}
		log.Printf("[%s] Appended %d entries from leader %s", rn.id, len(req.Entries), req.LeaderId)
	}

	if req.LeaderCommit > rn.commitIndex {
		lastNewEntryIndex := int32(len(rn.log) - 1)
		if req.LeaderCommit < lastNewEntryIndex {
			rn.commitIndex = req.LeaderCommit
		} else {
			rn.commitIndex = lastNewEntryIndex
		}
		rn.applyCommittedEntries()
	}

	return &pb.AppendEntriesResponse{Term: rn.currentTerm, Success: true}, nil
}

// AppendEntry adds a new entry to the log.
// Writes to WAL first — if WAL fails the entry is rejected.
func (rn *RaftNode) AppendEntry(command string) bool {
	rn.mu.Lock()
	defer rn.mu.Unlock()

	if rn.state != Leader {
		return false
	}

	newIndex := int32(len(rn.log))
	entry := LogEntry{
		Term:    rn.currentTerm,
		Command: command,
	}

	if rn.wal != nil {
		if err := rn.wal.AppendEntry(newIndex, entry); err != nil {
			log.Printf("[%s] WAL write failed, rejecting entry: %v", rn.id, err)
			return false
		}
	}

	rn.log = append(rn.log, entry)
	log.Printf("[%s] Leader appended entry: %s (index: %d)", rn.id, command, newIndex)
	return true
}
