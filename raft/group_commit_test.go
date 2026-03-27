package raft

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func TestGroupCommit(t *testing.T) {
	wal, path := tempWAL(t)
	defer os.Remove(path)
	defer wal.Close()

	node := NewRaftNode("node1", nil)
	node.SetWAL(wal)

	// Promote node to Leader manually to allow proposals
	node.mu.Lock()
	node.state = Leader
	node.currentTerm = 1
	node.leaderId = "node1"
	node.mu.Unlock()

	node.Start() // This should start the batch committer
	defer node.Stop()

	// Propose 100 commands concurrently
	var wg sync.WaitGroup
	numProposals := 100
	errs := make(chan error, numProposals)

	for i := 0; i < numProposals; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			cmd := fmt.Sprintf("SET key%d value%d", id, id)
			err := node.Propose(cmd)
			if err != nil {
				errs <- fmt.Errorf("Propose failed: %w", err)
			}
		}(i)
	}

	// Wait with timeout
	c := make(chan struct{})
	go func() {
		wg.Wait()
		close(c)
	}()

	select {
	case <-c:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for proposals to commit")
	}

	close(errs)
	for err := range errs {
		t.Errorf("proposal error: %v", err)
	}

	// Verify they are all in the log
	node.mu.RLock()
	if len(node.log) != numProposals {
		t.Errorf("expected %d entries in log, got %d", numProposals, len(node.log))
	}
	node.mu.RUnlock()

	// Verify they hit the WAL
	wal.mu.Lock()
	wal.file.Sync()
	wal.mu.Unlock()

	recs, err := wal.ReadAll()
	if err != nil {
		t.Fatalf("could not read wal: %v", err)
	}

	// We might have TermVote records from start, let's just count Entry records
	entryCount := 0
	for _, r := range recs {
		if r.Type == RecordTypeEntry {
			entryCount++
		}
	}
	if entryCount != numProposals {
		t.Errorf("expected %d entry records in WAL, got %d", numProposals, entryCount)
	}
}
