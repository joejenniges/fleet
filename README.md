# fleet

Coordinate the coding agents running on your machine.

When several agents (Claude Code, Codex, …) work at once, they fight over ports and browser
profiles, kill each other's processes, and keep guessing at commands someone already
figured out. fleet is one cross-platform binary that gives them:

- **Presence** — who is running, where, with which processes and ports (`fleet status`).
- **A kill policy you control** — per process type: never kill `sqlcmd`, kill `node` dev
  servers quickly, ask about anything unknown. Migrations and anything wrapped in
  `fleet run --critical` are protected, including their child and parent processes.
- **Run history** — `fleet run -- <cmd>` is transparent and records exit code, duration and
  output tail (secrets redacted).
- **Shared command knowledge** — verified known-good and known-bad commands. Known-bad ones
  are blocked automatically with the fix; the most-used ones are shown to every new session.
- **Reviewed policy changes** — agents propose; conservative changes apply immediately,
  anything that could make fleet kill more waits for you.
- **Signals between agents** — an agent waits for something another agent produces
  (`fleet wait orders:schema-published --desc "…" --detach`) and goes idle; other agents see
  the description, and the one that provides it runs `fleet signal … --data "…"`, which wakes
  the waiter with the data.

See [DESIGN.md](DESIGN.md) for the full design.

## Install

```sh
go install github.com/<you>/fleet/cmd/fleet@latest     # or download a release binary
fleet install claude                                    # hooks + skill + MCP server (user scope)
fleet test selfcheck                                    # verify everything works here
```

`fleet install claude --scope project` installs into the current repo instead and adds a
short coordination section to its `CLAUDE.md`. `fleet install claude --dry-run` shows the
changes first. `fleet uninstall claude` removes them. `--wake block` installs the blocking
Stop hook instead of the background `asyncRewake` one, for Claude Code versions without it.

## Everyday commands

```sh
fleet status                                   # agents, processes, ports, orphans
fleet run --critical -- dotnet ef database update
fleet runs list --failed

fleet docs top                                 # what agents see at session start
fleet docs search ef migration
fleet docs add known-bad "dotnet ef upgrade" --from-run 812 --note "not an ef verb"
fleet docs add valid-command "dotnet ef database update" --from-run 815 --fixes 41

fleet policy test -- node server.js            # verdict per level: builtin/global/user/repo/runtime
fleet policy test --pid 4242 --format json
fleet policy add --level user --id node-dev --name node,vite --action kill --grace 3s --reason "dev servers"
fleet policy pending                           # review agents' proposals (--format json for agents)
fleet policy approve 7                         # you only, from your own terminal
fleet policy files                             # which policy files apply here
fleet events                                   # audit log

fleet wait orders:schema-published --desc "OpenAPI spec for the orders API written to disk" --detach
fleet waits                                    # who is waiting for what, and why
fleet signal orders:schema-published --data "services/orders/openapi.yaml (v2.3)"
fleet signal web:dev-server-up --clear         # signals stay set until cleared
```

## Testing after install

```sh
fleet test selfcheck        # deterministic, no agent needed
fleet test standup          # isolated test home (own DB/config/policy via FLEET_HOME)
fleet test claude           # live headless Claude Code sessions; costs tokens
fleet test teardown
```

Live scenarios report **HARD** checks (guaranteed by fleet's hooks) separately from **soft**
ones (depend on the agent following instructions), so agent non-determinism shows up as a
miss rate rather than a flaky failure.

## Configuration

- `<config dir>/fleet/config.toml` — retention, top-N, hook timeouts, extra redaction patterns
  (see `examples/config.toml`).
- Policy: `<config dir>/fleet/policy.toml` (user), `<repo>/.fleet/policy.toml` (repo, commit
  it), machine-wide global file. Change them with `fleet policy …`, not by hand.
- `fleet version` prints every location.
