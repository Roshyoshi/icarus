# Subprocess protocol v1

Go never reads the coordination database. It invokes `icarus-sync version`
to check protocol compatibility, then `icarus-sync api` to allocate a launch.
The MCP and CLI adapters use the same Rust operations.
`version` also reports the linked `sqlite_version` and `sqlite_source_id`.

```sh
printf '%s\n' '{"version":1,"operation_id":"launch-1","command":{"method":"launch_start","provider":"codex","instance":"instance-uuid"}}' |
  icarus-sync --home "$ICARUS_HOME" --group parser api
```

A successful response is `{"version":1,"ok":true,"result":...}`. Request errors
set `ok:false`, include `error`, and exit nonzero. Startup failures may exit with
stderr before emitting a protocol response. Input is bounded to 256 KiB;
mutation requests to 64 KiB. MCP uses newline-delimited JSON-RPC over stdio,
supports protocol version `2024-11-05`, and never writes logs to stdout.

## Every caller supplies its identity

```json
{
  "version": 1,
  "operation_id": "one-new-id-for-this-send",
  "member": "member-id-from-your-startup-hook",
  "launch": "launch-id-from-your-startup-hook",
  "command": {"method": "send", "to": "recipient-member-id", "body": "ready"}
}
```

`launch_start` and `bind` are lifecycle operations available only through the
CLI, not MCP. `bind` takes a native `session`, optional `agent`, and optional
`parent_agent`. Member identity is unique within the group by provider,
instance UUID, native session, and native agent ID. An empty agent identifies
the main conversation. Parent relationships are immutable.

Reopening the same native conversation keeps its member and mailbox. A newer
launch ordinal supersedes the old launch, including all descendants until
they rebind. Older startup hooks cannot roll it back. Tools require the member
and launch explicitly; MCP also checks that the launch belongs to its helper.
These identifiers prevent accidental routing mistakes; they are not security
tokens against an uncooperative process with the same OS authority.

`members` lists registered identities and parents. Membership does not claim
that a process is alive or that a task succeeded. Retention is indefinite in
this MVP; no automatic deletion, leases, or garbage collection are inferred.

## Operations

| Method | Command fields | Result / contract |
|---|---|---|
| `members` | none | `members` with provider, instance, session, agent, parent, launch |
| `send` | `to`, `body` | Stable `message` ID and per-recipient `sequence` |
| `inbox` | optional `limit` (1–4, default 4) | Ordered `messages`, `acked`, `batch_end` |
| `ack` | `through` message ID | Highest `acked` sequence |
| `task_create` | `owner`, `description` | Unique `task` handle |
| `task_read` | `task` | Owner, description, result (`null` while pending) |
| `task_finish` | `task`, `result` | Immutable terminal task record; only owner may finish |
| `cell_read` | `key` | `key`, `revision`, `value`; absent means revision 0 / null |
| `cell_cas` | `key`, `expected`, `value` | `swapped` and current `cell`; success increments once |
| `wait_any` | `messages`, `tasks`, `cells`, `timeout_ms` | `ready`, `timeout`, or `cancelled` |

Task results have `status` (`success`, `failure`, `cancelled`) and a JSON
`value`. Cell watches are `{key,revision}`. Wait sets contain at most 64 task
and cell entries; the deadline is a relative duration, at most 24 hours.
Messages are at most 2048 JSON-encoded bytes, task/cell values 16 KiB, and hook
context 12 KiB. JSON escaping counts toward these limits.

Mutations, including `inbox`'s durable fetch boundary, require `operation_id`.
Reuse it only for retries with the same parsed arguments. Its effect and replay
result commit in one `BEGIN IMMEDIATE` transaction. A replay returns the
original result; different arguments fail. Validation and immutable-result
conflicts roll back without reserving the operation ID. Stale callers are
rejected even when they replay an earlier operation.

```text
send 1, 2, 3, 4, 5
inbox(limit=2) → [1,2]      durable batch_end = 2
inbox(limit=4) → [1,2]      cannot advance while outstanding
ack(1)
inbox()       → [2]
ack(2)
inbox()       → [3,4,5]
```

Acknowledgement is cumulative within the fetched prefix and may be repeated.
It cannot acknowledge another member's mailbox or skip unfetched messages.
It excludes acknowledged messages from new reads; it cannot retract already
fetched output or an explicit replay of a historical inbox operation. Hooks
never acknowledge messages. A consumer acknowledges after consumption and
deduplicates by message ID when needed.

CAS governs the cell transaction only. It grants no filesystem lock, lease,
automatic recovery, or exactly-once external action. A task finishes only
through `task_finish`. Repeating the same terminal result is permitted;
changing it fails. Timeouts and MCP cancellation cancel only the wait.

## Waits treat notifications as hints

```text
waiter: bind socket + publish address → fresh read → sleep ≤ 1 s → fresh read
writer: COMMIT → best-effort datagram → reply
```

One local SQLite database per group uses WAL, `synchronous=FULL`, and macOS
full-fsync settings. Writers hold short transactions. Readers release each
snapshot before sleeping. Lost wakeups, stale socket entries, helper crashes,
and denied Unix socket creation cannot hide pending state indefinitely:
polling remains available. Liveness assumes eventual scheduling, database
access, recovery, and retention. Storage must honor SQLite's sync semantics;
network filesystems are outside the contract.

MCP accepts `notifications/cancelled` for pending waits without blocking other
requests. At most 32 waits run per helper. CLI cancellation is ordinary process
termination; committed work remains. There is no durable wait object to cancel.

## Native hooks

Startup binds identities and injects concise tool instructions. Codex receives
delivery on `SessionStart`, `SubagentStart`, `UserPromptSubmit`, `Stop`, and
`SubagentStop`. Claude also receives delivery on `PostToolUse`. Generic Codex
tool hooks are deliberately omitted: the documented payload does not identify
the subagent. Native parent IDs are used where supplied; absent explicit
`parent_agent_id`, a subagent is attached to the main session named by the hook.
Nested parentage requires the native event to supply enough information; the
adapter does not parse unstable transcript files to guess it.

Stop hooks obey `stop_hook_active` and claim a durable guard per member, launch,
and native `turn_id`. When the CLI supplies no turn ID, the guard uses the
outstanding mailbox `batch_end`; assistant text cannot identify a turn.
Concurrent Stop hooks continue at most once per guard. After acknowledgement,
a new batch can trigger continuation even when the assistant repeats its text.
A crash after claiming the guard can defer delivery until a later boundary or
explicit inbox read. Messages remain unacknowledged. Hook failure never implies task
completion or grants a native tool permission.

Primary interface sources (checked 2026-10-14):
[Codex hooks](https://learn.chatgpt.com/docs/hooks),
[Codex authentication](https://learn.chatgpt.com/docs/auth),
[Codex configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference),
[Claude hooks](https://code.claude.com/docs/en/hooks),
[Claude clean configuration](https://code.claude.com/docs/en/debug-your-config#test-against-a-clean-configuration).
