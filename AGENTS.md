# local-agent — agent instructions

Go 1.25 Slack Socket Mode agent using Google ADK + OpenAI-compatible LLM.

## Entrypoint

`cmd/local-agent/main.go` — one binary, cobra CLI.

## Build & dev commands

```sh
go build -trimpath -o bin/local-agent ./cmd/local-agent
go test ./...
go vet ./...
```

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

Hexagonal (`docs/ARCHITECTURE.md` is authoritative). Strict dependency rules enforced by `internal/architecture/dependencies_test.go`:

- `internal/domain` — stdlib only. Pure data + policy.
- `internal/port` — domain + stdlib only. Shared interfaces.
- `internal/usecase` — must not import adapters or third-party SDKs.
- `internal/adapter` — must not import other adapters (composed in `internal/app`).
- `internal/app` — composition root; must not import CLI layer.

## Testing quirks

- Tests use only local fakes: temp SQLite, HTTP test servers, injected in-memory stores. No live credentials needed.
- `go test ./...` runs all tests. Architecture dependency check included.
- Domain tests: table-driven, package-internal (`package domain`).
- CLI tests inject streams + fakes.
- No integration test build tags needed.

## Key conventions

- **Secrets** go in `.env` (0600). **Config** goes in `.local-agent/config.yaml`.
- **Redaction**: `internal/secure.Redactor` strips credentials from logs/errors/output at the last mile.
- **Context limits**: count Unicode code points, not bytes or rune length.
- **Dedupe**: at-most-once by event + message keys. Ephemeral Slack history recovery is not persisted.
- **Canonical keys**: `slack:{team}:dm:{channel}` or `slack:{team}:channel:{channel}:thread:{root_ts}`.
- **Schema**: `PRAGMA user_version` for SQLite migrations.

## OpenCode config

`.opencode/opencode.json` loads caveman mode instruction and ADK docs MCP server. Skills directory has ADK + ponytail skills. No repo-local agents configured.
