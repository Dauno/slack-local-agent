# local-agent — agent instructions

Go 1.25 Slack Socket Mode agent using Google ADK + OpenAI-compatible LLM.

Module path is `github.com/Dauno/slack-local-agent` (not the directory name `local-agent`).

## Build & dev commands

```sh
go build -trimpath -o bin/local-agent ./cmd/local-agent   # production binary
go build -trimpath ./cmd/local-agent                        # verify-only (no -o)
go test ./...                                              # includes architecture dep check
go vet ./...
go mod tidy
```

No Makefile, no CI workflows, no `.golangci.yml`. `go vet` is the only lint.

## Commands

| Command | Notes |
|---------|-------|
| `bin/local-agent init` | wizard; creates artifacts + guides setup |
| `bin/local-agent init --reset-state` | destructive: deletes `.local-agent/local-agent.db` and `memory/` projections |
| `bin/local-agent doctor` | offline only; `--live` adds Slack + model checks |
| `bin/local-agent run` | requires `init` first (never bootstraps) |
| `bin/local-agent manifest [--write]` | renders Slack manifest |
| `bin/local-agent version` | build info |
| `bin/local-agent shim opencode` | hidden; cli-v1 mapper process for the `agent_cli` provider (reads one NDJSON request on stdin) |
| `bin/local-agent shim codex` | hidden; cli-v1 mapper for Codex CLI (same NDJSON contract) |

## Architecture

Hexagonal. Strict dependency rules enforced by `internal/architecture/dependencies_test.go`:

| Layer | Owns | Must not own |
|-------|------|--------------|
| `internal/domain` | stdlib only. Pure data + policy. | ADK, OpenAI, Slack, SQLite, Docker types. |
| `internal/port` | domain + stdlib. Shared interfaces. | Framework or transport implementations. |
| `internal/usecase` | domain + port. Business logic. | Adapters or third-party SDKs. |
| `internal/adapter` | Concrete implementations. | Must not import other adapters (composed in `internal/app`). |
| `internal/app` | Composition root. | Must not import CLI layer. |

**Adapters** (16): adkagent, adkartifact, agentcli, codexshim, envfile, fsproject, fssandbox, logging, memorycurator, memoryprojector, modelcalllimiter, openaillm, opencodeshim, slack, sqlite, toolfactory.

**Usecases** (5): bootstrap, bot, doctor, memory, sandbox.

**Other internal packages**: `agentdef` (agent/provider YAML definitions, stdlib+yaml.v3 only), `cliprotocol` (stdlib-only `cli-v1` NDJSON wire contract between the `agent_cli` adapter and shim processes), `manifest` (Slack app manifest rendering), `secure` (credential redaction), `cli` (cobra delivery; also hosts the hidden `shim opencode` and `shim codex` mapper commands), `buildinfo` (version metadata), `config` (path resolution).

### Agent CLI provider (`agent_cli`)

- Second provider type beside `openai_compatible`. Declarative YAML: `shim.command` (`self` or PATH executable) + `shim.args`; profiles carry `model`, optional `agent`, `approval` (`reject` default | `auto`), `variant`. HTTP fields are rejected for `agent_cli` and vice versa.
- `internal/adapter/agentcli` implements ADK `model.LLM` by spawning one shim process per model call: one `cli-v1` NDJSON request on stdin, bounded stdout/stderr, process-group kill on cancellation. Text-only: ADK tools, function history, images, and streaming are rejected before launch.
- `internal/adapter/opencodeshim` maps `cli-v1` to `opencode run --format json` (prompt on stdin, never argv; `--auto` only for `approval: auto`; never `--share`/`--continue`/`--session`). Accepts exactly OpenCode `1.18.3` in `describe`; `validate` checks the model catalog and primary agents.
- `internal/adapter/codexshim` maps `cli-v1` to `codex exec --json --ephemeral --color never -` (prompt on stdin via the `-` sentinel, never argv). Accepts exactly Codex CLI `0.144.5` in `describe`; `validate` checks the bundled model catalog (`codex debug models --bundled`), reasoning efforts (`variant` → `model_reasoning_effort`), rejects non-empty `agent`, and requires the working directory to be inside a Git worktree. `approval: reject` → `--sandbox read-only`, `auto` → `--sandbox workspace-write`; always `--ask-for-approval never`, never `--yolo`/full access/resume/output-file flags. Codex reuses its saved CLI login (no API key env); `doctor --live` checks auth per trusted shim identity (`opencode auth list` / `codex login status`, unknown identities fail closed).
- Every run receives the full canonical `sandbox.projects` registry; the app root must be registered. A CLI-backed root gets **no** ADK tool factory.
- An `openai_compatible` root may declare `agent_tools` referencing leaf agents of two forms: `agent_cli` leaves (no ADK tools, native CLI tools only, must omit `tool_scope`) and `openai_compatible` leaves that must declare `tool_scope: invocation_scoped` (e.g. `explore`). Scoped leaves receive the same invocation-scoped read-only tools as the root (`list_messages`, `list_repos`, `list_directory`, `read_file`, `list_worktrees`) bound to the trusted Slack actor and conversation key — never mutable tools or confirmations. All children are exposed through ADK `AgentTool`, use isolated in-memory child sessions, inherit the root global instruction, and do not change the durable root provider family.
- `port.AgentToolFactory.ToolsForInvocation` returns `([]any, error)`; a construction failure fails the turn instead of producing a partial tool list. `internal/app/agent_tools.go` prepares child models at startup and composes scoped children per invocation (`compositeAgentToolFactory`).
- Durable sessions are stamped with `local_agent_provider_family` state; startup and each turn fail closed on family mismatch (`init --reset-state` to switch families).

