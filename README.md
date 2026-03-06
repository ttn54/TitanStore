# TitanStore

A distributed key-value database built from scratch in Go, implementing the Raft consensus algorithm with full disk persistence, log compaction, and a plaintext TCP client API.

**Author:** Zen Nguyen · **Language:** Go 1.21 · **Status:** Complete

---

## What This Is

TitanStore is a production-modelled implementation of a fault-tolerant key-value store. It is not a toy — it ships a real binary WAL with crash-safe partial-record recovery, atomic fsync snapshots, and a Raft engine that has been validated under race detector and chaos tests.

It is the backend database for **TitanSync**, a file-sync daemon that uses TitanStore as its highly available, replicated state store.

---

## Feature Completion

| Phase | Feature | Status |
|-------|---------|--------|
| 1 | Leader election (randomized timeouts, majority vote) | ✅ Complete |
| 1 | Log replication (AppendEntries, commit quorum) | ✅ Complete |
| 1 | Split-brain prevention (term enforcement) | ✅ Complete |
| 1 | Heartbeat-based failure detection | ✅ Complete |
| 2 | Write-Ahead Log — binary length-prefix + gob encoding | ✅ Complete |
| 2 | Crash-safe WAL recovery (partial tail drop) | ✅ Complete |
| 2 | State machine: SET / DELETE | ✅ Complete |
| 2 | TCP client API: GET / SET / DELETE | ✅ Complete |
| 3 | `currentTerm` / `votedFor` WAL persistence (Raft §5.4) | ✅ Complete |
| 3 | Dynamic cluster membership via `--peers` flag | ✅ Complete |
| 3 | Log compaction — atomic fsync snapshot + WAL truncation | ✅ Complete |
| 3 | `RaftNode.Stop()` — clean goroutine lifecycle | ✅ Complete |
| 3 | TitanSync end-to-end integration smoke test | ✅ Complete |

---

## Architecture

```
                    ┌─────────────────────────────────────────┐
                    │           TitanStore Cluster             │
                    ├───────────┬───────────┬──────────────────┤
                    │  Node 1   │  Node 2   │     Node 3       │
                    │ gRPC :5001│ gRPC :5002│  gRPC :5003      │
                    │ TCP  :6001│ TCP  :6002│  TCP  :6003      │
                    │           │           │                  │
                    │ Follower  │  LEADER   │   Follower       │
                    │           │           │                  │
                    │ log[...]←─┤─replicate─┤─→log[...]        │
                    │ WAL+snap  │ WAL+snap  │  WAL+snap        │
                    └───────────┴───────────┴──────────────────┘
                          ↑         ↑             ↑
                          └─────────┴─────────────┘
                              gRPC (Raft RPCs)
                              RequestVote / AppendEntries

          TCP clients (TitanSync daemon, nc, any plaintext client)
                    │              │              │
                    ▼              ▼              ▼
             GET /SET /DELETE — followers redirect to leader
```

### How It Works

**Leader Election**
- All nodes start as Followers with a randomised election timeout (150–300 ms).
- If no heartbeat arrives before the timeout, a node becomes a Candidate, increments its term, votes for itself, and sends `RequestVote` RPCs.
- First candidate to collect a strict majority becomes Leader for that term.
- `currentTerm` and `votedFor` are flushed to the WAL (as `RecordTypeTermVote`) before any state change so they survive crashes — per Raft §5.4.

**Log Replication**
- Leader receives a `SET` or `DELETE` command via TCP.
- Leader appends the entry to its in-memory log and to `disk_<id>.wal` before responding.
- Leader sends `AppendEntries` RPCs to all Followers; Followers write to their own WALs.
- When a strict majority acknowledges, the entry is committed and applied to `dataStore`.

**Fault Tolerance**
- Heartbeats every 50 ms keep followers anchored.
- On leader failure, the next election fires within one timeout window (max 300 ms).
- `RaftNode.Stop()` closes a `stopCh` channel, causing `sendHeartbeats` and `runElectionTimer` goroutines to exit cleanly — prevents a killed node's background goroutines from resetting the surviving cluster's election timers (a real split-brain hazard).

