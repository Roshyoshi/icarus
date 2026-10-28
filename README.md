# Icarus

Icarus runs separate native Codex and Claude instances and gives their agents
durable local mailboxes, task results, and shared versioned state.

**Full disclosure: this repo was slopped up by Codex.** Codex generated the code
and verification tooling and ran the automated checks. The published results
are evidence within their stated bounds. No independent human audit is claimed.

The automated campaign passed on **macOS**. **Linux is experimental**; runtime
verification remains open. Real authenticated account separation and a real
Codex-to-Claude subagent exchange were deferred. This is not complete release
acceptance. See the [verification results](verification/RESULTS.md).

## Build and install

Use Go 1.24+, a current stable Rust toolchain, and `make`. Install the native
`codex` and/or `claude` CLI separately and put it on `PATH`. Native logged-out
diagnostics passed with Codex 0.162.1 and Claude Code 2.1.300. Icarus checks their
diagnostic formats strictly and refuses the first native launch if it cannot
establish the required logged-out state.

```sh
make build                       # builds bin/icarus and bin/icarus-sync
./bin/icarus --help
make install                     # installs both into ~/.local/bin
export PATH="$HOME/.local/bin:$PATH"
```

`make install PREFIX=/your/prefix` installs into `/your/prefix/bin`.
Keep both executables together and their path stable after creating instances.
If Go is outside `PATH`, pass `GO=/absolute/path/go` to `make`.

## Open two instances

Run these in separate terminals:

```sh
# Terminal A
icarus new codex writer --group hello-icarus

# Terminal B
icarus new claude reviewer --group hello-icarus
```

`new` creates a persistent, initially logged-out native home and opens the
provider's CLI. Sign in through that CLI. The labels `writer` and `reviewer`
identify local instances; they do not select accounts. Creating an existing
label fails without replacing it.

Icarus installs its MCP server and native hooks in each instance. Review them
through the native CLI's trust and permission controls. The
[native integration guide](docs/native-integration.md) describes the installed
configuration and hook behavior.

Reopen an instance with `open`. Native arguments follow `--`:

```sh
icarus open codex writer --group hello-icarus
icarus open claude reviewer --group hello-icarus -- --model opus
icarus list
```

The native home persists across reopening. Multiple conversations can share
one instance; each native session and subagent has its own coordination
identity. Reopening an instance does not itself resume a particular native
conversation. Use the native CLI's session controls for that.

## Pass a message between the conversations

With both conversations open in `hello-icarus`, ask Codex:

> Use the Icarus `members` tool to identify the Claude main conversation. Show
> its member and session IDs, then send it "Please reply with parser-ready."
> Use your own member and launch IDs from the startup hook.

If the group contains several Claude conversations, select the intended session
explicitly. Membership records persist and do not prove that a process is live.

In Claude, ask:

> Read your Icarus inbox. For the request to reply with parser-ready, send
> "parser-ready" to the message's sender. Then acknowledge the consumed message
> using its message ID.

Back in Codex, ask:

> Read your Icarus inbox, show the reply, and acknowledge it.

The native agents use these actual operations on the `icarus` MCP server:

```text
Codex: members()                         → Claude's member ID
Codex: send(to=Claude, body=request)      → stable message ID
Claude: inbox()                          → ordered messages with sender IDs
Claude: send(to=sender, body=reply)       → a separate message ID
Claude: ack(through=request_message_id)  → request excluded from new reads
Codex: inbox(); ack(through=reply_id)     → reply consumed explicitly
```

Every call supplies the caller's own `member` and `launch`. Each new mutation
also needs a new `operation_id`; retries reuse that ID and the same arguments.
For example, the MCP `send` arguments are below. Replace placeholders with the
returned IDs and choose a fresh operation ID:

```json
{
  "member": "<Codex member ID>",
  "launch": "<Codex launch ID>",
  "operation_id": "hello-request-1",
  "to": "<Claude member ID>",
  "body": "Please reply with parser-ready."
}
```

Authenticated execution of this walkthrough remains unverified. Hooks can
deliver mail at supported prompt/tool/stop boundaries. An idle conversation
retains its mail until resumed. Delivery can repeat until acknowledgement,
and acknowledgement cannot retract output a hook already fetched. Messages
are attributed peer data, not permission grants.

