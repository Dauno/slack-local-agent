# local-agent — agent instructions

Go 1.25 Slack Socket Mode agent using Google ADK + OpenAI-compatible LLM.

## Entrypoint

`cmd/local-agent/main.go` — one binary, cobra CLI.

## Build & dev commands

```sh
go build -trimpath -o bin/local-agent ./cmd/local-agent   # production binary
go build -trimpath ./cmd/local-agent                        # verify-only (no -o)
go test ./...                                              # includes architecture dep check
go vet ./...
go mod tidy
```

No Makefile, no CI workflows, no `.golangci.yml`. `go vet` is the only lint.

Release metadata: `-ldflags "-X github.com/Dauno/slack-local-agent/internal/buildinfo.Version=vX.Y.Z -X ...Commit=... -X ...Date=..."`

## Commands

| Command | Notes |
|---------|-------|
| `bin/local-agent init` | wizard; creates artifacts + guides setup |
| `bin/local-agent doctor` | offline only; `--live` adds Slack + model checks |
| `bin/local-agent run` | requires `init` first (never bootstraps) |
| `bin/local-agent manifest [--write]` | renders Slack manifest |
| `bin/local-agent version` | build info |

## Architecture

Hexagonal. Strict dependency rules enforced by `internal/architecture/dependencies_test.go`:

- `internal/domain` — stdlib only. Pure data + policy.
- `internal/port` — domain + stdlib only. Shared interfaces.
- `internal/usecase` — must not import adapters or third-party SDKs. Sub-packages: `bootstrap`, `bot`, `doctor`, `memory`.
- `internal/adapter` — must not import other adapters (composed in `internal/app`). Ten adapters: adkagent, envfile, fsproject, logging, memorycurator, memoryprojector, modelcalllimiter, openaillm, slack, sqlite.
- `internal/app` — composition root; must not import CLI layer.

`internal/integration` is a cross-package test layer that wires adapters and usecases together. Exempt from the architecture dependency check (`_test.go` files only).

`docs/ARCHITECTURE.md` is the authoritative architecture reference but is **gitignored** (local-only). The rules above are the executable subset.

## Data directory

`.local-agent/` is gitignored. Contains: `config.yaml`, `local-agent.db` (SQLite), `app-manifest.local.yaml`, `local.env.example`, and `memory/` (OKF file projections).

## Testing quirks

- Tests use only local fakes: temp SQLite, HTTP test servers, injected in-memory stores. No live credentials needed.
- `go test ./...` runs all tests, including the architecture dependency check.
- Domain tests: table-driven, package-internal (`package domain`).
- CLI tests inject streams + fakes.
- Integration tests (`internal/integration`) wire real adapters with temp SQLite; no build tags needed.

## Key conventions

- **Secrets** go in `.env` (0600). **Config** goes in `.local-agent/config.yaml`.
- **Redaction**: `internal/secure.Redactor` (via `secure.NewRedactor(secrets...)`) strips credentials from logs/errors/output at the last mile.
- **Context limits**: count Unicode code points, not bytes or rune length.
- **Dedupe**: at-most-once by event + message keys. Ephemeral Slack history recovery is not persisted.
- **Canonical keys**: `slack:{team}:dm:{channel}` or `slack:{team}:channel:{channel}:thread:{root_ts}`.
- **Schema**: `PRAGMA user_version` for SQLite migrations.
- **Memory**: curated entity memory stored in SQLite; `.local-agent/memory/` holds OKF file projections. Memory retrieval is deterministic (no LLM routing) and runs before each model call. Memory failure is non-fatal — the agent answers normally without it.

## OpenCode config

`.opencode/opencode.json` loads caveman mode instruction and ADK docs MCP server. Skills directory has ADK + ponytail skills. No repo-local agents configured.
