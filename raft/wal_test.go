package raft

import (
	"os"
	"testing"
)

// helper: create a temp WAL file that is removed after the test
func tempWAL(t *testing.T) (*FileWAL, string) {
	t.Helper()
	f, err := os.CreateTemp("", "titanstore-wal-*.wal")
	if err != nil {
		t.Fatalf("could not create temp file: %v", err)
	}
	path := f.Name()
	f.Close()

	wal, err := NewFileWAL(path)
	if err != nil {
		t.Fatalf("NewFileWAL failed: %v", err)
	}
	return wal, path
}

// TestWAL_AppendAndReadBack writes a few entries + a commit, then reads them
// back and verifies every field is preserved exactly.
func TestWAL_AppendAndReadBack(t *testing.T) {
	wal, path := tempWAL(t)
	defer os.Remove(path)
	defer wal.Close()

	entries := []LogEntry{
		{Term: 1, Command: "SET name alice"},
		{Term: 1, Command: "SET age 30"},
		{Term: 2, Command: "SET city vancouver"},
	}

	// Write entries to WAL
	for i, e := range entries {
		if err := wal.AppendEntry(int32(i), e); err != nil {
			t.Fatalf("AppendEntry(%d) failed: %v", i, err)
		}
	}

	// Write a commit marker
	if err := wal.AppendCommit(2); err != nil {
		t.Fatalf("AppendCommit failed: %v", err)
	}

	// Read everything back
	records, err := wal.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}

	// Expect 3 entry records + 1 commit record = 4 total
	if len(records) != 4 {
		t.Fatalf("expected 4 records, got %d", len(records))
	}

	// Verify each entry record
	for i, e := range entries {
		r := records[i]
		if r.Type != RecordTypeEntry {
			t.Errorf("record %d: expected RecordTypeEntry, got %d", i, r.Type)
		}
		if r.Index != int32(i) {
			t.Errorf("record %d: expected Index=%d, got %d", i, i, r.Index)
		}
		if r.Term != e.Term {
			t.Errorf("record %d: expected Term=%d, got %d", i, e.Term, r.Term)
		}
		if r.Command != e.Command {
			t.Errorf("record %d: expected Command=%q, got %q", i, e.Command, r.Command)
		}
	}

	// Verify commit record
	commit := records[3]
	if commit.Type != RecordTypeCommit {
		t.Errorf("expected RecordTypeCommit, got %d", commit.Type)
	}
	if commit.CommitIndex != 2 {
		t.Errorf("expected CommitIndex=2, got %d", commit.CommitIndex)
	}

	t.Log("✅ All entries and commit record read back correctly")
}

// TestWAL_AppendEntryBatch writes multiple entries at once, then reads them back.
func TestWAL_AppendEntryBatch(t *testing.T) {
	wal, path := tempWAL(t)
	defer os.Remove(path)
	defer wal.Close()

	entries := []LogEntry{
		{Term: 2, Command: "SET batch 1"},
		{Term: 2, Command: "SET batch 2"},
		{Term: 2, Command: "SET batch 3"},
	}

	if err := wal.AppendEntryBatch(10, entries); err != nil {
		t.Fatalf("AppendEntryBatch failed: %v", err)
	}

	recs, err := wal.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}

	if len(recs) != len(entries) {
		t.Fatalf("expected %d records, got %d", len(entries), len(recs))
	}

	for i, e := range entries {
		if recs[i].Type != RecordTypeEntry {
			t.Errorf("record %d: expected type %v, got %v", i, RecordTypeEntry, recs[i].Type)
		}
		if recs[i].Index != int32(10+i) {
			t.Errorf("record %d: expected index %d, got %d", i, 10+i, recs[i].Index)
		}
		if recs[i].Term != e.Term {
			t.Errorf("record %d: expected term %d, got %d", i, e.Term, recs[i].Term)
		}
		if recs[i].Command != e.Command {
			t.Errorf("record %d: expected command %s, got %s", i, e.Command, recs[i].Command)
		}
	}
}

// TestWAL_EmptyFile verifies that reading an empty WAL returns zero records
// without an error — this is the fresh-start case.
func TestWAL_EmptyFile(t *testing.T) {
	wal, path := tempWAL(t)
	defer os.Remove(path)
	defer wal.Close()

	records, err := wal.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll on empty file failed: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected 0 records on empty file, got %d", len(records))
	}

	t.Log("✅ Empty WAL handled correctly")
}

