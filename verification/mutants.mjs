// Compile deliberately broken production sources in disposable directories.
// A failed build or unrelated test failure is not a detected mutation.
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { cpSync, existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const out = resolve(process.env.ICARUS_MUTANT_OUT ??
  join(root, `verification/out/mutants-${new Date().toISOString().replaceAll(':', '-')}`));
if (existsSync(out) && readdirSync(out).length)
  throw new Error(`refusing to overwrite existing mutant evidence: ${out}`);
mkdirSync(out, { recursive: true });
const mutations = [
  {
    name: 'non_atomic_replay', file: 'store.rs',
    from: '        let result = mutate(&tx, request)?;',
    to: `        let result = mutate(&tx, request)?;
        tx.commit()?;
        crash_point("before_commit");
        let tx = self.conn.transaction_with_behavior(TransactionBehavior::Immediate)?;`,
    test: 'every_mutation_recovers_at_each_commit_crash_boundary',
    diagnostic: 'duplicate or missing physical effect',
  },
  {
    name: 'premature_ack', file: 'store.rs',
    from: '    let mut stmt = conn.prepare("SELECT id, sequence, sender, body FROM messages',
    to: `    if end > acked {
        conn.execute("UPDATE mailboxes SET acked=?2, batch_end=0 WHERE member=?1",
            params![member.as_str(), end])?;
    }
    let mut stmt = conn.prepare("SELECT id, sequence, sender, body FROM messages`,
    test: 'fetch_and_injection_do_not_acknowledge_mail',
    diagnostic: 'fetch or injection acknowledged mail without ack',
  },
  {
    name: 'stale_snapshot', file: 'wake.rs',
    from: '    loop {\n        if cancelled.load(Ordering::Acquire)',
    to: '    let mut retained: Option<Value> = None;\n    loop {\n        if cancelled.load(Ordering::Acquire)',
    extra: {
      from: '        let ready = store.ready(request)?;',
      to: `        let ready = if let Some(value) = &retained { value.clone() } else {
            let value = store.ready(request)?;
            retained = Some(value.clone());
            value
        };`,
    },
    test: 'wait_schedules_observe_commits_before_and_after_each_read_boundary',
    diagnostic: 'helper exceeded 5s deadline',
  },
  {
    name: 'ignore_ancestor_generation', file: 'store.rs',
    from: 'AND NOT EXISTS(SELECT 1 FROM lineage WHERE launch != ?2)',
    to: 'AND NOT EXISTS(SELECT 1 FROM lineage WHERE id = ?1 AND launch != ?2)',
    test: 'ancestor_resume_rejects_descendant_calls_and_replays_without_rebinding',
    diagnostic: 'assertion failed: !stale_output.status.success()',
  },
  {
    name: 'assistant_text_boundary', file: 'hooks.rs',
    from: '            format!("batch:{}", batch["batch_end"])',
    to: '            format!("text:{}", input.last_assistant_message.unwrap_or_default())',
    extra: {
      from: '    pub turn_id: Option<String>,',
      to: '    pub turn_id: Option<String>,\n    pub last_assistant_message: Option<String>,',
    },
    test: 'repeated_stop_text_can_deliver_the_next_acknowledgement_batch',
    diagnostic: 'identical assistant text must not suppress a later batch',
  },
];

function run(command, args, cwd, env, log, timeout = 300000) {
  const inherited = Object.fromEntries(Object.entries(process.env)
    .filter(([key]) => !key.startsWith('ICARUS_')));
  const result = spawnSync(command, args, { cwd, env: { ...inherited, ...env },
    encoding: 'utf8', timeout, maxBuffer: 16 * 1024 * 1024 });
  const output = (result.stdout ?? '') + (result.stderr ?? '');
  writeFileSync(join(out, log), output);
  if (result.error || result.signal) throw new Error(`${log}: ${result.error ?? result.signal}`);
  return { status: result.status, output };
}
function replaceOnce(source, from, to) {
  if (source.split(from).length !== 2) throw new Error(`mutation no longer matches uniquely: ${from}`);
  return source.replace(from, to);
}

const started = Date.now();
// Run the same tests on the unmodified helper before attributing failures.
const baseline = run('cargo', ['test', '--locked', '--test', 'verification', '--', '--test-threads=1'],
  root, { ICARUS_STARTUP_ROUNDS: '1', ICARUS_SYNC_BIN: join(root, 'target/debug/icarus-sync'),
    ICARUS_VERIFY_REPORT_DIR: join(out, 'baseline-reports') }, 'baseline.log');
if (baseline.status !== 0) throw new Error('unmodified baseline failed; see baseline.log');
const results = [];
for (const mutation of mutations) {
  const directory = join(out, mutation.name);
  mkdirSync(join(directory, 'sync'), { recursive: true });
  for (const file of ['Cargo.toml', 'Cargo.lock', 'sync/Cargo.toml'])
    cpSync(join(root, file), join(directory, file));
  cpSync(join(root, 'sync/src'), join(directory, 'sync/src'), { recursive: true });
  const sourcePath = join(directory, 'sync/src', mutation.file);
  let source = replaceOnce(readFileSync(sourcePath, 'utf8'), mutation.from, mutation.to);
  if (mutation.extra) source = replaceOnce(source, mutation.extra.from, mutation.extra.to);
  writeFileSync(sourcePath, source);
  const target = join(out, 'target');
  const build = run('cargo', ['build', '--locked', '--offline', '--manifest-path', join(directory, 'Cargo.toml')],
    root, { CARGO_TARGET_DIR: target }, `${mutation.name}.build.log`);
  if (build.status !== 0) throw new Error(`${mutation.name}: build failed, not a detected fault`);
  const binary = join(directory, 'icarus-sync');
  cpSync(join(target, 'debug/icarus-sync'), binary);
  const test = run('cargo', ['test', '--locked', '--test', 'verification', mutation.test,
    '--', '--exact', '--nocapture'], root,
    { ICARUS_SYNC_BIN: binary }, `${mutation.name}.test.log`);
  const detected = test.status !== 0 && test.output.includes(mutation.diagnostic);
  results.push({ mutation: mutation.name, test: mutation.test, detected,
    exit_code: test.status, required_diagnostic: mutation.diagnostic,
    binary_sha256: createHash('sha256').update(readFileSync(binary)).digest('hex') });
  writeFileSync(join(out, 'summary.json'), JSON.stringify({ baseline: 'passed',
    elapsed_ms: Date.now() - started, results }, null, 2));
  console.log(`${mutation.name}: ${detected ? 'detected' : 'UNEXPECTED RESULT'}`);
  if (!detected) throw new Error(`inspect ${mutation.name}.test.log; mutation did not fail as required`);
}
