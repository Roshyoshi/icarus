# Verification results — 2026-10-14

The expanded automated campaign passed on macOS. It found and fixed two real
implementation bugs and replaced an affected SQLite dependency. Linux runtime
verification and authenticated native tests remain open; this is not complete
release acceptance.

Tested release SHA-256:
`09717a512a487987c55cdb68af80d4933ada3602861f76e0f9054687b3beae62`.
Linked SQLite: **3.53.2**, source ID
`d6e03d8c777cfa2d35e3b60d8ec3e0187f3e9f99d8e2ee9cac695fd6fcdf1a24`.
Environment: macOS 26.5.1 arm64, Rust 1.96.0, Go 1.26.8, Node 24.15.0,
Java 21.0.11. [Exact environment](results/2026-10-14/environment.json).

| Check | Result | Evidence |
|---|---|---|
| Coupled public-history checking | **1,000/1,000 Ok**, 50,000 workload calls; no Illegal/Unknown/transport failure | [Summary](results/2026-10-14/linear/summary.json), [complete compressed histories](results/2026-10-14/linear/archive.json) |
| Cold database initialization | **3,200/3,200 successes**: 100 fresh groups × 32 helpers | [Every round](results/2026-10-14/faults/cold_startup.json) |
| Mutation × crash boundary | **24/24 passed**: 8 mutations × before commit / after commit / after notification | [Matrix](results/2026-10-14/faults/mutation_crash_matrix.json) |
| Forced wait schedules | **18/18 passed**: messages/tasks/cells × 3 boundaries × enabled/dropped notifications | [Schedules and actual sends](results/2026-10-14/faults/forced_wait_schedules.json) |
| Additional boundary tests | Cancellation/EOF, three waiter restart kinds, two hook races, four descendant levels, Stop batch progress, two injection crash boundaries passed | [Nine-test run](results/2026-10-14/faults-v2.log) |
| Compiled production mutants | **5/5 detected** after the unmodified baseline passed | [Summary](results/2026-10-14/mutants/summary.json) |
| Undecided-check control | Forced Porcupine timeout returned Unknown and exit 1 | [Separate control](results/2026-10-14/linear/timeout-control/summary.json) |
| Existing Rust and Go fixtures | 9 original Rust tests plus 9 new tests passed; 6 Go fixture tests passed uncached | [Rust/checks](results/2026-10-14/checks-final.log), [Go](results/2026-10-14/launcher-final.log) |
| Static checks | Formatting, Clippy with warnings denied, Go vet passed | [Log](results/2026-10-14/checks-final.log) |

The history campaign used 4 independent history jobs and concurrency
2/4/8 within each group. It recorded **86,912 total calls**: 50,000 workload
calls plus 36,912 setup, lifecycle, crash/recovery, and public audit calls.
Every history had overlapping workload intervals; there were
76,059 overlapping request-submission/return pairs.
These are client intervals, not instrumentation of SQLite lock overlap. All
nine fault-class × concurrency combinations were covered.
An [independent artifact audit](results/2026-10-14/linear/independent-audit.json)
recomputed these counts, ordering constraints, and hashes from the raw records.

All 333 deterministic post-commit crashes exposed the same durable message
before replay. All 333 random SIGKILL cases showed no effect in the pre-replay
read; random sampling did not hit both sides of commit. The checker unit cases
exercise both unknown-outcome branches. The 334 deterministic pre-commit
crashes also showed no effect before replay.

The checker reads public CLI/MCP results, never the journal as its ordering
oracle. Interrupted operations remain bounded by their original process-death
interval; later replay alone cannot establish earlier commitment. Raw histories
include errors, replays, ownership rejection, stale generations, and audits.
Seeds reproduce inputs, not the operating-system schedule. The separate timeout
control and superseded pilot runs are excluded from the 1,000 passing histories.

The forced wait run confirmed six successful datagram sends after arming. Three
before-arm enabled cases correctly had no target; nine cases deliberately
dropped every hint. After-empty-read polling observed changes in roughly
1.03 seconds under concurrent test load. Every applicable fault fixture checked
SQLite integrity and foreign keys. Process termination does not simulate power
loss, torn writes, faulty fsync, or storage hardware.

