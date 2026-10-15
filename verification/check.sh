#!/bin/sh
set -eu
cd "$(dirname "$0")"
jar=${TLA_JAR:-../.tools/tla2tools.jar}
if [ ! -f "$jar" ]; then
    echo 'Set TLA_JAR to tla2tools.jar (see verification/README.md).' >&2
    exit 1
fi
mkdir -p out
for model in Transactions Mailbox Identity StopGuard; do
    java -XX:+UseParallelGC -jar "$jar" -noGenerateSpecTE -workers 2 -metadir "out/$model-states" \
        -config "$model.cfg" "$model.tla" > "out/$model.log" 2>&1
    grep 'Model checking completed. No error' "out/$model.log"
done
ICARUS_VERIFY_OUT="$(pwd)/out" node histories.mjs
java -XX:+UseParallelGC -jar "$jar" -noGenerateSpecTE -workers 1 \
    -metadir out/recorded-states -config out/RecordedTrace.cfg out/RecordedTrace.tla \
    > out/RecordedTrace.log 2>&1
grep 'Model checking completed. No error' out/RecordedTrace.log
for trace in BrokenTrace BrokenReplay BrokenConflict; do
    if java -XX:+UseParallelGC -jar "$jar" -noGenerateSpecTE -workers 1 \
        -metadir "out/$trace-states" -config "out/$trace.cfg" "out/$trace.tla" \
        > "out/$trace.log" 2>&1; then
        echo "ERROR: corrupted implementation trace $trace passed" >&2
        exit 1
    fi
    grep 'Invariant TraceAccepted is violated' "out/$trace.log"
done
for mutation in PrematureAck StaleSnapshot NonAtomicReplay AliasProviders ShallowLineage StaleRebind TextBoundary; do
    model=Mailbox
    if [ "$mutation" = NonAtomicReplay ]; then model=Transactions; fi
    case "$mutation" in AliasProviders|ShallowLineage|StaleRebind) model=Identity ;; esac
    if [ "$mutation" = TextBoundary ]; then model=StopGuard; fi
    sed "s/$mutation = FALSE/$mutation = TRUE/" "$model.cfg" > "out/$mutation.cfg"
    if java -XX:+UseParallelGC -jar "$jar" -noGenerateSpecTE -workers 1 -metadir "out/$mutation-states" \
        -config "out/$mutation.cfg" "$model.tla" > "out/$mutation.log" 2>&1; then
        echo "ERROR: deliberately broken $mutation passed" >&2
        exit 1
    fi
    case "$mutation" in
        StaleSnapshot) grep 'Temporal property EventuallyObserve was violated' "out/$mutation.log" ;;
        AliasProviders) grep 'Invariant Distinct is violated' "out/$mutation.log" ;;
        ShallowLineage) grep 'Invariant Lineage is violated' "out/$mutation.log" ;;
        StaleRebind) grep 'Action property Monotonic is violated' "out/$mutation.log" ;;
        TextBoundary) grep 'Invariant NewBatchAvailable is violated' "out/$mutation.log" ;;
        *) grep 'Invariant Safety is violated' "out/$mutation.log" ;;
    esac
done
TLA_JAR="$jar" node <<'JS'
const fs=require('node:fs'),crypto=require('node:crypto');
const models=['Transactions','Mailbox','Identity','StopGuard','RecordedTrace'];
const bounds={
  Transactions:'five operation IDs; one mailbox/cell/task; two competing CAS and finishes',
  Mailbox:'three messages; two hooks; batch limits 1..2; at most one crash',
  Identity:'five members including grandchild; two launch generations',
  StopGuard:'two hooks; two successive batches; repeated assistant text; no native turn ID',
  RecordedTrace:'17 calls, one mailbox/cell/task, one known post-commit crash'
};
const sha=file=>crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex');
const summary={
  tlc_sha256:sha(process.env.TLA_JAR),protocol_sha256:sha('Protocol.tla'),
  checked_at:new Date().toISOString(),models:models.map(model=>{
    const log=fs.readFileSync('out/'+model+'.log','utf8');
    const counts=log.match(/([\d,]+) states generated, ([\d,]+) distinct states found, 0 states left on queue/);
    if(!counts)throw Error('missing complete exploration counts: '+model);
    return {model,bound:bounds[model],generated:Number(counts[1].replaceAll(',','')),
      distinct:Number(counts[2].replaceAll(',','')),
      depth:Number(log.match(/depth of the complete state graph search is (\d+)/)[1]),
      elapsed:log.match(/Finished in (.+) at/)[1],result:'pass'};
  }),
  rejected_model_mutants:['PrematureAck','StaleSnapshot','NonAtomicReplay',
    'AliasProviders','ShallowLineage','StaleRebind','TextBoundary'],
  rejected_trace_mutants:['BrokenTrace','BrokenReplay','BrokenConflict'],
  claim:'finite abstraction checks and sampled API trace conformance; not full refinement or power-loss verification'
};
fs.writeFileSync('out/formal-summary.json',JSON.stringify(summary,null,2)+'\n');
JS