### ADK durable runtime

The agent uses **durable ADK sessions** backed by SQLite. Key types:

- `port.AgentRuntime`: `Run(ctx, req) (AgentTurn, error)` / `Resume(ctx, decision) (AgentTurn, error)`
- `port.AgentTurn` carries `Text` and optional `*PendingConfirmation`.
- `adkagent.Runtime` constructs per-turn `llmagent` with tools from `AgentToolFactory`. Session IDs: `adk:{conversation-key}`.
- `adaptersqlite.AdkSessionService` implements ADK's `session.Service` using `database/sql` (no GORM).
- Backward compat: `port.Agent.Respond` still wired in `internal/app/run.go`. Bot use case branches: `runtime != nil` → `handleRuntimeTurn()`, else legacy path.

### Confirmation flow

1. Model emits `FunctionCall` → ADK detects `RequireConfirmation: true` → emits `adk_request_confirmation` wrapper
2. `adkagent.Runtime` extracts `PendingConfirmation` from the wrapper event
3. Bot use case creates `ConfirmationDelivery` in SQLite, publishes Slack prompt
4. User replies `approve <id>` / `reject <id>` → `HandleConfirmation`
5. `HandleConfirmation` validates actor, expiry, status (not consumed), marks consumed atomically, calls `runtime.Resume()`
6. Replay protection: `MarkConsumed` rejects duplicate approvals

### Slack Markdown delivery

- All `ResponsePublisher` text is standard Markdown, sent with `chat.postMessage.markdown_text`; no top-level `text` or app-generated blocks.
- `internal/adapter/slack` owns control-sequence neutralization and deterministic splitting at 11,900 Unicode code points, including multipart labels.
- Renderer `markdown_v1` metadata contains correlation ID, one-based part index, part count, and submitted-part SHA-256 digest.
- Recovery reconstructs parts from canonical sanitized content and fails closed on missing, duplicate, reordered, edited, or inconsistent parts.
- Upgrades across renderer formats require `init --reset-state`; `run` never performs a destructive migration.

## Data directory

`.local-agent/` is mostly gitignored. Contains: `config.yaml`, `local-agent.db` (SQLite), `app-manifest.local.yaml`, `local.env.example`, and `memory/` (OKF file projections). Exceptions: `agents/` and `providers/` subdirs hold YAML definitions and are tracked in git.

`docs/` is gitignored but contains authoritative TRDs — prefer those over guessing architecture.

## Testing

- Tests use local fakes: temp SQLite, HTTP test servers, injected in-memory stores. No live credentials needed.
- `go test ./...` runs everything, including the architecture dependency check.
- Integration tests (`internal/integration`) wire real adapters with temp SQLite; no build tags needed.

## Key conventions

- **Secrets** go in `.env` (0600). **Config** goes in `.local-agent/config.yaml`.
- **Redaction**: `internal/secure.Redactor` strips credentials from logs/errors/output at the last mile.
- **Context limits**: count Unicode code points, not bytes or rune length.
- **Dedupe**: at-most-once by event + message keys. Ephemeral Slack history recovery is not persisted.
- **Canonical keys**: `slack:{team}:dm:{channel}` or `slack:{team}:channel:{channel}:thread:{root_ts}`.
- **ADK session IDs**: `adk:{canonical-conversation-key}` — deterministic, opaque, never derived from untrusted text.
- **Schema**: `PRAGMA user_version` for SQLite migrations. Current version: 10.
- **Memory**: curated entity memory stored in SQLite; `.local-agent/memory/` holds OKF file projections. Memory retrieval is deterministic (no LLM routing) and runs before each model call. Memory failure is non-fatal.
- **Ephemeral context**: Slack enrichment and memory snippets are injected per-turn via the user message text; they must never become durable ADK events.
- **Sandbox**: workspace inspection is opt-in through `sandbox.enabled` and `sandbox.projects`; `list_directory` is non-recursive and blocks `.env`, `.local-agent`, and `.git` at every depth (including symlinks).

## OpenCode config

`.opencode/opencode.json` enables `lsp: true` (Go gopls), connects to ADK docs via MCP server, and references external instruction files (`caveman.md`, `soul-rules.md`) that apply to sessions in this repo. Skills directory has 7 Google ADK skills. No repo-local agents configured.