**Persistence and Recovery**
- Every write goes through the binary WAL (4-byte length prefix + gob payload, `fsync` on every record).
- On boot, WAL records are replayed in order: `RecordTypeEntry` rebuilds the log, `RecordTypeCommit` restores the commit pointer, `RecordTypeTermVote` restores `currentTerm`/`votedFor`.
- `TakeSnapshot()` serialises the full `dataStore` to disk atomically (`write temp → fsync → rename`) then truncates the WAL, capping recovery time to O(entries since last snapshot).
- On restart, two-phase recovery loads the snapshot first (if any), then replays only the WAL tail after `snapshotIndex`.

---

## Quick Start

### Prerequisites

- Go 1.21+
- `protoc` — only needed if you want to regenerate gRPC stubs (pre-generated files are committed)

### Build

```bash
go build -o bin/titanstore-server cmd/server/main.go
```

Or via Make:

```bash
make build
```

### Start a 3-Node Cluster

```bash
make cluster-start
```

Or manually — the same binary runs all three nodes; identity and topology come from flags:

```bash
go run cmd/server/main.go \
  -id=node1 -port=5001 -client-port=6001 \
  -advertise-client-addr=localhost:6001 \
  -peers="node2:localhost:5002:6002,node3:localhost:5003:6003" &

go run cmd/server/main.go \
  -id=node2 -port=5002 -client-port=6002 \
  -advertise-client-addr=localhost:6002 \
  -peers="node1:localhost:5001:6001,node3:localhost:5003:6003" &

go run cmd/server/main.go \
  -id=node3 -port=5003 -client-port=6003 \
  -advertise-client-addr=localhost:6003 \
  -peers="node1:localhost:5001:6001,node2:localhost:5002:6002" &
```

### Flag Reference

| Flag | Description |
|------|-------------|
| `-id` | Node identity — used as WAL filename (`disk_<id>.wal`) and snapshot filename |
| `-port` | gRPC port for inter-node Raft RPCs |
| `-client-port` | TCP port for the client API (GET / SET / DELETE) |
| `-advertise-client-addr` | TCP address this node advertises to peers for client redirects |
| `-peers` | Comma-separated peer list: `id:host:grpcPort:tcpPort,...` |

### Interact via TCP

Any TCP client works — `nc`, a custom client, or TitanSync:

```bash
# Write a value (must reach the leader, or be redirected)
echo 'SET user:1 {"name":"zen","ts":1741276800}' | nc localhost 6001

# Read from any node
echo 'GET user:1' | nc localhost 6002

# Delete
echo 'DELETE user:1' | nc localhost 6001
```

**Protocol reference — all messages are newline-terminated:**

| Request | Success response | Error response |
|---------|-----------------|----------------|
| `GET <key>` | `VALUE <value>` | `NOT_FOUND` |
| `SET <key> <value>` | `OK` | `ERR NOT_LEADER <tcp-addr>` |
| `DELETE <key>` | `OK` | `ERR NOT_LEADER <tcp-addr>` |

