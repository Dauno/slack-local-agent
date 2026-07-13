# local-agent

`local-agent` is a local-first conversational Slack agent written in Go. It
connects through Slack Socket Mode, uses Google ADK for orchestration, calls an
OpenAI-compatible Chat Completions endpoint, and keeps recent conversation
state in a project-local SQLite database.

The MVP is conversational only. It has no shell, filesystem, repository,
tool-calling, or autonomous background-task access.

## Requirements

- Go 1.25 or later to build from source.
- A Slack app with Socket Mode enabled.
- A Bot User OAuth token (`xoxb-...`).
- An app-level token (`xapp-...`) with `connections:write`.
- An API key for an OpenAI-compatible Chat Completions endpoint.

## Install

```sh
./install.sh
```

By default the binary is placed in `$HOME/.local-agent/bin/local-agent`. Override
the destination with `PREFIX`:

```sh
PREFIX=/usr/local ./install.sh
```

Build metadata is injected automatically from the current git revision. Override
with `VERSION`, `COMMIT`, and `DATE` environment variables.

### Manual build

```sh
go build -trimpath -o bin/local-agent ./cmd/local-agent
```

Optional release metadata can be injected with `-ldflags` into
`internal/buildinfo.Version`, `Commit`, and `Date`.

## Setup

Run setup from the directory whose Slack context and state should remain
isolated:

```sh
local-agent init
```

The wizard creates missing base artifacts first, then guides Slack app creation,
installation, Socket Mode token creation, access control, model credentials, and
privacy confirmation. Confirmed secrets are written only to `.env` with mode
`0600` where supported.

Generated local state:

```text
.local-agent/
  app-manifest.local.yaml
  config.yaml
  local-agent.db
  local.env.example
.env
```

Then validate and run:

```sh
local-agent doctor
local-agent doctor --live
local-agent run
```

`doctor` is offline by default. Only `doctor --live` contacts Slack and the
configured model endpoint. `run` never bootstraps missing files; use `init`
first.

Other commands:

```sh
local-agent manifest
local-agent manifest --write
local-agent version
```

## Configuration

Non-sensitive settings live in `.local-agent/config.yaml`. Missing YAML fields
receive explicit defaults. The generated model defaults are:

```yaml
model:
  name: deepseek-v4-flash
  base_url: https://api.deepseek.com
  api_key_env: DEEPSEEK_API_KEY
  reasoning_effort: high
  extra_body:
    thinking:
      type: enabled
```

Sensitive values are resolved from the process environment first, then the
project `.env`:

```text
DEEPSEEK_API_KEY=...
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...
```

Static `model.headers` are optional but deliberately reject credential-bearing
headers. Authentication belongs in `model.api_key_env`.

## Privacy

Recent authorized Slack conversation messages are stored locally in SQLite.
Message content sent to the bot is sent to Slack and to the configured model
endpoint when producing a response. Channel and thread replies are visible to
everyone who can read that Slack conversation, including people who are not
authorized to invoke the bot.

Recognizable credentials are redacted from persisted message content, logs,
setup summaries, doctor output, and application errors.

## Verification

```sh
go test ./...
go vet ./...
go build -trimpath ./cmd/local-agent
```

The suite uses temporary SQLite databases and local HTTP/Slack fakes; live
provider credentials are not needed.
