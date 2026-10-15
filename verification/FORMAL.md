# Formal checks and their implementation boundary

Run `ICARUS_SYNC_BIN=/absolute/path/icarus-sync TLA_JAR=/absolute/path/tla2tools.jar sh verification/check.sh` with the exact acceptance binary. Java and Node are required; native credentials are not. Generated logs, bounds, state counts, exploration depth, elapsed time, and TLC/Protocol hashes are in `verification/out/formal-summary.json`. The history summary records the binary hash.

These checks establish finite model exploration and sampled API conformance. They do not prove that every Rust execution refines the specification. Missing tools, parser errors, unexpected failures, and unexpectedly successful mutants fail the command.

## Shared transaction semantics

`Protocol.tla` contains the canonical evaluator. `Transactions.tla` uses it to explore commit, replay, reply, crash, and restart actions. `Mailbox.tla` uses its mailbox command/result rules. `Trace.tla` uses the same evaluator to check responses projected from a recorded implementation run.

The abstraction contains one mailbox, cell, and task. A successful command atomically changes durable state and inserts its argument/reply record. A matching operation-ID retry returns the recorded reply without reapplying the command. Changed arguments fail. A conflicting task finish fails without changing its terminal result or reserving its operation ID. A losing CAS succeeds as an operation with `swapped = false` and records that result. An old acknowledgement remains valid after its batch has been fully consumed.

SQL transactions are atomic actions. SQLite lock acquisition, individual SQL statements, disk writes, fsync, and checkpoints are outside the model. A helper crash discards volatile reply state but preserves committed state by assumption. Production process-crash and history tests provide separate evidence; I/O-fault and power-loss testing remain deferred.

## Bounds and properties

| Model | Bound | Checked contract |
|---|---|---|
| Transactions | Five operation IDs: one send, two CAS at revision zero, two conflicting finishes | One committed effect per ID, at most one CAS winner, immutable task result/replay, unsuccessful finish has no replay row |
| Mailbox | Three messages, two concurrent hook fetches, batch sizes one or two, at most one crash | Explicit acknowledgement only, ordered outstanding prefix, independent fetch/inject/ack, eventual fresh observation |
| Identity | Main, child, sibling, grandchild, other provider; two generations | Distinct scoped bindings, immutable parent relationship, no stale-ancestor call, no generation rollback |
| StopGuard | One member/launch, two hooks, two successive batches, repeated assistant text | At most one permitted injection per fallback batch boundary; a new batch remains eligible |
| Recorded trace | Seventeen calls; one mailbox/cell/task; one known post-commit crash | Successes, errors, retries, ID reuse after rollback, and committed work without a reply match the shared evaluator |

`Mailbox` does not assume a client must wait for hook injection before explicitly acknowledging a fetched prefix. Injection can occur after acknowledgement if a hook already fetched its output. The invariant forbids automatic acknowledgement as a side effect of fetch. No exactly-once consumption property is asserted.

`Mailbox` separates arm, database read, sleep, timeout wake, notification wake, notify, and drop. Notifications may all be dropped. Weak fairness of restart, arm, poll-deadline expiry, and fresh reads requires eventual scheduling and database access. At most one crash makes eventual stable recovery explicit. The temporal property is eventual observation by an uncancelled pending waiter; it does not establish a wall-clock bound or eventual model/hook consumption.

`StopGuard` checks the fallback used when no native turn ID is supplied: `batch:{batch_end}`. Native turn IDs are assumed distinct boundaries and are not enumerated here. The durable claim precedes injection. A crash after claim may suppress automatic injection for that boundary; no eventual-injection property is asserted. Native recursion flags and hook trust checks are adapter behavior covered outside this model.

`Identity` models scope keys as distinct symbols. It cannot establish that a native adapter constructs those keys correctly. The grandchild case exercises the difference between checking one parent and checking every ancestor.

## Recorded execution and independent checks

`histories.mjs` records invocation/response intervals and searches an independent JavaScript reference machine for a legal order. It includes three successful replays, two errors, twelve durable operation records, and one process killed after commit without a reply. The known committed operation is constrained to precede process death and is never discarded. The journal supplies its durable result; subsequent inbox reads expose its effect independently.

The selected legal call order is projected into `Protocol.tla`, including failed calls and replay attempts. It is not merely the journal's commit order. TLC checks both response agreement and complete consumption of the input trace, so a blocked or short trace cannot pass vacuously.

The projection abstracts payloads to JSON strings, UUID freshness to distinct symbols, and error wording to semantic categories. It omits read-only calls, uncertain pre-commit outcomes, multiple objects, ownership checks, native identity events, waits, hook output, SQL pages, and process scheduling. The campaign's broader independent history checker remains necessary.

## Negative controls

| Broken model | Required failure | Corresponding executable mutant |
|---|---|---|
| NonAtomicReplay | `Safety` | `non_atomic_replay` |
| PrematureAck | `Safety` | `premature_ack` |
| StaleSnapshot | `EventuallyObserve` | `stale_snapshot` |
| ShallowLineage | `Lineage` | `ignore_ancestor_generation` |
| TextBoundary | `NewBatchAvailable` | `assistant_text_boundary` |
| AliasProviders | `Distinct` | Scope regression fixtures; no matching executable mutant |
| StaleRebind | `Monotonic` | Stale-generation regression fixtures; no matching executable mutant |

Executable mutants are compiled transformations managed separately by `mutants.mjs`; a broken model failing does not count as detecting a broken implementation. `BrokenTrace`, `BrokenReplay`, and `BrokenConflict` corrupt an actual recorded trace and must fail `TraceAccepted`. The JavaScript history checker independently rejects omission of the unanswered committed send and a fabricated second CAS success.

Primary method references: [TLA+ trace validation](https://docs.tlapl.us/using:tlc:trace_validation), [durable linearizability](https://cs.rochester.edu/u/scott/papers/2016_DISC_persistence.pdf), [SQLite crash testing](https://www.sqlite.org/testing.html#crash_testing).
