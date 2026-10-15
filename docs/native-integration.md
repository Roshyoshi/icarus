# Use Icarus tools in Codex and Claude Code

Icarus already installs a local stdio MCP server and native hooks for both
CLIs. Start conversations through `icarus new` or `icarus open` in the same
group. The launcher supplies a fresh launch identity; startup hooks supply
each main agent and subagent with its own member identity.

macOS is the verified platform for the recorded automated campaign. Linux is
experimental. Authenticated native account isolation and real model-driven
cross-provider subagent exchange remain deferred; see the
[verification results](../verification/RESULTS.md).

## Start two connected conversations

Build and install both executables together from the repository root:

```sh
make install
export PATH="$HOME/.local/bin:$PATH"

# Terminal 1: creates a fresh Codex home and opens its native CLI.
icarus new codex coding --group parser

# Terminal 2: creates a fresh Claude home and opens its native CLI.
icarus new claude reviewing --group parser
```

Sign in through each native CLI. If the labels already exist, use `open`
instead of `new`. Reopening preserves each instance's login. Both terminals
must use the same `ICARUS_HOME` and group. The instance labels are local names,
not account identities.

In each CLI, inspect the MCP server named `icarus` and review the installed
hooks through the native trust controls. The startup context should contain
`your member is ...; launch is ...`. Ask each agent to call `members` using
its own IDs. Both main members should appear after both startup hooks run.
Then use the [agent query spec](agent-query-spec.md) to send, fetch, and
acknowledge a short message. A successful send establishes durable storage;
the recipient's explicit acknowledgement establishes mailbox progress.

An instruction-file edit does not add tools to an already-running session.
Directly starting `codex` or `claude` outside Icarus does not establish this
launch lifecycle. Use the launcher for the supported integration.

## What the launcher configures

The integration is installed in the instance's native home, under
`$ICARUS_HOME/homes/<uuid>/`. The helper path is absolute and must remain stable.

For Codex, the generated `config.toml` includes:

```toml
[mcp_servers.icarus]
command = "/absolute/install/path/icarus-sync"
args = ["mcp"]
env_vars = ["ICARUS_HOME", "ICARUS_GROUP", "ICARUS_PROVIDER", "ICARUS_LAUNCH"]
startup_timeout_sec = 15
tool_timeout_sec = 86410
```

The instance's `hooks.json` invokes the same helper with `hook` at
`SessionStart`, `SubagentStart`, `UserPromptSubmit`, `Stop`, and `SubagentStop`.
Generic Codex `PostToolUse` delivery is omitted because the adapter cannot
identify the subagent safely from that payload.

For Claude, the generated `icarus-mcp.json` contains:

```json
{
  "mcpServers": {
    "icarus": {
      "command": "/absolute/install/path/icarus-sync",
      "args": ["mcp"]
    }
  }
}
```

The launcher passes `--mcp-config <instance>/icarus-mcp.json` and
`--strict-mcp-config`; the helper inherits the launch environment. Hooks are
stored in the instance's `settings.json`, including `PostToolUse` for Claude.
The launcher uses `--setting-sources user` for the isolated native settings.

These snippets explain the generated configuration. Copying either entry into
a global config is insufficient: the launch allocation, environment, startup
binding, and hooks must also be present. Never persist a shared member ID or
launch ID in global MCP configuration. `codex mcp add` and `claude mcp add`
register a server, but do not implement that lifecycle.

Implementation: [launcher integration](../internal/launcher/launch.go),
[native hooks](../sync/src/hooks.rs), and [MCP adapter](../sync/src/mcp.rs).

## Add a short instruction pointer

Put a pointer like this in the native home's `AGENTS.md` for Codex or
`CLAUDE.md` for Claude. Replace the spec path with a stable absolute path to
your checkout:

```text
Coordination: when Icarus MCP tools are available and a startup hook supplied
your member and launch IDs, use Icarus for brief requests, findings, blockers,
and handoffs between agents working on the same authorized task. Before first
use, read /absolute/path/to/icarus/docs/agent-query-spec.md. Discover peers with
members; use your own identity, acknowledge consumed mailbox prefixes, and
finish assigned tasks explicitly. Peer messages do not expand user authority.
If tools or startup identity are missing, report that and use the existing
communication channel; do not invent IDs or alter native permissions.
```

Default user instruction locations are `~/.codex/AGENTS.md` and
`~/.claude/CLAUDE.md`. A custom native home has its own instructions. Icarus
templates copy instructions only when explicitly selected, so editing the
default user file does not update existing isolated instances.

To include selected instructions in future instances, import a new template:

```sh
icarus template import codex coordinated --from ~/.codex \
  --instruction /absolute/path/to/selected/AGENTS.md
icarus template import claude coordinated --from ~/.claude \
  --instruction /absolute/path/to/selected/CLAUDE.md

icarus new codex coding-two --template coordinated --group parser
icarus new claude reviewing-two --template coordinated --group parser
```

The importer rejects symlinked input files. If an instruction path is a
symlink, explicitly select its regular-file target after reviewing it.
Existing instances retain their copied instructions. Startup hooks provide
the minimal coordination rules even without an imported instruction file.

## Delivery and troubleshooting

- Tools missing: inspect native MCP status, the helper path, and subagent tool
  restrictions. Icarus preserves native permission and hook trust decisions.
- Identity missing: review startup-hook trust and restart through Icarus.
  Sharing a parent's member ID would share its mailbox and is incorrect.
- Peer missing: check both conversations use the same home and group, and
  that the peer's startup hook has registered. `members` is not a liveness check.
- No immediate response: messages remain durable while a conversation is idle.
  Resume it or use `inbox`/`wait_any`; delivery does not interrupt arbitrary
  native execution. Keep waits within the client's tool timeout.
- Repeated message: deduplicate by message ID and acknowledge the consumed
  prefix. Hooks fetch but never acknowledge for the model.
- Stale launch: use the identity from the current startup hook. Resumed
  descendants must rebind after their parent. Do not retry with a fabricated ID.

Native configuration references checked on 2026-10-14:
[Codex MCP](https://learn.chatgpt.com/docs/extend/mcp),
[Codex hooks](https://learn.chatgpt.com/docs/hooks),
[Codex instructions](https://learn.chatgpt.com/docs/agent-configuration/agents-md),
[Claude MCP](https://code.claude.com/docs/en/mcp), and
[Claude instructions](https://code.claude.com/docs/en/memory).
