package raft

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"io"
	"os"
	"sync"
)

// RecordType distinguishes WAL record kinds.
type RecordType uint8

const (
	// RecordTypeEntry is a log entry written to disk before in-memory append.
	RecordTypeEntry RecordType = 1
	// RecordTypeCommit marks the highest index that has been applied to the state machine.
	RecordTypeCommit RecordType = 2
	// RecordTypeTermVote persists currentTerm and votedFor so they survive restarts.
	// Per the Raft paper these fields are "persistent state" and must never be lost.
	RecordTypeTermVote RecordType = 3
)

// WALRecord is the atomic unit written to the Write-Ahead Log.
// Encoded as binary (gob over a length-prefixed byte slice) — no plain strings.
type WALRecord struct {
	Type        RecordType
	Index       int32
	Term        int32
	Command     string
	CommitIndex int32
	VotedFor    string // used by RecordTypeTermVote
}

// WAL is the interface for the Write-Ahead Log.
// All methods must be safe for concurrent use.
type WAL interface {
	// AppendEntry durably writes a log-entry record BEFORE it is added to
	// the in-memory log.
	AppendEntry(index int32, entry LogEntry) error

	// AppendCommit writes a commit marker so boot-sequence recovery knows
	// which entries have already been applied to the state machine.
	AppendCommit(commitIndex int32) error

	// AppendTermVote durably persists currentTerm and votedFor.
	// Must be called while the node's mutex is held, before releasing it,
	// so the on-disk state never lags behind the in-memory state.
	AppendTermVote(term int32, votedFor string) error

	// ReadAll replays every record from the start of the file.
	// Partial tail records left by a crash are silently dropped.
	// Called once during boot-sequence recovery only.
	ReadAll() ([]WALRecord, error)

	// Close flushes and releases the underlying file handle.
	Close() error
}

// FileWAL is the disk-backed implementation of WAL.
//
// Wire format (append-only):
//
//	[4 bytes big-endian uint32: payload length] [N bytes: gob-encoded WALRecord]
//	[4 bytes big-endian uint32: payload length] [N bytes: gob-encoded WALRecord]
//	...
type FileWAL struct {
	mu   sync.Mutex
	file *os.File
	path string
}

// NewFileWAL opens or creates the WAL file at path.
// Existing content is preserved; new records are appended.
func NewFileWAL(path string) (*FileWAL, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return &FileWAL{file: f, path: path}, nil
}

// write serialises rec into a length-prefixed binary frame and fsyncs to disk.
func (w *FileWAL) write(rec WALRecord) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(rec); err != nil {
		return err
	}

	data := buf.Bytes()
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))

	if _, err := w.file.Write(lenBuf); err != nil {
		return err
	}
	if _, err := w.file.Write(data); err != nil {
		return err
	}
	// fsync: flush OS page cache to physical media before returning.
	return w.file.Sync()
}

// AppendEntry writes a log-entry record before it is added to the in-memory log.
func (w *FileWAL) AppendEntry(index int32, entry LogEntry) error {
	return w.write(WALRecord{
		Type:    RecordTypeEntry,
		Index:   index,
		Term:    entry.Term,
		Command: entry.Command,
	})
}

// AppendCommit writes a commit marker so recovery can rebuild the state machine.
func (w *FileWAL) AppendCommit(commitIndex int32) error {
	return w.write(WALRecord{
		Type:        RecordTypeCommit,
		CommitIndex: commitIndex,
	})
}

// AppendTermVote persists currentTerm and votedFor to disk.
func (w *FileWAL) AppendTermVote(term int32, votedFor string) error {
	return w.write(WALRecord{
		Type:     RecordTypeTermVote,
		Term:     term,
		VotedFor: votedFor,
	})
}

// ReadAll reads every record from the start of the file.
// A partial tail record caused by a crash is silently dropped.
func (w *FileWAL) ReadAll() ([]WALRecord, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	var records []WALRecord
	lenBuf := make([]byte, 4)

	for {
		if _, err := io.ReadFull(w.file, lenBuf); err != nil {
			// Clean EOF or partial length header — stop reading.
			break
		}

		length := binary.BigEndian.Uint32(lenBuf)
		data := make([]byte, length)
		if _, err := io.ReadFull(w.file, data); err != nil {
			// Partial payload from a prior crash — discard tail and stop.
			break
		}

		var rec WALRecord
		if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&rec); err != nil {
			// Corrupt record — stop reading.
			break
		}
		records = append(records, rec)
	}

	return records, nil
}

// Close flushes and closes the underlying file.
func (w *FileWAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}
