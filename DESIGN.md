# fleet — design

A single cross-platform Go binary that coordinates many coding agents running on one machine:
who is running, what they hold, what may be killed, and which commands are known to work.

Status: design v0.1 (2026-10-27). Working name `fleet`.

---

## 1. Goals and non-goals

**Goals**

1. Every agent on the machine can see every other agent: directory, processes, ports, purpose.
2. Shared resources (ports, browser profiles, test DBs) are leased, not fought over.
3. A higher-priority agent can take resources from a lower-priority one *safely*, under a
   policy the human controls.
4. Commands that work (and commands that fail) are recorded with evidence and shared, so
   agents stop guessing.
5. Install is one binary on PATH plus `fleet install <agent>`.

**Non-goals (for now)**

- Multi-machine coordination. One machine = one fleet. WSL and native Windows are separate fleets.
- Task planning / ticket management (Beads, Lodestar and friends already do this).
- Sandboxing agents. fleet coordinates cooperative agents; it is not a security boundary.

**Prior art borrowed from**

| Project | What we take |
|---|---|
| dibs | No daemon, single binary; claims with expiry; hooks that *enforce*; handoff notes; repo-committed knowledge |
| MCP Agent Mail | Memorable agent names; glob reservations; threaded, searchable messages; audit trail |
| Switchboard | One `checkin` that heartbeats + renews leases + drains inbox; shared key/value board; `init` that writes the agent-instructions blurb and skill |

---

## 2. Architecture

```
            ┌───────────── agent A (Claude Code) ─────────────┐
            │  hooks ──► fleet hook <event>                    │
            │  MCP   ──► fleet mcp            (stdio, per agent)│
            │  Bash  ──► fleet run [--critical] -- <cmd>        │
            └───────────────────────┬─────────────────────────┘
                                    │  all modes read/write
            ┌───────────────────────▼─────────────────────────┐
            │  state dir: fleet.db (SQLite, WAL)               │
            │  config dir: config.toml, policy.toml            │
            │  repo: .fleet/policy.toml, .fleet/docs/ (git)    │
            └───────────────────────▲─────────────────────────┘
                                    │
            human ──► fleet status | policy pending/approve | preempt | ui
```

- **One binary, many modes.** `fleet mcp`, `fleet hook <event>`, `fleet run`, and the human CLI
  are all the same executable.
- **No daemon.** Every invocation opens the DB, does its work, and exits. Housekeeping (reaping
  dead agents, expiring leases, retention) runs lazily inside normal invocations, rate-limited
  through a `meta` table timestamp.
- **SQLite via `modernc.org/sqlite`** (pure Go, no cgo → trivial cross-compile). WAL mode,
  `busy_timeout=5000`, one connection per process.
- **Fail open.** A hook that errors or exceeds its timeout exits 0 with no output, so a broken
  fleet never stalls agents. The only exceptions are enforcement decisions the user configured
  (known-bad blocking, policy-file edit blocking, claim conflicts) — and even those fail open if
  the DB is unreachable.

### Locations

| | Linux | macOS | Windows |
|---|---|---|---|
| State (DB) | `$XDG_STATE_HOME/fleet` or `~/.local/state/fleet` | `~/Library/Application Support/fleet` | `%LOCALAPPDATA%\fleet` |
| User config | `$XDG_CONFIG_HOME/fleet` | `~/Library/Application Support/fleet` | `%APPDATA%\fleet` |
| Global (machine) policy | `/etc/fleet` | `/Library/Application Support/fleet` | `%ProgramData%\fleet` |
| Repo | `<repo root>/.fleet/` | same | same |

`FLEET_HOME` overrides state+user config (used by tests and for isolated setups).

---

## 3. Identity

- **Agent ID** = the agent's own session ID when the agent provides one (Claude Code hooks
  pass `session_id`). Otherwise a random ULID.
