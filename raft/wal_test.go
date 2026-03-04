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
	f.Write([]byte{0xDE, 0xAD, 0xBE})        // only 3 bytes — crash mid-write
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
