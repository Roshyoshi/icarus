import {spawn} from 'node:child_process';
import {mkdir,writeFile} from 'node:fs/promises';
import {resolve,dirname} from 'node:path';
import {fileURLToPath} from 'node:url';
import {randomUUID} from 'node:crypto';
import assert from 'node:assert/strict';

const root=resolve(dirname(fileURLToPath(import.meta.url)),'..');
const args=process.argv.slice(2), options={};
for(let i=0;i<args.length;i+=2){assert.ok(args[i].startsWith('--')&&args[i+1],'expected --key value');options[args[i].slice(2)]=args[i+1];}
assert.ok(options.codex&&options.claude,'Usage: node verification/live.mjs --codex INSTANCE --claude INSTANCE [--icarus PATH]');
const launcher=resolve(options.icarus||`${root}/bin/icarus`);
const helper=resolve(process.env.ICARUS_SYNC_BIN||`${dirname(launcher)}/icarus-sync`);
const group=`live-${randomUUID().slice(0,8)}`, token=`icarus-live-${randomUUID()}`;
const home=process.env.ICARUS_HOME||`${process.env.HOME}/.local/share/icarus`;
const out=`${root}/verification/out/${group}`;await mkdir(out,{recursive:true});
const base=`This is an opt-in Icarus integration test. Use your native subagent tool to start exactly one subagent. Give it the task below. Keep native permissions and hook trust checks. Do not edit files. Wait for the subagent's explicit result, then report it. Subagent task: use only your own member and launch IDs injected by Icarus. Create a task assigned to yourself with description ${token}. Use a unique operation_id per mutation and reuse it on retries. Poll members until there is a member of the other provider with a non-null parent. Do not target its main agent. Deadline: five minutes. `;
const codex=base+`Send the exact mailbox body ${token} to the Claude subagent. Use wait_any and inbox to receive reply:${token} from that subagent, acknowledge its message ID, and finish your task with status success and value ${token}. If coordination cannot run, explicitly fail the task and report why.`;
const claude=base+`Use wait_any and inbox to receive ${token} from the Codex subagent. Acknowledge its message ID and send the exact body reply:${token} to its sender. Finish your task with status success and value ${token}. If coordination cannot run, explicitly fail the task and report why.`;

async function run(provider,prompt) {
  const native=provider==='codex'?['exec','--skip-git-repo-check','--json',prompt]:['--print','--output-format','json',prompt];
  const p=spawn(launcher,['open',provider,options[provider],'--group',group,'--',...native],{env:{...process.env,ICARUS_SYNC_BIN:helper},stdio:['ignore','pipe','pipe']});
  let stdout='',stderr='';p.stdout.on('data',b=>stdout+=b);p.stderr.on('data',b=>stderr+=b);
  let killTimer;
  const timer=setTimeout(()=>{p.kill('SIGTERM');killTimer=setTimeout(()=>p.kill('SIGKILL'),5000);},360000);
  let code;
  try {code=await new Promise((done,reject)=>{p.on('error',reject);p.on('close',done);});}
  finally {clearTimeout(timer);clearTimeout(killTimer);await writeFile(`${out}/${provider}.stdout`,stdout);await writeFile(`${out}/${provider}.stderr`,stderr);}
  assert.equal(code,0,`${provider} failed; inspect ${out}`);
}
console.log(`Running real native subagents in group ${group}. Native login and trusted hooks are prerequisites. Logs: ${out}`);
const results=await Promise.allSettled([run('codex',codex),run('claude',claude)]);
const p=spawn(helper,['--home',home,'--group',group,'history']);let text='';p.stdout.on('data',b=>text+=b);
assert.equal(await new Promise(r=>p.once('close',r)),0);
const journal=JSON.parse(text);await writeFile(`${out}/history.json`,JSON.stringify(journal,null,2)+'\n');
for(const result of results)if(result.status==='rejected')throw result.reason;
const providers=new Map(journal.filter(e=>e.request.command.method==='launch_start').map(e=>[e.result.launch,e.request.command.provider]));
const children=new Map(journal.filter(e=>e.request.command.method==='bind'&&e.request.command.agent&&e.result.parent).map(e=>[e.result.member,providers.get(e.request.launch)]));
for(const [from,to,body] of [['codex','claude',token],['claude','codex',`reply:${token}`]]) {
  const message=journal.find(e=>e.request.command.method==='send'&&e.request.command.body===body&&children.get(e.request.member)===from&&children.get(e.request.command.to)===to);
  assert.ok(message,`missing durable ${from} subagent -> ${to} subagent send`);
  assert.ok(journal.some(e=>e.request.command.method==='ack'&&e.request.member===message.request.command.to&&e.request.command.through===message.result.message),`missing recipient acknowledgement for ${from} send`);
  assert.ok(journal.some(e=>e.request.command.method==='task_finish'&&children.get(e.request.member)===from&&e.result.result.status==='success'&&e.result.result.value===token),`missing explicit ${from} task completion`);
}
console.log(`PASS: durable messages, recipient acknowledgements, and explicit successful tasks from both native subagents. Evidence: ${out}`);
