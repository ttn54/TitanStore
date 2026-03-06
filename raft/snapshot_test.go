package raft

import (
	"os"
	"testing"
)

// TestSnapshot_WriteAndRead verifies that a snapshot round-trips through
// WriteSnapshot / ReadSnapshot with all fields intact.
func TestSnapshot_WriteAndRead(t *testing.T) {
	f, err := os.CreateTemp("", "titanstore-snap-*.snap")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	snap := Snapshot{
		SnapshotIndex: 10,
		SnapshotTerm:  3,
		CurrentTerm:   3,
		VotedFor:      "node2",
		DataStore: map[string]string{
			"city":    "vancouver",
			"version": "42",
		},
	}

	if err := WriteSnapshot(path, snap); err != nil {
		t.Fatalf("WriteSnapshot failed: %v", err)
	}

	got, err := ReadSnapshot(path)
	if err != nil {
		t.Fatalf("ReadSnapshot failed: %v", err)
	}

	if got.SnapshotIndex != 10 {
		t.Errorf("SnapshotIndex: want 10, got %d", got.SnapshotIndex)
	}
	if got.SnapshotTerm != 3 {
		t.Errorf("SnapshotTerm: want 3, got %d", got.SnapshotTerm)
	}
	if got.CurrentTerm != 3 {
		t.Errorf("CurrentTerm: want 3, got %d", got.CurrentTerm)
	}
	if got.VotedFor != "node2" {
		t.Errorf("VotedFor: want node2, got %q", got.VotedFor)
	}
	if got.DataStore["city"] != "vancouver" {
		t.Errorf("DataStore[city]: want vancouver, got %q", got.DataStore["city"])
	}
	if got.DataStore["version"] != "42" {
		t.Errorf("DataStore[version]: want 42, got %q", got.DataStore["version"])
	}
	t.Log("✅ Snapshot round-trips through WriteSnapshot/ReadSnapshot correctly")
}

// TestSnapshot_AtomicWrite verifies that a second WriteSnapshot overwrites the
// first completely and the old data is gone.
func TestSnapshot_AtomicWrite(t *testing.T) {
	f, err := os.CreateTemp("", "titanstore-snap-atomic-*.snap")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	first := Snapshot{SnapshotIndex: 1, CurrentTerm: 1, DataStore: map[string]string{"k": "old"}}
	if err := WriteSnapshot(path, first); err != nil {
		t.Fatalf("first WriteSnapshot: %v", err)
	}

	second := Snapshot{SnapshotIndex: 5, CurrentTerm: 2, DataStore: map[string]string{"k": "new", "x": "extra"}}
	if err := WriteSnapshot(path, second); err != nil {
		t.Fatalf("second WriteSnapshot: %v", err)
	}

	got, err := ReadSnapshot(path)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if got.SnapshotIndex != 5 {
		t.Errorf("want SnapshotIndex=5, got %d", got.SnapshotIndex)
	}
	if got.DataStore["k"] != "new" {
		t.Errorf("want k=new, got %q", got.DataStore["k"])
	}
	t.Log("✅ Second WriteSnapshot fully replaces first")
}

// TestReadSnapshot_Missing verifies that ReadSnapshot returns (zero, nil) when
// the file does not exist — this is the fresh-start case.
func TestReadSnapshot_Missing(t *testing.T) {
	snap, err := ReadSnapshot("/tmp/titanstore-does-not-exist-ever.snap")
	if err != nil {
		t.Fatalf("expected nil error for missing file, got: %v", err)
	}
	if snap != nil {
		t.Fatalf("expected nil snapshot for missing file, got: %+v", snap)
	}
	t.Log("✅ Missing snapshot file returns nil, nil")
}