// TestWAL_CrashRecovery simulates a node crash mid-write by manually
// truncating the file after the last full record, then verifies ReadAll
// silently drops the partial tail and returns only complete records.
func TestWAL_CrashRecovery(t *testing.T) {
	wal, path := tempWAL(t)
	defer os.Remove(path)

	// Write two good entries
	_ = wal.AppendEntry(0, LogEntry{Term: 1, Command: "SET x 100"})
	_ = wal.AppendEntry(1, LogEntry{Term: 1, Command: "SET y 200"})
	wal.Close()

	// Simulate crash: append a partial/corrupt frame directly to the file
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	f.Write([]byte{0x00, 0x00, 0x00, 0x10}) // length header says 16 bytes follow
	f.Write([]byte{0xDE, 0xAD, 0xBE})       // only 3 bytes — crash mid-write
	f.Close()

	// Re-open WAL and read back
	wal2, err := NewFileWAL(path)
	if err != nil {
		t.Fatalf("NewFileWAL failed after crash: %v", err)
	}
	defer wal2.Close()

	records, err := wal2.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll after crash failed: %v", err)
	}

	// Only the 2 good entries should come back — partial tail is dropped
	if len(records) != 2 {
		t.Fatalf("expected 2 records after crash recovery, got %d", len(records))
	}
	if records[0].Command != "SET x 100" {
		t.Errorf("record 0 command mismatch: %q", records[0].Command)
	}
	if records[1].Command != "SET y 200" {
		t.Errorf("record 1 command mismatch: %q", records[1].Command)
	}

	t.Log("✅ Crash recovery: partial tail silently dropped, good records intact")
}

// TestWAL_TermVote_RoundTrip verifies AppendTermVote records survive close/reopen.
func TestWAL_TermVote_RoundTrip(t *testing.T) {
	wal, path := tempWAL(t)
	defer os.Remove(path)

	if err := wal.AppendTermVote(3, "nodeA"); err != nil {
		t.Fatalf("AppendTermVote failed: %v", err)
	}
	if err := wal.AppendTermVote(5, "nodeB"); err != nil {
		t.Fatalf("AppendTermVote (2) failed: %v", err)
	}
	wal.Close()

	wal2, err := NewFileWAL(path)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer wal2.Close()

	records, err := wal2.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}
	if records[0].Type != RecordTypeTermVote || records[0].Term != 3 || records[0].VotedFor != "nodeA" {
		t.Errorf("record 0 mismatch: %+v", records[0])
	}
	if records[1].Type != RecordTypeTermVote || records[1].Term != 5 || records[1].VotedFor != "nodeB" {
		t.Errorf("record 1 mismatch: %+v", records[1])
	}
	t.Log("✅ TermVote records round-trip correctly through WAL")
}

// TestRecoverFromWAL_RestoresTermAndVote verifies that currentTerm and votedFor
// are rebuilt correctly after a simulated restart.
func TestRecoverFromWAL_RestoresTermAndVote(t *testing.T) {
	f, err := os.CreateTemp("", "titanstore-termvote-*.wal")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	walPath := f.Name()
	f.Close()
	defer os.Remove(walPath)

	// Phase 1: node simulates two term bumps then grants a vote
	{
		wal, _ := NewFileWAL(walPath)
		node := NewRaftNode("n1", map[string]string{})
		node.SetWAL(wal)

		// Simulate term=1, votedFor=n1
		node.mu.Lock()
		node.currentTerm = 1
		node.votedFor = "n1"
		node.persistTermVote()
		node.mu.Unlock()

		// Simulate stepping up to term=3, voted for someone else
		node.mu.Lock()
		node.currentTerm = 3
		node.votedFor = "n2"
		node.persistTermVote()
		node.mu.Unlock()

		wal.Close()
	}

	// Phase 2: fresh node recovers
	{
		wal, _ := NewFileWAL(walPath)
		node := NewRaftNode("n1", map[string]string{})
		node.SetWAL(wal)

		if err := node.RecoverFromWAL(); err != nil {
			t.Fatalf("RecoverFromWAL: %v", err)
		}
		wal.Close()

		node.mu.RLock()
		term := node.currentTerm
		voted := node.votedFor
		node.mu.RUnlock()

		if term != 3 {
			t.Errorf("expected currentTerm=3, got %d", term)
		}
		if voted != "n2" {
			t.Errorf("expected votedFor=n2, got %q", voted)
		}
	}
	t.Log("✅ currentTerm and votedFor restored correctly after simulated restart")
}

