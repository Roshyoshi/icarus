import {readFileSync, writeFileSync} from 'node:fs';
import {createHash} from 'node:crypto';
import {resolve} from 'node:path';
import assert from 'node:assert/strict';
const dir=resolve(process.argv[2]);
const bytes=readFileSync(`${dir}/histories.jsonl`);
const histories=bytes.toString().trim().split('\n').map(line=>JSON.parse(line));
const summary=JSON.parse(readFileSync(`${dir}/summary.json`));
const hash=b=>createHash('sha256').update(b).digest('hex');
assert.equal(hash(bytes),summary.histories_sha256);
assert.equal(histories.length,summary.requested_histories);
assert.equal(hash(readFileSync(summary.binary)),summary.binary_sha256);
const faultMatrix={},faultObservations={},routes={},methods={};
let totalCalls=0,workloadCalls=0,observedOverlapHistories=0;
for(let i=0;i<histories.length;i++){
 const h=histories[i];assert.equal(h.index,summary.start_index+i);assert.equal(h.seed,summary.seed+h.index);assert.equal(h.result,'Ok');
 const work=h.calls.filter(c=>c.phase==='workload');assert.equal(work.length,summary.requested_workload_calls_per_history);workloadCalls+=work.length;
 assert.equal(h.concurrency,[2,4,8][h.index%3]);
 assert.ok(work.some((a,j)=>work.some((b,k)=>k>j&&a.submitted_ns<b.return_ns&&b.submitted_ns<a.return_ns)));observedOverlapHistories++;
 for(const c of h.calls){totalCalls++;routes[c.route]=(routes[c.route]??0)+1;methods[c.request.command.method]=(methods[c.request.command.method]??0)+1;assert.ok(c.invoke_ns<c.submitted_ns&&c.submitted_ns<c.return_ns);}
 const fault=h.calls.find(c=>c.phase==='fault');const before=h.calls.find(c=>c.phase==='audit_before_replay');const recovery=h.calls.find(c=>c.phase==='recovery');assert.ok(fault&&before&&recovery);
 assert.ok(fault.return_ns<before.invoke_ns&&before.return_ns<recovery.invoke_ns);
 assert.equal(fault.fault,['before_commit','after_commit','sigkill'][Math.floor(h.index/3)%3]);
 assert.deepEqual(fault.request,recovery.request);assert.equal(recovery.observed.response.ok,true);
 const messages=before.observed.response.result.messages;assert.ok(messages.length===0||messages.length===1);
 const matrix=`${fault.fault}/clients-${h.concurrency}`;faultMatrix[matrix]=(faultMatrix[matrix]??0)+1;
 const observation=`${fault.fault}/${fault.observed.kind}/${messages.length===0?'no-effect-observed-before-replay':'effect-observed-before-replay'}`;faultObservations[observation]=(faultObservations[observation]??0)+1;
 if(fault.fault==='before_commit'){assert.equal(fault.observed.kind,'known_abort');assert.equal(messages.length,0);}
 if(fault.fault==='after_commit'){assert.equal(fault.observed.kind,'known_commit');assert.equal(messages.length,1);}
 if(messages.length===1){assert.equal(messages[0].message,recovery.observed.response.result.message);}
}
assert.equal(workloadCalls,summary.phase_calls.workload);
assert.deepEqual(faultMatrix,summary.fault_by_concurrency);
assert.equal(Object.keys(faultMatrix).length,9);
assert.deepEqual(routes,summary.route_calls);
const audit={checked_at_utc:new Date().toISOString(),auditor_sha256:hash(readFileSync(new URL(import.meta.url))),history_sha256:hash(bytes),binary_sha256:summary.binary_sha256,histories:histories.length,workload_calls:workloadCalls,total_calls:totalCalls,independently_recomputed_routes:routes,independently_recomputed_methods:methods,histories_with_submitted_request_overlap:observedOverlapHistories,fault_by_concurrency:faultMatrix,fault_public_read_observations:faultObservations,checks:['all generated histories accepted','exact workload quota per history','ordered contiguous seeds and indices','all nine fault-by-concurrency combinations','invocation < submission < response/death','each workload contains actual overlapping submitted requests','public inbox read strictly after original death and before identical replay','deterministic abort shows zero messages before replay','deterministic commit shows its stable message ID before replay','raw history and binary hashes match summary'],limitations:['No-effect means not observed by the pre-replay public read; it is not a power-loss or storage-internals audit.','Unknown SIGKILL histories can be linearized as aborts when no prior effect is observed; deterministic postcommit failures separately test committed effects without replies.']};
writeFileSync(`${dir}/independent-audit.json`,JSON.stringify(audit,null,2)+'\n');
console.log(JSON.stringify(audit,null,2));