// TestTakeSnapshot_WritesFileAndTruncatesWAL verifies that TakeSnapshot:
//  1. Writes a snapshot file that restores the full state
//  2. Truncates the WAL (only a TermVote record remains)
//  3. Sets snapshotIndex / snapshotTerm on the node
func TestTakeSnapshot_WritesFileAndTruncatesWAL(t *testing.T) {
	walFile, err := os.CreateTemp("", "titanstore-snap-wal-*.wal")
	if err != nil {
		t.Fatalf("temp WAL: %v", err)
	}
	walPath := walFile.Name()
	walFile.Close()
	defer os.Remove(walPath)

	snapPath := walPath + ".snap"
	defer os.Remove(snapPath)

	wal, _ := NewFileWAL(walPath)
	node := NewRaftNode("snap-node", map[string]string{})
	node.SetWAL(wal)
	node.SetSnapshotPath(snapPath)

	// Populate node as if it ran a few commands
	node.mu.Lock()
	node.state = Leader
	node.currentTerm = 2
	node.votedFor = "snap-node"
	node.log = []LogEntry{
		{Term: 1, Command: "SET city berlin"},
		{Term: 2, Command: "SET lang rust"},
	}
	node.commitIndex = 1
	node.lastApplied = 1
	node.dataStore = map[string]string{"city": "berlin", "lang": "rust"}
	node.mu.Unlock()

	if err := node.TakeSnapshot(); err != nil {
		t.Fatalf("TakeSnapshot failed: %v", err)
	}

	// snapshotIndex should equal commitIndex
	node.mu.RLock()
	si := node.snapshotIndex
	st := node.snapshotTerm
	node.mu.RUnlock()

	if si != 1 {
		t.Errorf("snapshotIndex: want 1, got %d", si)
	}
	if st != 2 {
		t.Errorf("snapshotTerm: want 2, got %d", st)
	}

	// WAL must contain only the TermVote record written during truncation
	records, err := wal.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll after TakeSnapshot: %v", err)
	}
	if len(records) != 1 || records[0].Type != RecordTypeTermVote {
		t.Errorf("expected 1 TermVote record in WAL after snapshot, got %d records", len(records))
	}

	// Snapshot file must restore state correctly on a fresh node
	snap, err := ReadSnapshot(snapPath)
	if err != nil || snap == nil {
		t.Fatalf("ReadSnapshot after TakeSnapshot: err=%v snap=%v", err, snap)
	}
	if snap.DataStore["city"] != "berlin" || snap.DataStore["lang"] != "rust" {
		t.Errorf("unexpected dataStore in snapshot: %v", snap.DataStore)
	}
	t.Log("✅ TakeSnapshot writes file and truncates WAL correctly")
}

// TestRecoverFromWAL_WithSnapshot verifies the full recovery path:
//  1. Take a snapshot at index 1
//  2. Append two more entries to the WAL (index 2, 3)
//  3. Restart from snapshot + WAL tail — all three keys must be present
func TestRecoverFromWAL_WithSnapshot(t *testing.T) {
	walFile, err := os.CreateTemp("", "titanstore-snap-recovery-*.wal")
	if err != nil {
		t.Fatalf("temp WAL: %v", err)
	}
	walPath := walFile.Name()
	walFile.Close()
	defer os.Remove(walPath)

	snapPath := walPath + ".snap"
	defer os.Remove(snapPath)

	// --- Phase 1: populate, snapshot, then append two more entries ---
	{
		wal, _ := NewFileWAL(walPath)
		node := NewRaftNode("rec-node", map[string]string{})
		node.SetWAL(wal)
		node.SetSnapshotPath(snapPath)

		node.mu.Lock()
		node.state = Leader
		node.currentTerm = 2
		node.votedFor = "rec-node"
		node.log = []LogEntry{
			{Term: 1, Command: "SET a alpha"},
			{Term: 2, Command: "SET b beta"},
		}
		node.commitIndex = 1
		node.lastApplied = 1
		node.dataStore = map[string]string{"a": "alpha", "b": "beta"}
		node.mu.Unlock()

		if err := node.TakeSnapshot(); err != nil {
			t.Fatalf("TakeSnapshot: %v", err)
		}

		// Two more entries after the snapshot
		node.mu.Lock()
		node.state = Leader
		node.mu.Unlock()
		node.AppendEntry("SET c gamma")
		node.AppendEntry("SET d delta")

		node.mu.Lock()
		node.commitIndex = int32(len(node.log) - 1)
		node.applyCommittedEntries()
		node.mu.Unlock()

		wal.Close()
	}

	// --- Phase 2: fresh node recovers from snapshot + WAL tail ---
	{
		wal, _ := NewFileWAL(walPath)
		node := NewRaftNode("rec-node", map[string]string{})
		node.SetWAL(wal)
		node.SetSnapshotPath(snapPath)

		if err := node.RecoverFromWAL(); err != nil {
			t.Fatalf("RecoverFromWAL: %v", err)
		}
		wal.Close()

		for k, want := range map[string]string{"a": "alpha", "b": "beta", "c": "gamma", "d": "delta"} {
			got, ok := node.GetValue(k)
			if !ok {
				t.Errorf("key %q missing after snapshot+WAL recovery", k)
				continue
			}
			if got != want {
				t.Errorf("key %q: want %q, got %q", k, want, got)
			}
		}
	}
	t.Log("✅ Recovery with snapshot + WAL tail restores all keys correctly")
}