TLC checked these separate finite models:

| Model | Generated states | Distinct states | Depth |
|---|---:|---:|---:|
| Transactions | 56,148 | 7,116 | 16 |
| Mailbox | 1,086,339 | 87,704 | 21 |
| Identity | 486 | 75 | 8 |
| StopGuard | 727 | 125 | 9 |
| RecordedTrace | 18 | 18 | 18 |

All stated properties passed within the [bounds and fairness assumptions](FORMAL.md).
All seven broken models and three corrupted implementation traces failed at
their intended properties. The 17-call trace includes 12 durable operations,
three successful replays, two errors, and a committed call without a reply; it
uses the same transaction evaluator as model exploration. This is sampled API
conformance, not a proof that arbitrary Rust executions refine TLA+.
[Formal summary](results/2026-10-14/formal/formal-summary.json),
[trace summary](results/2026-10-14/formal/history-summary.json).

The original release failed **28/720** concurrent cold starts; the new run had
zero failures across 3,200 starts. The workloads differ (24 versus 32 helpers
per group), so these counts are observations, not a production failure-rate
estimate. Bootstrap retries now handle WAL-init busy errors using one retry
budget. Repeated assistant text also no longer aliases Stop boundaries: without
a native turn ID, the guard uses the outstanding mailbox batch.
[Before-fix evidence](results/2026-10-14/before-fix/metadata.txt).

SQLite 3.51.1 was affected by the upstream WAL-reset corruption issue; no such
corruption was reproduced here. The runtime version gate and bundled upgrade
address that dependency exposure. [Method/dependency sources](METHODS.md).

Benchmarks ran after the history workload completed: 500 durable CAS calls,
25 commit-return-to-observation samples, and 25 empty hook subprocesses per run.
The writer/reader use separate connections in one process; hook timing includes
process startup and database opening. Throughput and wake timing link the
current release library; hooks execute the frozen release helper. No model
latency is included.

| Measurement | Datagrams available | Sandbox polling fallback |
|---|---:|---:|
| Bound datagram sockets | 25/25 | 0/25 |
| Commit return → observation, median | 0.566 ms | 997.024 ms |
| Commit return → observation, p95 | 1.723 ms | 1000.476 ms |
| Durable CAS throughput | 199.7 calls/s | 240.0 calls/s |
| Empty hook process, median | 6.821 ms | 6.449 ms |
| Empty hook process, p95 | 7.755 ms | 10.274 ms |

[All datagram samples](results/2026-10-14/benchmark-datagrams.json),
[all polling samples](results/2026-10-14/benchmark-polling.json). The two runs use
different sandbox permissions, so their hook timings are not a controlled
performance comparison. These small local samples are not scaling results or
a latency SLO. Signed observation offsets preserve any visibility that precedes
the writer returning from COMMIT rather than clamping it to zero.

Fresh native logged-out diagnostics passed for Codex and Claude, including a
Codex settings profile and invalid ambient credential fixtures.
[Native diagnostic log](results/2026-10-14/native-diagnostics.log).

Still open: Linux runtime execution (the configured local builder timed out),
authenticated A/B account isolation, reopening a real login, and real
Codex-to-Claude subagent exchange. The user explicitly deferred those live
login/model tests. Power-loss/VFS fault injection is also outside this campaign.
The supplied macOS/Linux CI workflow has not been run remotely in this session.

Reproduce with `make verify-sync`; see [commands and dependencies](README.md).
Current evidence is under `results/2026-10-14/`. Older files directly under
`results/` are superseded measurements, not part of these counts.

To reconstruct the exact archived history without rerunning native helpers:

```sh
gzip -dc verification/results/2026-10-14/linear/histories-*.jsonl.gz > /tmp/icarus-history.jsonl
shasum -a 256 /tmp/icarus-history.jsonl
```

Expected SHA-256:
`56ae5f64e0eb1b51930733f790e4f4bd4c3a9fb2dd519fcc16cce1c03f1d8c82`.