If a follower receives a write command it replies `ERR NOT_LEADER localhost:6002` (the leader's **TCP** address, not gRPC), so the client can reconnect directly.

### Stop Cluster

```bash
make cluster-stop
```

---

## Testing

```bash
# Full suite with race detector (required before every commit)
make test
# or:
go test -race ./raft/

# Individual tests
go test -v -run TestLeaderElection          ./raft/   # election convergence
go test -v -run TestLogReplication          ./raft/   # majority-quorum commit
go test -v -run TestSplitBrain              ./raft/   # leader kill + re-election
go test -v -run TestExecuteCommand_DELETE   ./raft/   # state machine
go test -v -run TestWAL_AppendAndReadBack   ./raft/   # WAL round-trip
go test -v -run TestWAL_CrashRecovery       ./raft/   # partial-tail drop
go test -v -run TestWAL_TermVote_RoundTrip  ./raft/   # term/vote persistence
go test -v -run TestRecoverFromWAL_RestoresTermAndVote ./raft/
go test -v -run TestWALRecovery_EndToEnd    ./raft/   # full power-cycle simulation
go test -v -run TestWAL_Truncate            ./raft/   # snapshot WAL truncation
go test -v -run TestTCPServer               ./raft/   # TCP handler
go test -v -run TestTCPServer_Redirect_ReturnsTCPAddr ./raft/
go test -v -run TestSnapshot_WriteAndRead   ./raft/   # snapshot round-trip
go test -v -run TestSnapshot_AtomicWrite    ./raft/   # crash-safe rename
go test -v -run TestTakeSnapshot_WritesFileAndTruncatesWAL ./raft/
go test -v -run TestRecoverFromWAL_WithSnapshot ./raft/
go test -v -run TestTitanSync_Integration   ./raft/   # 7-step end-to-end smoke test
```

All tests pass with zero data races.

---

## Code Structure

```
TitanStore/
├── cmd/server/
│   └── main.go                        # Binary entrypoint: flag parsing, WAL boot,
│                                      # gRPC + TCP server wiring, SIGINT shutdown
├── proto/
│   ├── raft.proto                     # gRPC service contract (2 RPCs: RequestVote,
│   │                                  # AppendEntries) — the wire format is final
│   ├── raft.pb.go                     # Generated — do not edit
│   └── raft_grpc.pb.go                # Generated — do not edit
├── raft/
│   ├── node.go                        # RaftNode: consensus state machine, election,
│   │                                  # replication, WAL hooks, snapshot, Stop()
│   ├── node_test.go                   # Election, replication, split-brain, DELETE
│   ├── wal.go                         # WAL interface + FileWAL (length-prefix gob,
│   │                                  # fsync, Truncate)
│   ├── wal_test.go                    # WAL unit tests + end-to-end recovery test
│   ├── tcp.go                         # TCPServer: plaintext GET/SET/DELETE,
│   │                                  # idle deadline, leader redirect
│   ├── tcp_test.go                    # TCP handler + redirect unit tests
│   ├── peers.go                       # ParsePeersFlag: id:host:grpcPort:tcpPort
│   ├── snapshot.go                    # Snapshot struct, WriteSnapshot (atomic
│   │                                  # temp→rename), ReadSnapshot
│   ├── snapshot_test.go               # Snapshot round-trip, atomic write, recovery
│   └── titansync_integration_test.go  # 7-step end-to-end smoke test
├── scripts/
│   ├── start-cluster.sh               # Start 3-node cluster with --peers flags
│   ├── stop-cluster.sh                # Kill cluster processes
│   └── chaos-demo.sh                  # Kill/restart nodes under load
├── CONTEXT.md                         # Design authority: no-rewrite boundaries
├── DEVELOPMENT_JOURNAL.md             # Exhaustive engineering decision log
├── Makefile
└── go.mod                             # module titanstore, Go 1.21
```

---

## Key Engineering Decisions

| Decision | Choice | Reason |
|----------|--------|--------|
| Inter-node RPC | gRPC + protobuf | Typed contract, compile-time safety, connection pooling |
| Client API | Plaintext TCP (`nc`-compatible) | Zero dependency for TitanSync, trivially debuggable |
| WAL serialisation | `encoding/gob` + 4-byte length prefix | Type-safe, crash-detectable partial tail records |
| WAL durability | `fsync` on every record | "If SET returns OK, it is on disk" — no exceptions |
| Snapshot write | `write-to-temp` → `fsync` → `os.Rename` | `rename(2)` is atomic on Linux; no partial-snapshot state |
| Snapshot trigger | Explicit `TakeSnapshot()` call | Separates concerns; avoids background goroutine lock contention |
| Goroutine shutdown | `stopCh chan struct{}` closed by `Stop()` | Broadcasts instantly to all goroutines; prevents ghost-leader heartbeats |
| Cluster config | `--peers id:host:grpcPort:tcpPort` flag | No external file, composable with shell, carries both address types |
| Leader redirect | Returns TCP address, not gRPC | A TCP client cannot speak gRPC; redirect must be a connectable address |

For the full rationale (alternatives considered, trade-offs accepted) see [DEVELOPMENT_JOURNAL.md](DEVELOPMENT_JOURNAL.md).

---

## Dependencies

```
google.golang.org/grpc    v1.60.1
google.golang.org/protobuf v1.32.0
```

No external testing libraries. The standard library `testing` package is used throughout.

