# Concurrency verification choices

Icarus's concurrency boundary is multiple processes contending on SQLite and
delivering hook output. A Rust thread scheduler alone cannot explore that
boundary. The implementation combines public-history checking with forced
subprocess schedules, finite models, and deliberately broken programs.

| Method | Decision and evidence |
|---|---|
| Porcupine | Use a coupled executable model over actual CLI/MCP histories. Shared operation IDs and identities prevent naïve partitioning by mailbox or cell. Bound crash effects by process death; a successful later retry alone cannot establish earlier commitment. [Porcupine](https://github.com/anishathalye/porcupine) |
| Controlled process faults | Use causal barriers around arm/read/fetch/injection/commit, then kill or resume real helpers. Preserve histories and public audits. This exercises the actual SQLite/OS boundary. [etcd robustness testing](https://github.com/etcd-io/etcd/blob/main/tests/robustness/README.md) |
| TLA+ / TLC | Exhaustively explore configured finite abstractions. Share transaction rules with recorded trace validation and require complete trace consumption. Bounds and fairness remain part of the claim. [TLC](https://lamport.org/tla/tools.html), [trace validation](https://docs.tlapl.us/using:tlc:trace_validation) |
| Loom / Shuttle | Defer for the database protocol: they explore instrumented Rust concurrency, not arbitrary child processes or SQLite internals. They could later target helper-local cancellation logic. [Loom](https://github.com/tokio-rs/loom), [Shuttle](https://github.com/awslabs/shuttle) |
| Apalache | Defer. Bounded symbolic checking could extend model bounds but still needs explicit implementation correspondence. [Apalache](https://apalache-mc.org/docs/apalache/running.html) |
| Full deterministic simulation | Defer. Replacing time, storage, processes, and scheduling is a substantial architectural commitment. [FoundationDB testing](https://apple.github.io/foundationdb/testing.html) |
| SQLite VFS faults | Required before a power-failure claim. Process termination preserves the OS cache and does not simulate unsynced-write loss. [SQLite crash tests](https://sqlite.org/testing.html#crash_testing) |

Negative controls are mandatory: accepting correct examples alone does not
show that a checker detects the failure class it names. The suite checks both
fabricated illegal histories and compiled production mutants. Parser/build
errors do not count as detected protocol defects.

Research found two implementation defects: concurrent cold-start WAL
initialization could return SQLITE_BUSY despite a configured busy timeout,
and repeated assistant text aliased distinct Stop boundaries. Both now have
regression tests. SQLite does not guarantee invoking a busy handler on every
lock conflict; bootstrap now retries contention within a shared budget.
[SQLite busy handlers](https://sqlite.org/c3ref/busy_handler.html)

The previous bundled SQLite 3.51.1 was also in the affected range of SQLite's
WAL-reset corruption race. No corruption was reproduced in Icarus. The helper
now links 3.53.2 and rejects versions below 3.51.3, the fix boundary for that
release line. Its version command exposes the actual linked version and source
ID. [SQLite WAL-reset issue](https://sqlite.org/wal.html#walreset)
