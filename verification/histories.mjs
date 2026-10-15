import {spawn} from 'node:child_process';
import {mkdtemp, mkdir, readFile, writeFile, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {resolve, dirname} from 'node:path';
import {fileURLToPath} from 'node:url';
import {randomUUID,createHash} from 'node:crypto';
import {isDeepStrictEqual} from 'node:util';
import assert from 'node:assert/strict';

const root=resolve(dirname(fileURLToPath(import.meta.url)),'..');
const binary=resolve(process.env.ICARUS_SYNC_BIN || `${root}/target/debug/icarus-sync`);
const output=resolve(process.env.ICARUS_VERIFY_OUT || `${root}/verification/out`);
const home=await mkdtemp(`${tmpdir()}/icarus-history-`);
const events=[];
let clock=0;
async function invoke(request,crash) {
  const call=randomUUID(),start=++clock;
  events.push({event:'invoke',clock:start,call,request,crash:crash??null});
  const p=spawn(binary,['--home',home,'--group','history','api'],{env:{...process.env,ICARUS_TEST_CRASH:crash??''},stdio:['pipe','pipe','pipe']});
  let out='',err='';p.stdout.on('data',b=>out+=b);p.stderr.on('data',b=>err+=b);
  p.stdin.end(JSON.stringify(request));
  const code=await new Promise((done,reject)=>{p.once('error',reject);p.once('close',done);});
  const response=out?JSON.parse(out):null;
  events.push({event:response?'response':'crash',clock:++clock,call,response,code,stderr:err});
  if(crash)assert.equal(code,86,err);
  else assert.ok(response,err);
  return response;
}
const setup=command=>({version:1,operation_id:randomUUID(),command});
const launch=(await invoke(setup({method:'launch_start',provider:'codex',instance:'fixture'}))).result.launch;
const member=(await invoke({...setup({method:'bind',session:'native-session',agent:null,parent_agent:null}),launch})).result.member;
const req=command=>({...setup(command),launch,member});
const first=events.length;

const replayedSend=req({method:'send',to:member,body:'one'});
await Promise.all([
  invoke(replayedSend),
  invoke(req({method:'send',to:member,body:'two'})),
  invoke(req({method:'cell_cas',key:'cell',expected:0,value:'a'})),
  invoke(req({method:'cell_cas',key:'cell',expected:0,value:'b'})),
]);
await invoke(replayedSend);
await invoke({...replayedSend,command:{...replayedSend.command,body:'changed'}});
const ambiguous=req({method:'send',to:member,body:'committed without reply'});
await invoke(ambiguous,'after_commit');
// Keep the ambiguous operation pending: the durable journal, not a reply,
// must account for it. The following read exposes its effect.
const inboxRequest=req({method:'inbox',limit:2});
const inbox=await invoke(inboxRequest);
await invoke(req({method:'ack',through:inbox.result.messages[1].message}));
await invoke(req({method:'inbox',limit:4}));
await invoke(inboxRequest);
// A new operation ID may acknowledge an already-acknowledged message.
await invoke(req({method:'ack',through:inbox.result.messages[0].message}));
const task=(await invoke(req({method:'task_create',owner:member,description:'completion race'}))).result.task;
const finishes=[
  req({method:'task_finish',task,result:{status:'success',value:'winner-a'}}),
  req({method:'task_finish',task,result:{status:'failure',value:'winner-b'}}),
];
const finished=await Promise.all(finishes.map(r=>invoke(r)));
const winner=finished.findIndex(r=>r.ok),loser=1-winner;
assert.equal(finished.filter(r=>r.ok).length,1);
await invoke(finishes[winner]);
// The rejected finish did not commit a replay row. Its ID remains reusable.
await invoke({...finishes[loser],command:finishes[winner].command});

const p=spawn(binary,['--home',home,'--group','history','history']);
let journalText='';p.stdout.on('data',b=>journalText+=b);
assert.equal(await new Promise(r=>p.once('close',r)),0);
const journal=JSON.parse(journalText).filter(e=>!['launch_start','bind'].includes(e.request.command.method));
const workload=events.slice(first);
const attempts=workload.filter(e=>e.event==='invoke').map(e=>{
  const end=workload.find(r=>r.call===e.call&&r.event!=='invoke');
  const durable=journal.find(j=>j.operation_id===e.request.operation_id);
  if(end.response?.ok)assert.deepEqual(end.response.result,durable?.result);
  // This fixture only kills at a known post-commit point. The effect must
  // precede process death; a general unknown outcome needs a separate model.
  if(!end.response)assert.ok(durable,'known post-commit call missing');
  return {...e,end:end.clock,ok:end.response?.ok??!!durable,result:end.response?.result??durable?.result,error:end.response?.error};
});

// Independent bounded linearizability search. It uses invocation/response
// intervals and results, not the journal's transaction order. Unanswered
// committed calls stay in the search. The model is deliberately small.
function step(state,op) {
  const s=structuredClone(state),c=op.request.command,r=op.result;
  const same=isDeepStrictEqual;
  const previous=s.ledger[op.request.operation_id];
  if(previous) {
    if(same(previous.request,op.request))return op.ok&&same(previous.result,r)?s:null;
    return !op.ok&&op.error.includes('different arguments')?s:null;
  }
  if(!op.ok) {
    return c.method==='task_finish'&&s.terminal!==null&&!same(s.terminal,c.result)?s:null;
  }
  if(c.method==='send') {
    if(r.sequence!==s.mail.length+1||s.mail.some(m=>m.message===r.message))return null;
    s.mail.push({message:r.message,sequence:r.sequence,from:member,body:c.body});
  } else if(c.method==='cell_cas') {
    const swapped=c.expected===s.revision;
    if(swapped){s.revision++;s.value=c.value;}
    if(r.swapped!==swapped||r.cell.revision!==s.revision||!same(r.cell.value,s.value))return null;
  } else if(c.method==='inbox') {
    const limit=c.limit??4;
    if(s.batch===0&&s.mail.length>s.acked)s.batch=Math.min(s.mail.length,s.acked+limit);
    const batch=s.mail.slice(s.acked,Math.min(s.batch,s.acked+limit));
    if(r.acked!==s.acked||r.batch_end!==s.batch||!same(r.messages,batch))return null;
  } else if(c.method==='ack') {
    const sequence=s.mail.findIndex(m=>m.message===c.through)+1;
    if(!sequence||(sequence>s.acked&&sequence>s.batch))return null;
    s.acked=Math.max(s.acked,sequence);if(s.acked===s.batch)s.batch=0;
    if(r.acked!==s.acked)return null;
  } else if(c.method==='task_create') {
    if(s.task)return null;s.task=r.task;
  } else if(c.method==='task_finish') {
    if(s.task!==c.task||(s.terminal!==null&&!same(s.terminal,c.result)))return null;
    s.terminal=c.result;if(!same(r.result,s.terminal))return null;
  } else throw Error(`unmodeled operation ${c.method}`);
  s.ledger[op.request.operation_id]={request:op.request,result:r};
  return s;
}
const initial={mail:[],acked:0,batch:0,revision:0,value:null,task:null,terminal:null,ledger:{}};
function linearize(ops,state=initial,order=[]) {
  if(!ops.length)return order;
  for(const candidate of ops) {
    if(ops.some(other=>other!==candidate&&other.end<candidate.clock))continue;
    const next=step(state,candidate);if(!next)continue;
    const result=linearize(ops.filter(o=>o!==candidate),next,[...order,candidate.call]);
    if(result)return result;
  }
  return null;
}
const order=linearize(attempts);assert.ok(order,'no legal transaction order explains the history');
assert.ok(journal.some(e=>e.operation_id===ambiguous.operation_id),'ambiguous commit disappeared');
assert.equal(linearize(attempts.filter(a=>a.request.operation_id!==ambiguous.operation_id)),null,'checker must reject omission of an unanswered committed call');
const broken=structuredClone(attempts);broken.find(o=>o.request.command.method==='cell_cas'&&!o.result.swapped).result.swapped=true;
assert.equal(linearize(broken),null,'checker must reject double CAS success');

function tla(value) {
  if(Array.isArray(value))return `<<${value.map(tla).join(', ')}>>`;
  if(typeof value==='object'&&value!==null)return `[${Object.entries(value).map(([k,v])=>`${k} |-> ${tla(v)}`).join(', ')}]`;
  if(typeof value==='boolean')return value?'TRUE':'FALSE';
  return JSON.stringify(value);
}
const sequence=new Map(journal.filter(j=>j.request.command.method==='send')
  .map(j=>[j.result.message,j.result.sequence]));
const projected=order.map(call=>{
  const op=attempts.find(a=>a.call===call),c=op.request.command,r=op.result;
  const command={kind:c.method,input:'',expected:0,limit:0,sequence:0};
  let result='precondition',fresh='';
  if(c.method==='send'){
    command.input=JSON.stringify(c.body);
    if(op.ok){fresh=r.message;result={message:r.message,sequence:r.sequence};}
  }
  if(c.method==='cell_cas'){
    command.input=JSON.stringify(c.value);command.expected=c.expected;
    if(op.ok)result={swapped:r.swapped,revision:r.cell.revision,output:JSON.stringify(r.cell.value)};
  }
  if(c.method==='inbox'){
    command.limit=c.limit??4;
    if(op.ok)result={messages:r.messages.map(m=>m.message),acked:r.acked,batch:r.batch_end};
  }
  if(c.method==='ack'){
    command.sequence=sequence.get(c.through);
    if(op.ok)result={acked:r.acked};
  }
  if(c.method==='task_create'){
    command.input=c.description;
    if(op.ok)result={created:true};
  }
  if(c.method==='task_finish'){
    command.input=JSON.stringify(c.result);
    if(op.ok)result={output:JSON.stringify(r.result)};
  }
  if(!op.ok&&op.error.includes('different arguments'))result='arguments';
  return {op:op.request.operation_id,command,fresh,reply:{ok:op.ok,result}};
});
await mkdir(output,{recursive:true});
for(const name of ['Trace','Protocol'])
  await writeFile(`${output}/${name}.tla`,await readFile(`${root}/verification/${name}.tla`));
await writeFile(`${output}/history.json`,JSON.stringify({events:workload,journal,linearization:order},null,2)+'\n');
async function emitTrace(name,items){
  await writeFile(`${output}/${name}.tla`,`---- MODULE ${name} ----\nEXTENDS Trace\nRecordedEvents == ${tla(items)}\n====\n`);
  await writeFile(`${output}/${name}.cfg`,'CONSTANT Events <- RecordedEvents\nSPECIFICATION Spec\nINVARIANT TraceAccepted\nPROPERTY TraceConsumed\nCHECK_DEADLOCK FALSE\n');
}
await emitTrace('RecordedTrace',projected);
const corrupted=structuredClone(projected);
corrupted.find(e=>e.command.kind==='cell_cas').reply.result.revision=99;
await emitTrace('BrokenTrace',corrupted);
const replayCorrupt=structuredClone(projected);
replayCorrupt.filter(e=>e.op===replayedSend.operation_id&&e.reply.ok)[1].reply.result.message='invented-message';
await emitTrace('BrokenReplay',replayCorrupt);
const rejectedCommit=structuredClone(projected);
const conflict=rejectedCommit.find(e=>e.command.kind==='task_finish'&&!e.reply.ok);
conflict.reply={ok:true,result:{output:conflict.command.input}};
await emitTrace('BrokenConflict',rejectedCommit);
const summary={
  binary, binary_sha256:createHash('sha256').update(await readFile(binary)).digest('hex'),
  attempts:attempts.length,durable_operations:journal.length,ambiguous_commits:1,
  errors:attempts.filter(a=>!a.ok).length,
  successful_replays:attempts.filter(a=>a.ok).length-journal.length,
  linearizable:true,trace_projection:'selected legal call order, including failed calls and replay attempts',
  negative_controls:['lost ambiguous commit','double CAS success'],
  trace_negative_controls:['wrong revision','changed replay result','committed conflicting finish'],
  omissions:['one mailbox/cell/task','no read-only calls','no unknown pre-commit outcomes',
    'no ownership/identity/wait/hook events','no storage page or power-loss model']
};
await writeFile(`${output}/history-summary.json`,JSON.stringify(summary,null,2)+'\n');
await rm(home,{recursive:true,force:true});
console.log(`History accepted: ${attempts.length} calls, ${journal.length} durable effects, 1 unanswered commit, ${summary.errors} errors, ${summary.successful_replays} successful replays.`);