- **Display name**: deterministic two-word name derived from the ID (`amber-falcon`), for humans.
- **Agent process**: at SessionStart the hook walks its own ancestor chain and records the
  nearest ancestor that is not a shell or `fleet` itself (sh, bash, zsh, fish, dash, cmd,
  powershell, pwsh, conhost). That PID + its create time is the agent process.
- **Finding yourself later**: `fleet mcp` and `fleet run` are also descendants of the agent
  process. They walk their own ancestors and match against registered agent processes.
  `FLEET_AGENT_ID` overrides this when set.
- **Process identity is always (pid, create_time)**, never pid alone — PIDs get reused,
  especially on Windows.

---

## 4. Data model (SQLite)

```
agents      id PK, name, kind (claude|codex|gemini|cursor|other), session_id,
            pid, pid_ctime, cwd, repo_root, priority (int, default 0),
            status (active|shelved|ended), started_at, last_seen, ended_at, note
processes   id PK, agent_id, pid, pid_ctime, name, cmdline, cwd, ports (json),
            purpose, critical (bool), critical_reason, source (run|hook|scan|mark),
            first_seen, last_seen, ended_at
runs        id PK, agent_id, repo_root, cwd, command, norm_cmd, argv0,
            exit_code (nullable), duration_ms, started_at, ended_at,
            critical (bool), source (run|hook), os, tool_version,
            output (text, capped), output_lines, output_compacted (bool)
docs        id PK, kind (valid-command|known-bad|behavior|recipe|note), scope (machine|repo),
            repo_root, title, command, norm_cmd, body, fix_doc_id (known-bad → good),
            evidence_run_id, verified (bool), created_by, created_at, updated_at, retired_at
doc_marks   doc_id, repo_root, file (CLAUDE.md|AGENTS.md|GEMINI.md|…), agent_id,
            marked_at, expires_at          -- "I already know this from my instructions file"
policy_proposals
            id PK, level (user|repo|global), repo_root, change (json), class
            (restrictive|permissive), status (pending|applied|approved|rejected),
            proposed_by, proposed_at, decided_at, decided_note
policy_comments
            id PK, proposal_id, agent_id, stance (support|oppose|neutral), body, at
leases      resource PK, kind (port|browser|db|lock|glob), agent_id, expires_at, meta (json)
messages    id PK, from_agent, to_agent (or '*'), type (preempt|release_request|fyi|policy),
            body (json), created_at, delivered_at
board       key PK, value, agent_id, updated_at
events      id PK, at, agent_id, kind, detail (json)       -- audit log
meta        key PK, value                                   -- schema version, last prune, …
```

Exactly-once delivery: `UPDATE messages SET delivered_at=? WHERE to_agent=? AND delivered_at IS NULL RETURNING …`.

---

## 5. Hooks

Hooks are what make fleet reliable instead of "used when the agent remembers". The Claude Code
mapping is below; other agents get thin adapters with the same `fleet hook <event>` entry point.

| Event | fleet does | Output to agent |
|---|---|---|
| **SessionStart** | Register agent (id, name, cwd, repo, process). Lazy housekeeping. | Context block: your name; other active agents (name, repo, ports); pending policy proposals + how to review; top-N commands for this repo; undelivered messages. |
| **UserPromptSubmit** | Heartbeat, drain inbox. | Undelivered messages, if any. |
| **PreToolUse** (Bash) | Match each simple command in the line against known-bad docs → block with the known-good alternative. Block agent attempts to run `fleet policy approve/reject`. Stamp `lastpre:<agent>` (used by the PostToolUse scan). Denials are logged as `hook.denied` events. | Deny + reason, or nothing. |
| **PreToolUse** (Edit/Write) | Block edits to any `policy.toml` / `config.toml` managed by fleet; point to `fleet policy add`. Phase 2: block edits to paths claimed by another agent. | Deny + reason, or nothing. |
| **PostToolUse** (Bash) | Record the command as a run (`source=hook`; exit code when the agent reports it; skipped for `fleet run …`, which records itself). Track long-lived processes: descendants of the agent process that listen on a port, **plus** any listener created since the agent's last PreToolUse that no other agent owns (catches `server &`, which is re-parented away from the agent immediately). Heartbeat. | Undelivered messages, if any. |
| **SessionEnd** | Mark agent ended. Release leases. For each still-running tracked process, evaluate policy: `kill` → terminate; otherwise leave it and flag it as an orphan in `fleet status`. | — |

