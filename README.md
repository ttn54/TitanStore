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

**Consensus:** Two gRPC RPCs implement the full Raft paper — `RequestVote` for elections and `AppendEntries` for replication and heartbeats. Election timeouts are randomised (150–300 ms) to prevent split votes; the leader sends heartbeats every 50 ms.

**Persistence:** Every log entry is written to a binary WAL (4-byte length-prefix + gob payload) and `fsync`'d before the RPC returns. `currentTerm` and `votedFor` are also WAL-persisted before any state transition, satisfying the Raft paper's §5.4 durability requirement.

**Snapshots:** `TakeSnapshot()` writes a gob-encoded snapshot of the full state machine to a temp file, calls `fsync`, then `os.Rename` — which is atomic on Linux. The WAL is then truncated so recovery time is bounded by state size, not log history.

**Leader redirect:** When a follower receives a write, it replies `ERR NOT_LEADER <addr>` where `<addr>` is the leader's **TCP** port (not gRPC), so the client can reconnect and retry directly.

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
