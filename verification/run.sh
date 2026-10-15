#!/bin/sh
# Full local acceptance campaign. Native login/model tests remain opt-in.
set -eu
cd "$(dirname "$0")/.."
GO=${GO:-go}
unset ICARUS_TEST_CRASH ICARUS_TEST_DROP_WAKEUPS ICARUS_TEST_ARMED \
    ICARUS_TEST_PAUSE ICARUS_TEST_BARRIER_DIR ICARUS_TEST_NOTIFY_REPORT
out=${ICARUS_VERIFY_OUT:-"$PWD/verification/out/acceptance-$(date -u +%Y%m%dT%H%M%SZ)"}
mkdir -p "$out"
out=$(cd "$out" && pwd)
if [ -f "$out/version.json" ]; then
    echo "Refusing to overwrite an existing campaign: $out" >&2
    exit 1
fi
make build check GO="$GO" > "$out/build.log" 2>&1
cp bin/icarus-sync "$out/icarus-sync"
"$out/icarus-sync" version > "$out/version.json"
shasum -a 256 "$out/icarus-sync" > "$out/binary.sha256"
uname -a > "$out/platform.txt"
cargo test --locked --test protocol --test mcp > "$out/protocol.log" 2>&1
"$GO" test -count=1 ./... > "$out/launcher.log" 2>&1
ICARUS_SYNC_BIN="$out/icarus-sync" ICARUS_STARTUP_ROUNDS=100 \
    ICARUS_REQUIRE_DATAGRAM=1 ICARUS_VERIFY_REPORT_DIR="$out/faults" \
    cargo test --locked --test verification -- --test-threads=1 \
    > "$out/faults.log" 2>&1
(
    cd verification/linear
    "$GO" vet ./... > "$out/checker-vet.log" 2>&1
    "$GO" test -race ./... > "$out/checker-tests.log" 2>&1
    "$GO" run . --binary "$out/icarus-sync" --histories 1000 --calls 50 \
        --jobs 4 --seed 20261014 --output "$out/linear" \
        > "$out/linear.log" 2>&1
)
ICARUS_SYNC_BIN="$out/icarus-sync" sh verification/check.sh \
    > "$out/formal.log" 2>&1
mkdir -p "$out/formal"
cp verification/*.tla verification/*.cfg "$out/formal/"
for name in PrematureAck StaleSnapshot NonAtomicReplay AliasProviders \
    ShallowLineage StaleRebind TextBoundary; do
    cp "verification/out/$name.cfg" "$out/formal/"
done
for name in Transactions Mailbox Identity StopGuard RecordedTrace BrokenTrace \
    BrokenReplay BrokenConflict PrematureAck StaleSnapshot NonAtomicReplay \
    AliasProviders ShallowLineage StaleRebind TextBoundary; do
    cp "verification/out/$name.log" "$out/formal/"
done
for name in RecordedTrace BrokenTrace BrokenReplay BrokenConflict; do
    cp "verification/out/$name.tla" "verification/out/$name.cfg" "$out/formal/"
done
for name in history history-summary formal-summary; do
    cp "verification/out/$name.json" "$out/formal/"
done
ICARUS_MUTANT_OUT="$out/mutants" node verification/mutants.mjs \
    > "$out/mutants.log" 2>&1
cargo run --release --locked --example bench -- "$out/icarus-sync" \
    > "$out/benchmark.json" 2> "$out/benchmark-build.log"
printf 'passed\n' > "$out/STATUS"
echo "Acceptance campaign passed: $out"