Notes:

- `Stop` fires at the end of every turn, not at session end; cleanup belongs in `SessionEnd`.
- Hook timeouts: 2 s default, 5 s for SessionStart. Configurable.
- Port scanning is the slow part on macOS (gopsutil shells out to `lsof`); it only runs in
  PostToolUse for Bash and is skipped if it ran within the last 3 s.
- Everything a hook prints is untrusted-data-safe: no free text from other agents is injected
  as an instruction; messages are rendered as typed, quoted data.

---

## 6. Policy

### 6.1 Files and levels

Levels, lowest to highest specificity:

| Level | File | Who edits |
|---|---|---|
| `builtin` | compiled into the binary | release |
| `global` | `<global dir>/policy.toml` | human only (agents may *propose*; approval required for any change) |
| `user` | `<user config>/policy.toml` | `fleet policy add/remove` |
| `repo` | `<repo>/.fleet/policy.toml` (committed) | `fleet policy add/remove` |
| `runtime` | DB: critical marks from `fleet run --critical` / `mark_critical` | automatic |

```toml
[defaults]
unknown = "ask"        # never | ask | kill   — processes no rule matches
grace = "10s"          # SIGTERM→SIGKILL window (Unix); wait-for-agent window (Windows)
on_timeout = "notify"  # notify | kill        — when an `ask` deadline passes

[[rule]]
id = "node-dev"                     # stable id, used by `fleet policy remove`
name = ["node", "vite", "next-server", "esbuild"]
action = "kill"
grace = "3s"
reason = "dev servers are stateless"

[[rule]]
id = "db-cli"
name = ["sqlcmd", "psql", "mysql", "flyway", "liquibase"]
action = "never"

[[rule]]
id = "migrations"
cmdline_regex = "(migrate|db:push|db:seed|ef database update)"
action = "never"
```

Matching fields (all present fields must match): `name` (argv0 basename, case-insensitive,
`.exe` stripped), `args_contains` (substring of the full command line), `cmdline_regex`.

### 6.2 Semantics

- **Rules: most restrictive wins across every level.** `never` > `ask` > `kill`. Order of rules
  and levels never matters. A sloppy rule can make fleet more cautious, never less. Builtin rules
  (DB clients, migrations) therefore act as a floor and cannot be removed.
- **Defaults: most specific level wins** (repo > user > global > builtin), field by field. This
  is what lets a user opt into `unknown = "kill"` at all; with most-restrictive semantics the
  builtin `ask` would make that impossible.
- **Grace** = the longest grace among matched rules at the winning action (a rule without
  `grace` uses the effective default).
- **on_timeout**: `notify` beats `kill`.
- **Unmatched** processes use the effective `defaults.unknown`.
- **Protection flows through lineage** (we kill whole trees):
  - *Descendants*: the decision for a process is the most restrictive among it and all its
    descendants, because they die with it. `node runner.js` → `psql -f seed.sql` is `never`.
  - *Ancestors*: an ancestor within the agent's tree that resolves to `never` protects the
    process, because killing part of a protected job leaves it half-done. `npm run db:migrate`
    → `node prisma …` is protected even though `node` is `kill`. (An `ask` ancestor does not
    protect; only `never`.)
- **Critical marks** (runtime level) resolve to `never` for their lifetime.

Classification used for proposals:

- Rules (monotone because most-restrictive wins): adding a `never`/`ask` rule or removing a
  `kill` rule is **restrictive**; adding a `kill` rule or removing a `never`/`ask` rule is
  **permissive**.