Agents and tool authors can use the
[agent query specification](docs/agent-query-spec.md) for exact requests and
replies. The [protocol reference](docs/protocol.md) defines ordering, retries,
task completion, revision comparisons, and waits.

## Reuse settings without copying a login

Without an imported template, the first `new` seeds a built-in `default`.
Defaults use Astra with `max` reasoning and `xhigh` plan reasoning for Codex,
and Opus with `xhigh` effort and the `dark-ansi` theme for Claude.

Import a named template before creating its instances:

```sh
icarus template import codex review --from "$HOME/.codex"
icarus new codex review-work --template review --group reviews

# If the imported template contains reviewer.config.toml:
icarus open codex review-work --profile reviewer --group reviews
```

The same import command supports `claude` with `--from` set to the chosen
Claude configuration directory. The importer reports included and excluded
field names. It copies supported settings and Codex profiles. Instructions and
skills require explicit `--instruction PATH` and `--skill PATH` selection.
Credentials, histories, trust records, environment blocks, authentication
helpers, and existing executable integrations are excluded.

Templates are editable snapshots. Changes affect future instances; existing
instances keep their copies. The launcher removes ambient provider credentials
and authentication selectors and checks the effective native configuration
before the first launch. This isolates account selection within one OS user;
it does not protect data from another process with that user's authority.

## Two executables, one database per group

```text
icarus (Go)
  template → unique persistent native home → exec native CLI

native CLI ── MCP over stdio ── icarus-sync (Rust) ── group SQLite database
native hooks ── subprocess ────────────────────────┘
                            peer wakeups via Unix datagrams
```

There is no central daemon. Each native CLI owns its MCP helper. Go owns
templates, instance creation, and launch; Rust owns every coordination-state
transition. SQLite commits the mutation and replay result together. Datagram
wakeups are hints; pending waits also read a fresh database snapshot each second.

Native CLIs own login, refresh, permissions, and subagents. The launcher uses
`exec`, preserving terminal behavior, signals, and exit status. Subagents need
native permission to use the MCP tools and must use their own identities.

Runtime data defaults to `~/.local/share/icarus`; set `ICARUS_HOME` to relocate it.

```text
$ICARUS_HOME/
  templates/{codex,claude}/NAME/   settings snapshots
  instances/PROVIDER/LABEL/        label and internal UUID
  homes/UUID/                      stable native home and eventual login
  groups/GROUP/coordination.db     durable coordination state
```

## What the verification establishes

The macOS campaign on 2026-10-14 checked the production helper, forced selected
process schedules, and tested whether concurrent public results fit an
independent sequential model. It also checked finite TLA+ models and verified
that deliberately broken implementations fail.

| Question | Observed result |
|---|---|
| Can concurrent CLI/MCP calls fit a legal operation order? | 1,000/1,000 histories accepted, containing 50,000 workload calls |
| Can helpers initialize the same fresh database together? | 3,200/3,200 starts succeeded across 100 groups with 32 helpers each |
| Do effects and replay results survive process crashes together? | 24/24 mutation/crash cases passed |
| Can waits observe work across notification races and dropped hints? | 18/18 forced schedules passed, including six confirmed datagram sends |
| Do identity, acknowledgement, and hook boundaries hold in fixtures? | All nine new boundary tests passed, including four descendant levels and hook/ack/resume races |
| Do the checks detect known broken behavior? | 5/5 compiled production mutants detected; seven broken models and three corrupted traces rejected |
| Do the bounded protocol models satisfy their stated properties? | All four finite models passed; recorded API trace accepted |

The campaign found and fixed cold-start database contention and repeated Stop
text suppressing later mail. It also upgraded an affected SQLite dependency.
The [results and raw evidence](verification/RESULTS.md) explain each count,
the observed failures, model bounds, benchmarks, and remaining gaps.

These checks do not establish arbitrary execution correctness, power-loss
recovery, or exactly-once model consumption and external actions. Linux runtime
tests and authenticated native account/subagent acceptance remain open.

For the ordinary test suite, then the full automated concurrency campaign:

```sh
make test check
sh verification/download-tlc.sh
make verify-sync
```

The full campaign also needs Node, Java, and permission to create local Unix
datagram sockets. It makes no model calls. See the
[verification guide](verification/README.md) for dependencies, separate checks,
native diagnostics, and the opt-in authenticated test procedure.
