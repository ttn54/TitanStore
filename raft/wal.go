package raft

// RecordType distinguishes WAL record kinds.
type RecordType uint8

const (
	// RecordTypeEntry is a log entry written to disk before in-memory append.
	RecordTypeEntry RecordType = 1
	// RecordTypeCommit marks the highest index that has been applied to the state machine.
	RecordTypeCommit RecordType = 2
)

// WALRecord is the atomic unit written to the Write-Ahead Log.
// Encoded as binary (gob over a length-prefixed byte slice) — no plain strings.
type WALRecord struct {
	Type        RecordType
	Index       int32
	Term        int32
	Command     string
	CommitIndex int32
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

	// ReadAll replays every record from the start of the file.
	// Partial tail records left by a crash are silently dropped.
	// Called once during boot-sequence recovery only.
	ReadAll() ([]WALRecord, error)

	// Close flushes and releases the underlying file handle.
	Close() error
}