- Defaults: compare effective defaults before and after. Restrictive iff `unknown` does not
  move toward `kill`, `grace` does not shrink, and `on_timeout` does not move to `kill`.

### 6.3 Changing policy

All changes go through commands; direct edits are blocked by the PreToolUse hook and, if made
anyway, fleet validates on load and falls back to the last good copy (cached in the DB).

```
fleet policy add    --level user|repo|global  --id ID [--name a,b] [--args-contains S]
                    [--cmdline-regex R] --action never|ask|kill [--grace 5s] --reason "…"
fleet policy remove --level … --id ID --reason "…"
fleet policy set-default --level … [--unknown ask] [--grace 10s] [--on-timeout notify]
```

- **Restrictive** changes apply immediately (except at `global`, which always needs approval)
  and are recorded as proposals with `status=applied` for the audit trail.
- **Permissive** changes become `pending` proposals.

### 6.4 Reviewing pending changes

```
fleet policy pending [--format pretty|json]      # pretty is default
fleet policy show <proposal-id> [--format …]
fleet policy comment <proposal-id> --stance support|oppose|neutral "text"
fleet policy approve <proposal-id> [--note "…"]  # human only
fleet policy reject  <proposal-id> [--note "…"]  # human only
```

- `approve`/`reject` require an interactive terminal (stdin is a TTY) and are blocked by the
  PreToolUse hook when an agent tries them. Agents cannot approve their own (or anyone's) changes.
- Every agent sees pending proposals in its SessionStart context and is instructed (skill +
  instructions blurb) to:
  1. tell the user there are N pending policy changes and how to approve them
     (`fleet policy pending`, then `fleet policy approve <id>`),
  2. give its own opinion — and if it is the *author*, say so and explain why it needs it;
  3. if it is **not** the author, consider whether the change hurts *its* work (e.g. a `kill`
     rule that would hit a process it relies on) and record that with `fleet policy comment`.
- JSON output includes `is_mine` per proposal (relative to the calling agent) and all comments,
  so an agent can review in one call.

### 6.5 Testing policy

```
fleet policy test -- dotnet ef database update
fleet policy test --pid 4242
fleet policy test --format json -- node server.js
```

Returns, per level, the matched rules and that level's verdict, plus the final decision:

```json
{
  "input": {"argv0": "node", "cmdline": "node server.js"},
  "levels": [
    {"level": "builtin", "file": "(builtin)", "matched": [], "verdict": null, "default_unknown": "ask"},
    {"level": "global",  "file": "/etc/fleet/policy.toml", "exists": false},
    {"level": "user",    "file": "~/.config/fleet/policy.toml",
     "matched": [{"id": "node-dev", "action": "kill", "grace": "3s"}], "verdict": "kill"},
    {"level": "repo",    "file": "/src/app/.fleet/policy.toml", "matched": [], "verdict": null},
    {"level": "runtime", "critical": false}
  ],
  "final": {"action": "kill", "grace": "3s", "on_timeout": "notify",
            "decided_by": ["user:node-dev"], "protected_by_descendant": null}
}
```

With `--pid`, `runtime` and descendant protection are evaluated against the live process tree.

---

## 7. Preemption (phase 2)

1. Human runs `fleet preempt <agent> [--resource port:5173|browser:*|all] [--deadline 2m] [--dry-run]`
   (or `fleet priority <agent> high` and a higher-priority agent requests a held resource).
2. Plan is computed: each tracked process, the policy decision, and what will happen. Printed;
   requires confirmation unless `--yes`. `--dry-run` stops here.
3. Leases are revoked; a typed `preempt` message is queued. The target sees it at its next hook
   and is instructed to shelve: WIP commit or stash, `fleet note` a handoff, stop its own processes.
4. After the deadline, remaining processes are handled per policy: `kill` → terminate (Unix:
   SIGTERM, wait grace, SIGKILL, whole tree, children first; Windows: hard kill of tree);
   `ask` → `on_timeout`; `never` → untouched, reported.
5. Every action is written to `events` with the rule that allowed it.

Priority is set by the human (`fleet priority`), never by an agent about itself.

---

## 8. `fleet run` and run history

```
fleet run [--critical] [--reason "…"] [--purpose "…"] [--no-record] -- <command> [args…]
```

- Transparent wrapper: same stdin, stdout/stderr passthrough, same exit code, Ctrl-C and
  SIGTERM forwarded to the child. Output is tee'd into a capped buffer for recording.
- The run row is inserted *before* exec (so crashes are visible) and completed after.
- `--critical` marks the child (pid, ctime) as `never` for its lifetime; the mark is removed
  on exit. Any command whose `cmdline_regex` resolves to `never` is critical automatically.
- Recorded per run: command, normalized command, cwd, repo, OS, exit code, duration, agent,
  and best-effort tool version, cached per tool per day. Only an allowlist of well-known tools
  (dotnet, node, npm, go, python, git, docker, …) is ever probed: running `<arbitrary> --version`
  could execute a script with side effects.
- On failure, `fleet run` prints a one-line hint with the exact `fleet docs add known-bad …
  --from-run N` command, which is how agents learn to record mistakes.
- **Redaction before storage** (command and output): `-P <x>`, `--password[= ]x`,
  `Password=…;`, `pwd=…`, `token=…`, `Authorization: Bearer …`, `AKIA…`, `ghp_…`/`github_pat_…`,
  `sk-…`, `xox[bp]-…`, `*_TOKEN=…`/`*_SECRET=…`/`*_PASSWORD=…` env assignments.
  User-extensible via `config.toml`.
- Known limitation: output is captured through pipes, so the child does not see a TTY. Agents
  don't have one anyway; for humans, `fleet run` skips capture when stdout is a terminal
  (phase 3: pty support on Unix).

### Retention (`config.toml`)

```toml
[runs]
max_age = ""              # "" = keep forever (default). e.g. "180d"
max_output_bytes = 262144 # cap captured output per run at record time (keeps the tail)
compact_after = "14d"     # older runs keep only the last N lines of output
compact_keep_lines = 40
```

`fleet prune` applies retention on demand; it also runs lazily at most once a day.

---

## 9. Knowledge base (`fleet docs`)

### 9.1 Entries

```
fleet docs add <kind> "<command or title>" [--note "…"] [--from-run <run-id>]
               [--fixes <known-bad-id>] [--scope repo|machine]
kinds: valid-command | known-bad | behavior | recipe | note
```

Examples:

```
fleet docs add known-bad "dotnet ef upgrade" --from-run 812 --note "not a valid ef verb"
fleet docs add valid-command "dotnet ef database update" --from-run 815 --fixes 41
fleet docs add behavior "integration tests need docker compose up -d db first"
```

- An entry backed by a run (`--from-run`, or a matching successful run found automatically) is
  **verified**; one without evidence is shown as **unverified**.
- **Tallies are derived, not stored**: success/failure counts, last success, last failure, and
  the tool versions seen, all computed from `runs` with the same normalized command (scoped to
  the repo for repo-scope entries). An entry whose recent runs fail is flagged
  `stale?` in listings.
- Normalization: trim, collapse whitespace, strip a leading `fleet run [flags] --`, lowercase
  and `.exe`-strip argv0 only, redact secrets.
- Scope defaults to `repo` inside a git repo, else `machine`.

### 9.2 Top commands at SessionStart

- **Ordered by execution count** (runs, both `fleet run` and hook-recorded Bash, in the last
  90 days for that repo; machine-scope entries count across repos), descending. Top N = 10
  by default (`[docs] top_n`).
- **Already-known suppression**: an entry is skipped if
  1. fleet finds its command verbatim in the agent's instructions file(s) for that repo
     (`CLAUDE.md`, `.claude/CLAUDE.md`, `AGENTS.md`, `GEMINI.md`, … depending on agent kind) —
     checked automatically each SessionStart, cheap and deterministic; or
  2. an agent marked it: `fleet docs known <doc-id> --file CLAUDE.md` (for paraphrased
     entries the substring check can't see).
- Marks are per `(doc, repo, instructions file)` — Codex reading `AGENTS.md` still gets the
  command even if Claude's `CLAUDE.md` has it.
- Marks **expire after 14 days** (`[docs] known_mark_ttl`). After expiry the entry competes for
  the top-N list again; if it's still in the file, the agent (or the automatic check) re-marks it.
- The skill tells agents: when you see a top command that your instructions file already
  covers, run `fleet docs known`.

### 9.3 Known-bad interception

PreToolUse matches each Bash command against `known-bad` entries by normalized-token prefix
(`dotnet ef upgrade` matches `dotnet ef upgrade --verbose`). Default action is **block** with
a message naming the known-good fix if one is linked. `[docs] known_bad_action = "block" | "off"`.

### 9.4 Recipes (phase 3)

Named multi-step procedures, `fleet do <recipe> [--param k=v]`, each step optionally critical,
stored as docs of kind `recipe` with a TOML body.

### 9.5 Sharing via git (phase 3)

`fleet docs export` writes repo-scope entries to `.fleet/docs/<slug>.toml` (one file per entry,
merge-friendly); `fleet docs import` on SessionStart picks up entries pulled from git. Tallies
stay machine-local (they'd churn the repo otherwise).

---

## 10. Leases, ports, browsers, board (phase 2)

- `fleet lease acquire <kind>:<name> [--ttl 30m]`, `release`, `list`. Glob leases on paths.
- `fleet port allocate [--prefer 5173]` → a free port, leased to the caller.
- `fleet browser-mcp [-- <chrome-devtools-mcp args>]` — MCP command wrapper: leases a profile
  slot (`profile-1`, `profile-2`, …), then execs `npx chrome-devtools-mcp@latest --userDataDir <slot>`.
  Slot released when the wrapper exits.
- `fleet board set/get/list` — shared scratch (`dev-server:<repo> → http://localhost:5173`),
  which also gives dev-server reuse for free.
- `checkin` (MCP + every hook) = heartbeat + renew own leases + drain inbox.

---

## 11. MCP tools

| Tool | Phase |
|---|---|
| `whoami`, `list_agents`, `status` | 1 |
| `docs_top`, `docs_search`, `docs_add`, `docs_mark_known` | 1 |
| `runs_recent` (own / repo history) | 1 |
| `policy_test`, `policy_propose`, `policy_pending`, `policy_comment` | 1 |
| `mark_critical`, `annotate_process` | 1 |
| `checkin`, `send_message` | 2 |
| `lease_acquire`, `lease_release`, `allocate_port`, `board_get`, `board_set` | 2 |
| `note_handoff` | 2 |

There are deliberately no MCP tools for `approve`, `reject`, `preempt`, or `priority`.

---

## 12. Install

```
fleet install claude [--scope user|project] [--dry-run]
```

- Registers the MCP server (`claude mcp add --scope user fleet -- fleet mcp` if `claude` is on
  PATH; otherwise prints the snippet).
- Merges hooks into `~/.claude/settings.json` (backup written first; idempotent; tagged so
  `fleet uninstall` can remove exactly what it added).
- Writes the skill to `~/.claude/skills/fleet/SKILL.md`.
- With `--scope project`, appends a short coordination section to the repo's `CLAUDE.md`.

Adapters for Codex / Gemini CLI / Cursor follow the same shape (phase 4).

Distribution: goreleaser → GitHub releases (linux/darwin/windows × amd64/arm64), Homebrew tap,
Scoop bucket, `go install`.

---

## 13. Security notes

- The DB and config files are created `0600`/`0700`; on Windows they live under the user profile.
- Agent-to-agent messages are typed; free text is displayed as quoted data, never as an instruction.
- Agents cannot approve policy or preempt; those require a TTY and are hook-blocked.
- Redaction is best-effort; `--no-record` exists for commands that must never be stored.
- fleet is not a sandbox. A misbehaving agent can bypass it by not using it.

---

## 14. Testing

Three layers:

1. **Unit tests** (`go test ./...`): policy semantics and classification, tokenizing,
   normalization and redaction, the run wrapper, settings merging.
2. **`fleet test selfcheck`**: deterministic end-to-end run in a throwaway `FLEET_HOME`.
   Drives every hook with simulated Claude Code payloads, runs `fleet run` and `fleet mcp` as
   real subprocesses, and really starts and kills fixture processes. 19 checks: registration,
   cross-agent visibility, known-bad blocking, approve blocking, policy-file edit blocking,
   proposal queueing, exactly-once broadcast, restrictive auto-apply, per-level `policy test`,
   hook run recording, top-N ordering and suppression (file + marks, per instructions family),
   exit-code passthrough + redaction, auto-critical, SessionEnd kill/spare, reaping, retention
   compaction, MCP handshake and tool list. No agent needed; suitable for CI on all three OSes.
3. **Live Claude scenarios** (`fleet test standup` → `fleet test claude` → `fleet test teardown`):
   - `standup` creates `<dir>/fleet-home` (FLEET_HOME), `work/` (one git repo per scenario),
     `claude/settings.json` (fleet hooks pointing at this binary, omitted if the user's own
     settings already run fleet hooks, to avoid double-firing) and `claude/mcp.json`, `logs/`,
     and `env.sh`/`env.ps1`. A pointer in the user cache dir records the current test home.
   - Isolation is by environment only: test sessions are launched with `FLEET_HOME` set; nothing
     global is switched, so real agents keep using the real DB during a test run.
   - `claude` runs `claude -p … --settings … --mcp-config … --strict-mcp-config
     --allowedTools … --output-format json` per session, transcripts saved to `logs/`.
   - Every check is **HARD** (guaranteed by hooks regardless of agent choices; failure = fleet
     bug or a Claude Code behavior change) or **soft** (depends on the agent following
     instructions; reported as a miss, not a failure). `--repeat N` turns soft checks into a rate.
   - Scenarios: `presence`, `broadcast`, `known-bad`, `approve-blocked`, `kill-on-end`,
     `record-docs`.
   - `teardown` kills every fixture (`--fleet-test-fixture=` processes) and deletes the home.
   - Fixtures are built into the binary (`fleet test serve-port|exit|sleep`, hidden), so tests
     need no Python/Node/shell utilities and behave the same on Windows.

## 15. Phases

1. **Core** ✅ scaffolded: store + migrations, agents/identity, `status`, `run` (+ history,
   redaction, retention, `prune`), policy engine (`test`, `add/remove/set-default`, proposals,
   `pending`, `comment`, `approve/reject`), docs (`add`, `list`, `search`, `top`, `known`,
   `retire`), hooks (SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, SessionEnd),
   MCP phase-1 tools, `install/uninstall claude`, `fleet test`.
2. **Resources**: leases, ports, `browser-mcp`, board, messages/checkin, preemption, priority,
   handoff notes, file claims enforcement.
3. **Knowledge+**: recipes, git export/import of docs, pty for `fleet run`.
4. **Reach**: Codex / Gemini / Cursor adapters, `fleet ui` (TUI), `fleet uninstall`.

## 16. Open questions

- Should repo-level permissive changes require approval if the repo file is committed and
  reviewed in PRs anyway? (Current answer: yes — PR review happens too late.)
- Exit codes for hook-recorded Bash runs depend on what the agent reports; Claude Code's
  `tool_response` shape should be verified and parsed when it includes one.
- Should `fleet run` auto-suggest `docs add` after a failure followed by a success with the same
  first two tokens? (Likely yes, phase 3.)
