# fleet — handoff notes for Claude Code

fleet is a single Go binary that coordinates multiple coding agents on one machine:
presence, kill policy, run history, and a shared knowledge base of commands that work.
**Read DESIGN.md first** — it is the source of truth for behavior. This file covers the
state of the code and what to do next.

## State of the scaffold

Phase 1 plus signals/waits (DESIGN.md §11, §16) are implemented, not stubbed. What was verified before handoff:

- `go vet` and `go build` clean; cross-compiles for windows/amd64, darwin/arm64, linux/arm64.
- Unit tests pass (`policy`, `cmdtext`, `runner`, `install`).
- `fleet test selfcheck` passes 29/29 on Linux (real subprocesses, real process kills, real
  MCP stdio handshake).

What was **not** verified (do these first):

1. **The default SQLite driver.** The build sandbox could not reach `modernc.org`, so
   everything was verified with the alternative driver via `-tags ncruces`. The default build
   (`internal/store/driver_modernc.go`) imports `modernc.org/sqlite`, which is not yet in
   go.mod. `go mod tidy` adds it. Keep modernc as the default: hooks open the DB on every tool
   call and modernc starts much faster than the WASM-based ncruces driver.
2. **Claude Code specifics** — confirm each against current Claude Code docs (the
   `claude-code-guide` agent can help) and adjust:
   - hook payload fields used in `internal/hooks/hooks.go` (`session_id`, `cwd`, `tool_name`,
     `tool_input.command`, `tool_input.file_path`, `tool_response`, `source`);
   - the Bash `tool_response` shape — per the hooks reference (Oct 2026) it is
     `{stdout, stderr, interrupted, isImage}` with **no exit code**, so hook-recorded runs have
     NULL exit codes; `parseToolResponse`'s exit-code guesses are harmless but will not match.
     Re-check on the installed version by logging a real payload with `FLEET_DEBUG=1`;
   - output format: `hookSpecificOutput.additionalContext` for SessionStart /
     UserPromptSubmit / PostToolUse, and `permissionDecision: "deny"` +
     `permissionDecisionReason` for PreToolUse;
   - whether `SessionEnd` fires in `claude -p` (headless) mode;
   - the Stop hook's `asyncRewake: true` field and its semantics (background run; exit 2 wakes
     the agent with stderr as a system reminder) — documented in the hooks reference as of
     Oct 2026, but verify on the installed version. If it's missing, install with
     `fleet install claude --wake block`. Known from the docs: `timeout` IS enforced on
     asyncRewake hooks (600 s default — install now writes `max_wait` + 1 min), and `claude -p`
     kills async hooks at teardown (async wake is interactive-only);
   - CLI flags used by `internal/testkit/claude.go`: `-p`, `--output-format json`,
     `--settings`, `--mcp-config`, `--strict-mcp-config`, `--allowedTools`, `--max-turns`,
     `--model`; and `claude mcp add/remove --scope` used by `internal/install`.
3. **macOS and Windows at runtime.** Only cross-compiled. Run `fleet test selfcheck` on both.
   Likely trouble spots: process names/cmdlines via gopsutil, `procs.Terminate` on Windows
   (hard kill only), `fleet run` argument quoting for `.cmd`/`.bat` targets on Windows, temp
   dirs behind symlinks on macOS (`/var` → `/private/var`) in path comparisons (`paths.Canon`
   does not resolve symlinks).
4. **The live scenarios** (`fleet test claude`) have never been executed. Expect to tune
   prompts and timeouts.
5. **The idle wake path needs a manual check** — headless `claude -p` exits after its turn,
   so the live suite can only test blocking waits. In two interactive Claude Code sessions:
   1. Session A: "Run `fleet wait demo:ready --desc 'the demo service is up' --detach`, then
      end your turn."
   2. Confirm A goes idle and `fleet waits` shows the wait.
   3. Session B (or your terminal): `fleet signal demo:ready --data "http://localhost:9999"`.
   4. A should wake within a second or two and continue with the URL. If it doesn't, check
      `fleet events` for `wait.woke`, run with `FLEET_DEBUG=1`, and try `--wake block`.

## First steps

```sh
# 0. Module path is github.com/joejenniges/fleet (done)

go mod tidy                       # adds modernc.org/sqlite, writes go.sum
go build -o fleet ./cmd/fleet
go test ./...
./fleet test selfcheck            # must be 29/29 on every OS

# then, with Claude Code installed and logged in:
./fleet test standup
./fleet test claude --only presence --model <small model>
./fleet test claude               # all scenarios
./fleet test teardown
```

