# Verification

`make verify-sync` builds a release helper and runs fault schedules, generated
histories, and the recorded trace against a frozen copy. Existing fixtures use
the debug helper; production mutants use separate builds. It needs Rust, Go, Node, Java,
and permission to create local Unix datagram sockets. It makes no model calls
and uses no native credentials. The [measured results](RESULTS.md) distinguish
completed checks from release acceptance that remains open.

```sh
sh verification/download-tlc.sh
make verify-sync

# An existing pinned jar and a Go binary outside PATH also work:
TLA_JAR=/absolute/path/tla2tools.jar GO=/absolute/path/go make verify-sync
```

Each campaign writes a new directory under `verification/out/`, with the exact
helper binary/hash, SQLite version/source ID, logs, raw histories, model traces,
counterexamples, fault reports, and benchmark samples. Every command must exit
successfully. The macOS/Linux CI matrix runs this command and uploads artifacts;
supplying a workflow does not establish that a remote job has run.

## Acceptance gates

| Layer | Required result |
|---|---|
| Build and fixtures | Formatting, Clippy, Go vet, Rust protocol/MCP tests, Go provider fixtures pass |
| Cold initialization | 100 fresh databases × 32 helpers paused before open; zero failures; valid integrity and foreign keys |
| Mutation recovery | 8 mutation types × 3 crash boundaries; one durable effect and stable replay |
| Forced waiting | 3 wait kinds × 3 boundaries × 2 notification policies; all observe the committed change |
| Boundary cases | Cancellation, EOF, killed waiters, fetch/injection crashes, ack/resume races, four descendant levels, repeated Stop text |
| Public histories | 1,000 seeds × 50 workload calls at concurrency 2/4/8; every coupled Porcupine check returns `Ok` |
| Formal models | Safety and stated liveness pass within recorded bounds; all seven broken models and three corrupted traces fail as intended |
| Executable controls | All five compiled production mutants fail their designated test after the unmodified baseline passes |

Setup, audits, lifecycle changes, and recovery calls are counted separately
from the 50,000 workload calls. The history checker includes reads, errors,
retries, operation-ID collisions, ownership checks, resumed identities, and
interrupted operations. It reads public interfaces, not SQLite's transaction
journal. `Illegal`, `Unknown`, timeout, or transport failure fails the campaign.
The [checker documentation](linear/README.md) describes its intervals, crash
semantics, workload shape, and single-seed reproduction.

The wait suite forces a writer to commit before arming, after arming, or after
an empty read. Enabled-notification cases after arming require a bound socket
and successful datagram send. Before-arm cases intentionally have no target.
Dropped-notification cases require the drop report and still complete by fresh
polling. Recorded latency is writer-response to waiter-response, not the exact
SQLite commit instant.

## Run layers separately

```sh
make test check build
ICARUS_SYNC_BIN="$PWD/bin/icarus-sync" ICARUS_STARTUP_ROUNDS=100 \
  ICARUS_REQUIRE_DATAGRAM=1 ICARUS_VERIFY_REPORT_DIR="$PWD/verification/out/faults" \
  cargo test --locked --test verification -- --test-threads=1

cd verification/linear
go test -race ./...
go run . --binary ../../bin/icarus-sync --histories 1000 --calls 50 \
  --jobs 4 --seed 20261014 --output ../out/linear
cd ../..

ICARUS_SYNC_BIN="$PWD/bin/icarus-sync" make model
node verification/mutants.mjs
make bench
```

The barriers and fault switches are explicit `ICARUS_TEST_*` environment
variables. The launcher excludes them from native sessions. A barrier writes
`<point>.ready`, waits for `<point>.release`, and fails after 15 seconds. Each
selected helper pauses once. Mutation tests compile disposable source copies;
they never modify production sources. The stale-snapshot mutant is detected by
stalled observation under a controlled schedule; its timeout alone is not a
unique diagnosis of stale SQLite state.

See [method selection and primary sources](METHODS.md) and
[formal bounds, assumptions, and implementation correspondence](FORMAL.md).
TLC's pinned SHA-256 is
`0f56d6a813368c48008dfbecb23a73ab43a4a5e1bf09f026782a674aa9d4b3c9`.
The `v1.8.0` asset identifies itself as TLC 2026.10.13.164326, revision
210df3a; the checksum pins bytes independently of the release label.

Process crashes do not simulate lost writes, torn pages, power failure, or
faulty storage. Finite checks do not prove arbitrary Rust executions, native
scheduling, exactly-once model consumption, or external actions. CAS only
governs coordination state. Native login and real subagent exchange remain
separate acceptance tests.

## Native diagnostics and deferred live acceptance

```sh
ICARUS_REAL_CLI_TESTS=1 go test -count=1 ./internal/launcher \
  -run TestRealLoggedOutDiagnostics -v
```

This creates disposable homes, supplies invalid ambient credential fixtures,
runs native auth/config diagnostics, and exits through native help. It includes
a Codex profile. It neither logs in nor runs models.

The user deferred authenticated tests. When ready, use a separate runtime home
and sign in through the native CLIs. Labels are not assertions of accounts.

```sh
export ICARUS_HOME="$HOME/.local/share/icarus-live-test"
icarus new codex a --group login-check
icarus new codex b --group login-check    # must still start logged out
icarus open codex a -- login status      # must retain A's login
icarus new claude a --group login-check
icarus new claude b --group login-check  # must still start logged out
icarus open claude a -- auth status --json

# Uses authenticated A instances and may spend model credits.
node verification/live.mjs --codex a --claude a --icarus "$(command -v icarus)"
```

The live test checks direct child-to-child messages, acknowledgement, and
explicit task completion. It retains stdout/stderr and a fixture-group journal.
Missing tools, denied hooks, failed login, and unmet assertions fail the test;
native approvals and hook trust remain in force.