// TestWALRecovery_EndToEnd simulates a full power-cycle:
//  1. A leader node writes and commits several keys to disk via WAL.
//  2. The node is discarded (simulating a restart).
//  3. A fresh node opens the same WAL file and calls RecoverFromWAL.
//  4. The test verifies every key is restored exactly, and commitIndex is correct.
func TestWALRecovery_EndToEnd(t *testing.T) {
	f, err := os.CreateTemp("", "titanstore-e2e-*.wal")
	if err != nil {
		t.Fatalf("could not create temp WAL file: %v", err)
	}
	walPath := f.Name()
	f.Close()
	defer os.Remove(walPath)

	// --- Phase 1: write and commit three keys ---
	{
		wal, err := NewFileWAL(walPath)
		if err != nil {
			t.Fatalf("NewFileWAL (phase 1): %v", err)
		}

		node := NewRaftNode("e2e-node", map[string]string{})
		node.SetWAL(wal)

		// Promote to leader so AppendEntry is accepted
		node.mu.Lock()
		node.state = Leader
		node.currentTerm = 1
		node.mu.Unlock()

		commands := []string{
			"SET city vancouver",
			"SET lang go",
			"SET version 2",
			"DELETE lang", // delete one key to verify DELETE survives recovery
		}
		for _, cmd := range commands {
			if !node.AppendEntry(cmd) {
				t.Fatalf("AppendEntry(%q) failed", cmd)
			}
		}

		// Advance commitIndex and flush the commit record to WAL
		node.mu.Lock()
		node.commitIndex = int32(len(node.log) - 1)
		node.applyCommittedEntries()
		node.mu.Unlock()

		if err := wal.Close(); err != nil {
			t.Fatalf("wal.Close (phase 1): %v", err)
		}
	}

	// --- Phase 2: fresh node recovers from the WAL ---
	{
		wal, err := NewFileWAL(walPath)
		if err != nil {
			t.Fatalf("NewFileWAL (phase 2): %v", err)
		}
		defer wal.Close()

		node := NewRaftNode("e2e-node", map[string]string{})
		node.SetWAL(wal)

		if err := node.RecoverFromWAL(); err != nil {
			t.Fatalf("RecoverFromWAL: %v", err)
		}

		// city and version should be present; lang was deleted
		want := map[string]string{
			"city":    "vancouver",
			"version": "2",
		}
		for k, wantVal := range want {
			got, ok := node.GetValue(k)
			if !ok {
				t.Errorf("key %q missing after recovery", k)
				continue
			}
			if got != wantVal {
				t.Errorf("key %q: want %q, got %q", k, wantVal, got)
			}
		}

		// lang was deleted — must not be present
		if _, ok := node.GetValue("lang"); ok {
			t.Error("key \"lang\" should have been deleted but is present after recovery")
		}

		// commitIndex and lastApplied should match
		node.mu.RLock()
		ci := node.commitIndex
		la := node.lastApplied
		node.mu.RUnlock()

		if ci != 3 || la != 3 {
			t.Errorf("expected commitIndex=3 lastApplied=3, got commitIndex=%d lastApplied=%d", ci, la)
		}
	}

	t.Log("✅ End-to-end WAL recovery: all keys (including DELETE) restored after simulated restart")
}

// TestWAL_Truncate verifies that after Truncate the WAL file is empty and new
// records can be appended and read back correctly.
func TestWAL_Truncate(t *testing.T) {
	wal, path := tempWAL(t)
	defer os.Remove(path)
	defer wal.Close()

	// Write some entries
	_ = wal.AppendEntry(0, LogEntry{Term: 1, Command: "SET a 1"})
	_ = wal.AppendEntry(1, LogEntry{Term: 1, Command: "SET b 2"})
	_ = wal.AppendCommit(1)

	// Truncate
	if err := wal.Truncate(); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	// File must be empty now
	records, err := wal.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll after Truncate failed: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected 0 records after Truncate, got %d", len(records))
	}

	// New records appended after truncate must be readable
	if err := wal.AppendTermVote(5, "nodeZ"); err != nil {
		t.Fatalf("AppendTermVote after Truncate failed: %v", err)
	}

	records, err = wal.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll after post-truncate append failed: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record after post-truncate append, got %d", len(records))
	}
	if records[0].Type != RecordTypeTermVote || records[0].Term != 5 || records[0].VotedFor != "nodeZ" {
		t.Errorf("unexpected record: %+v", records[0])
	}
	t.Log("✅ Truncate clears WAL and allows fresh appends")
}
