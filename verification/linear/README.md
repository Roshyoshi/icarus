# Real-process linearizability campaign

Run from this directory. The verifier uses Porcupine v1.3.0 with an independent
sequential model of the public JSON protocol. It launches the actual Rust
helper through both CLI and persistent MCP stdio connections. It never reads
SQLite tables, the operation journal, or the implementation's transaction order.

```sh
go test -race ./...
go run . --binary ../../target/release/icarus-sync \
  --histories 1000 --calls 50 --seed 20261014 --jobs 4 \
  --output ../out/linear
```

The output directory must not already contain `histories.jsonl`. No credentials
or native model calls are involved. Each history gets its own temporary home
and database, which are removed after its subprocesses close.

`--jobs` defaults to 1. It controls independent histories, not concurrency within
a group. History indices alternate among 2, 4, and 8 concurrent clients. Each
history contains exactly the requested number of workload calls, at least 32.
Setup, lifecycle, crash/recovery, and public state audits are counted separately.

The 32 scheduled workload calls guarantee sends to several recipients, a hot
CAS race and a second cell, partial prefix acknowledgement, duplicate operation
replays, changed-argument rejection, task-owner rejection, and competing task
finishes. Seeded generated calls fill the remaining quota. Seeds vary requests,
route choices, operation types, and scheduling groups; they do not control the
OS scheduler. Each history also exercises main/subagent IDs, two providers,
resume with stale-generation rejection, and public reads that drain every
mailbox and inspect every used cell and task. `wait_any`, native hook behavior,
and bootstrap races belong to the separate production fault suite.

The model remains coupled across members, mailboxes, cells, tasks, and operation
IDs. There is no per-key partitioning that could hide a cross-object violation.
It checks successful results, stable replay results, and the allowed semantic
errors. A transport failure cannot stand in for a semantic rejection.

## Crashes and ambiguous outcomes

Each history rotates among these fault cases:

- `before_commit`, exit 86: known abort; the model must retain the old state.
- `after_commit`, exit 86: known commit; the model must apply the effect.
- SIGKILL without a complete reply: unknown; the model allows either no effect
  or an effect within the original invocation/death interval.

Fault selection advances every three histories; concurrency advances every
history. Every nine histories cover the full three-by-three combination. The
summary reports that cross-product and distinguishes SIGKILL with a completed
reply from SIGKILL without a reply.

An identical public replay supplies a candidate result, including a generated
message ID. It does not establish that the first attempt committed: the retry
could have committed it. A public inbox read **before** the replay constrains
which alternative can explain the history. A complete reply observed before a
deliberate kill remains a completed operation, with its actual exit status
recorded. Ordinary CLI responses must agree with exit 0 (success) or 1 (error).

The checker accepts a history only if a legal atomic operation order respects
all observed invocation/response intervals. Its per-history timeout defaults to
five seconds. `Unknown`, `Illegal`, and infrastructure failures fail the campaign;
none counts as a pass. Already-running histories finish before the campaign
stops scheduling more work.

## Artifacts and reproduction

- `histories.jsonl`: one complete history per line, ordered by history index;
  every request, response, crash classification, invocation timestamp, request
  submission timestamp, and response/death timestamp is retained.
- `summary.json`: exact counts by route, method, phase, concurrency and outcome;
  actual overlap, checker timing, binary/SQLite version, source hashes, and the
  complete history-file hash.
- `failure-seed-N.html`: Porcupine's interactive counterexample or timeout
  visualization when checking fails.

Rerun a single case without changing its concurrency or fault selection:

```sh
go run . --binary ../../target/release/icarus-sync \
  --seed 20261014 --start-index 743 --histories 1 --calls 50 \
  --output ../out/repro-743
```

This regenerates the same requests and route decisions. The recorded history
is the evidence for the original OS schedule; a rerun can schedule differently.
The unit tests contain seven distinct deliberately false protocol histories
and six crash-observation combinations. They also accept valid concurrent CAS,
partial acknowledgement, and historical inbox replay examples, and reject
an error response used as evidence of a known committed effect.

Counts describe finite executions. They do not prove all schedules, external
model consumption, disk power-loss behavior, native identity payloads, or wait
liveness. The overlapping intervals show outstanding submitted requests, not
instrumented overlap inside SQLite's write lock.

Primary checker documentation:
[Porcupine API and nondeterministic models](https://pkg.go.dev/github.com/anishathalye/porcupine@v1.3.0).
