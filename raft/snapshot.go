package raft

import (
	"encoding/gob"
	"errors"
	"os"
)

// Snapshot captures the full state machine at a point in the Raft log.
// It is written atomically via a temp-file + rename so a crash mid-write
// always leaves a valid previous snapshot on disk.
type Snapshot struct {
	// SnapshotIndex is the log index of the last entry included in the snapshot.
	SnapshotIndex int32
	// SnapshotTerm is the term of that entry.
	SnapshotTerm int32
	// CurrentTerm and VotedFor are Raft persistent state — must survive restarts.
	CurrentTerm int32
	VotedFor    string
	// DataStore is the full key-value state machine at SnapshotIndex.
	DataStore map[string]string
}

// WriteSnapshot atomically writes snap to path.
// It writes to path+".tmp", fsyncs, then renames over path.
// A crash before the rename leaves the previous snapshot intact.
func WriteSnapshot(path string, snap Snapshot) error {
	tmp := path + ".tmp"

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	if err := gob.NewEncoder(f).Encode(snap); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}

	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	// Atomic replace: on Linux rename(2) is atomic within the same filesystem.
	return os.Rename(tmp, path)
}

// ReadSnapshot reads the snapshot at path.
// Returns (nil, nil) if the file does not exist — this is the fresh-start case.
func ReadSnapshot(path string) (*Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var snap Snapshot
	if err := gob.NewDecoder(f).Decode(&snap); err != nil {
		return nil, err
	}
	return &snap, nil
}
