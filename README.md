# TitanStore

TitanStore is a distributed key-value database built from scratch in Go. It implements the Raft consensus algorithm to elect a leader, replicate writes across a cluster, and survive node failures — all backed by a binary Write-Ahead Log with crash-safe recovery and atomic snapshots.

It is the storage backend for TitanSync, a file-sync daemon that needs a highly available, replicated state store.

---

## Features

- **Raft consensus** — leader election, log replication, split-brain prevention
- **Durable writes** — binary WAL with `fsync` on every record; if `SET` returns `OK`, it is on disk
- **Crash recovery** — two-phase boot: load snapshot, replay WAL tail since snapshot index
- **Log compaction** — `TakeSnapshot()` serialises state atomically (`write → fsync → rename`) and truncates the WAL
- **Dynamic cluster config** — topology via `--peers` flag, no recompile needed
- **TCP client API** — plaintext `GET` / `SET` / `DELETE`, compatible with `nc`; followers automatically redirect writes to the current leader

---

## Quick Start

```bash
# Build
go build -o bin/titanstore-server cmd/server/main.go

# Start a 3-node cluster
make cluster-start

# Or manually
go run cmd/server/main.go \
  -id=node1 -port=5001 -client-port=6001 \
  -advertise-client-addr=localhost:6001 \
  -peers="node2:localhost:5002:6002,node3:localhost:5003:6003" &
```

## Client API

```bash
# Write — connect to any node; followers redirect you to the leader
echo 'SET user:42 {"name":"zen","ts":1741276800}' | nc localhost 6001

# Read — any node serves reads
echo 'GET user:42' | nc localhost 6002

# Delete
echo 'DELETE user:42' | nc localhost 6001
```

**Protocol** — newline-terminated plaintext:

| Command | Success | Error |
|---------|---------|-------|
| `GET <key>` | `VALUE <value>` | `NOT_FOUND` |
| `SET <key> <value>` | `OK` | `ERR NOT_LEADER <tcp-addr>` |
| `DELETE <key>` | `OK` | `ERR NOT_LEADER <tcp-addr>` |

---

## Architecture

```
┌────────────────────────────────────────────────────────────┐
│                     TitanStore Cluster                      │
│                                                             │
│   Node 1 :5001/:6001   Node 2 :5002/:6002   Node 3 :5003   │
│   ┌─────────────┐      ┌─────────────┐      ┌───────────┐  │
│   │  Follower   │      │   LEADER    │      │ Follower  │  │
│   │  log[...]   │←─────│─ replicate ─│─────→│ log[...]  │  │
│   │  WAL+snap   │      │  WAL+snap   │      │ WAL+snap  │  │
│   └─────────────┘      └─────────────┘      └───────────┘  │
└────────────────────────────────────────────────────────────┘
          ↑          gRPC: RequestVote / AppendEntries         ↑

   TCP clients ──→ any node ──→ redirect to leader if needed
```

---

## How It Works

### Leader Election

All nodes start as Followers. Each picks a random election timeout (150–300 ms). If no heartbeat arrives before the timeout fires, the node becomes a Candidate: it increments its term, votes for itself, and sends `RequestVote` RPCs to all peers. The first candidate to collect a strict majority wins and becomes Leader. The randomised timeout prevents all nodes from timing out simultaneously and splitting votes.

Once elected, the leader sends `AppendEntries` heartbeats every 50 ms to reset every follower's timer. If the leader dies, the surviving nodes time out and hold a new election within one timeout window.

### Write Path

```
Client                 Leader                  Followers
  │                      │                         │
  │── SET k v ──────────►│                         │
  │                      │── write to WAL (fsync) ─┤
  │                      │── AppendEntries RPC ────►│
  │                      │                         │── write to WAL (fsync)
  │                      │◄── success (majority) ──┤
  │                      │── apply to dataStore    │
  │◄── OK ───────────────│                         │
```

The leader writes to its own WAL first, then fans out `AppendEntries` RPCs in parallel. Once a strict majority acknowledges, the entry is committed and applied to the in-memory key-value store. The `OK` response is only sent after commit — if the node crashes before that, the write is retried from WAL on recovery.

### Crash Recovery

On boot, the node runs a two-phase recovery:

1. **Load snapshot** — if a snapshot file exists, seed `dataStore`, `commitIndex`, `currentTerm`, and `votedFor` from it.
2. **Replay WAL tail** — read all WAL records; skip any entries already covered by the snapshot (`index ≤ snapshotIndex`), replay the rest in order.

This bounds recovery time to O(entries since the last snapshot) rather than O(full log history).

### Log Compaction

`TakeSnapshot()` serialises the full `dataStore` to a temp file, calls `fsync`, then calls `os.Rename` to atomically replace the previous snapshot. The WAL is then truncated to a single term/vote record. If the process crashes mid-snapshot, the previous snapshot is untouched — `rename(2)` is atomic on Linux.

### Leader Redirect

If a client sends a write to a follower, the follower replies:

```
ERR NOT_LEADER localhost:6002
```

The address is the leader's **TCP client port**, not its gRPC port. The client can immediately reconnect to that address and retry.

---

## Design Decisions

| Decision | Choice | Why |
|----------|--------|-----|
| Inter-node RPC | gRPC + protobuf | Typed contract, connection pooling, timeout via `context` |
| Client protocol | Plaintext TCP | Zero dependency for clients; works with `nc` for debugging |
| WAL encoding | `encoding/gob` + 4-byte length prefix | Typed, forward-compatible; length prefix enables partial-tail detection on crash |
| WAL durability | `fsync` per record | Strict durability: committed = on disk, no exceptions |
| Snapshot write | temp file → fsync → `os.Rename` | `rename(2)` is atomic; previous snapshot never lost on crash |
| Goroutine shutdown | `stopCh chan struct{}` closed by `Stop()` | Channel close broadcasts to all goroutines instantly; prevents a dead node's heartbeats from blocking re-election |

For the full engineering rationale see [DEVELOPMENT_JOURNAL.md](DEVELOPMENT_JOURNAL.md).

---

## Tests

```bash
go test -race ./raft/
```

17 tests covering leader election, log replication, split-brain recovery, WAL crash recovery, TCP redirect, snapshot atomicity, and a 7-step end-to-end smoke test that simulates a TitanSync client through a full leader kill and re-election cycle. All pass with zero data races.

---

## Project Structure

```
├── cmd/server/main.go          # entrypoint: flags, boot sequence, gRPC + TCP
├── proto/raft.proto            # wire contract for Raft RPCs
├── raft/
│   ├── node.go                 # RaftNode: election, replication, WAL, snapshot
│   ├── wal.go                  # FileWAL: binary WAL with fsync and Truncate
│   ├── snapshot.go             # atomic snapshot write/read
│   ├── tcp.go                  # TCP client server: GET/SET/DELETE
│   ├── peers.go                # --peers flag parser
│   └── *_test.go               # unit + integration tests
└── scripts/
    ├── start-cluster.sh
    ├── stop-cluster.sh
    └── chaos-demo.sh
```

---

## Dependencies

```
google.golang.org/grpc     v1.60.1
google.golang.org/protobuf v1.32.0
```

No external testing libraries.