## Code map

| Path | What |
|---|---|
| `cmd/fleet` | `main` |
| `internal/cli` | cobra commands. Thin: parse flags, call `app`, print. |
| `internal/app` | **All business logic**: policy proposals/approval, docs, `RunCommand`, status, session context, process scan, SessionEnd cleanup. CLI, MCP and hooks share it. |
| `internal/hooks` | `fleet hook <event>`: payload parsing, fail-open timeout wrapper, deny/context output; `stop.go` is the signal wake (asyncRewake / block) |
| `internal/mcpserver` | MCP tools over stdio (official go-sdk) |
| `internal/policy` | Policy files, evaluation (rules most-restrictive, defaults most-specific, lineage), change classification, atomic writes |
| `internal/store` | SQLite schema (append-only migrations), all queries |
| `internal/cmdtext` | Tokenize, segment (`&&`, `|`, `;`), normalize, redact |
| `internal/procs` | gopsutil wrapper: (pid, ctime) identity, ancestry, snapshots, ports, terminate |
| `internal/runner` | Transparent child execution with output tail capture, signal forwarding |
| `internal/agentid` | Agent names, resolving "which agent am I" by process ancestry |
| `internal/app/signals.go`, `internal/store/signals.go`, `internal/cli/signals.go`, `internal/mcpserver/signals.go` | Signals and waits (DESIGN.md §11) |
| `internal/install` | `fleet install claude`: hooks merge into settings.json, skill, MCP registration |
| `internal/install/skill/SKILL.md` | The skill agents read (embedded in the binary) |
| `internal/testkit` | `fleet test …`: standup/teardown, fixtures, selfcheck, live scenarios |

## Conventions

- Business logic goes in `internal/app`, never in CLI/MCP/hook handlers.
- Hooks **fail open**: any error or timeout → exit 0, no output. Only deliberate denials
  block an agent.
- Never add MCP tools for human-only actions (approve/reject, preempt, priority). Those
  require an interactive terminal outside any agent session (`isHumanTerminal`) and are also
  blocked by the PreToolUse hook. Selfcheck asserts no such tool is exposed.
- Processes are identified by (pid, create time), never pid alone.
- Policy files are only written through `policy.WriteFile` (validate + atomic rename).
- Schema changes: append a migration in `internal/store/schema.go`; never edit a released one.
- Any new hook behavior gets a selfcheck step; anything that depends on agent judgment gets a
  soft check in a live scenario.
- Redact before storing anything that came from a command line or output (`a.Red`).

## Known gaps / next tasks

- Signals: no cross-machine delivery (would need `fleet serve`); consider Claude Code channels
  (`notifications/claude/channel`, research preview) as a push alternative to the Stop-hook
  wake once they leave preview. The Go SDK can declare experimental capabilities but may need
  a newer version (or a raw JSON-RPC write) to send custom notifications.

- **Phase 2** (DESIGN.md §7, §10): leases, `port allocate`, `browser-mcp`, board, `checkin`,
  preemption flow with `--dry-run`, file-claim enforcement in PreToolUse. Tables `leases`,
  `messages`, `board` already exist; stub commands print "not implemented".
- No `fleet config set` yet: agents are blocked from editing `config.toml` directly but have
  no command to change it. Add one (with restrictive/permissive classification where relevant,
  e.g. retention).
- Concurrent policy writes: two proposals applied at the same instant could race on the same
  file. Add a lock (e.g. a `BEGIN IMMEDIATE` transaction held around read-apply-write).
- `TestPID` walks ancestors up to the owning agent's process; for untracked processes it stops
  at the first shell. Revisit once leases/preemption use it heavily.
- `fleet run` captures through pipes; for a human at a TTY it skips capture. Phase 3: pty.
- Hook-recorded Bash runs have no exit code (Claude Code doesn't report one; see above).
- Recipes, git export/import of repo docs (phase 3); adapters for Codex/Gemini/Cursor and a
  TUI (phase 4).
- Release: `.goreleaser.yaml` is a starting point; add a Homebrew tap / Scoop bucket when ready.
- CI: `.github/workflows/ci.yml` runs tests and selfcheck on Linux, macOS and Windows.

## Build environment note

This scaffold was produced in a sandbox without access to golang.org / modernc.org /
go.yaml.in. Builds there used a copy of the repo with `replace` directives pointing at GitHub
mirrors. None of that is in this repo; on a normal machine plain `go mod tidy` works.
