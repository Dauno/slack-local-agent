# local-agent

`local-agent` is a local-first Slack bot written in Go. It connects through
Slack Socket Mode, uses Google ADK for agent orchestration, calls an
OpenAI-compatible Chat Completions endpoint, and persists conversation state
in a project-local SQLite database.

## Requirements

- Go 1.25 or later to build from source.
- A Slack app with Socket Mode enabled.
- A Bot User OAuth token (`xoxb-...`).
- An app-level token (`xapp-...`) with `connections:write`.
- An API key for an OpenAI-compatible Chat Completions endpoint.

## Install

With Go 1.25+ installed, download and build the latest release:

```sh
curl -fsSL https://raw.githubusercontent.com/Dauno/local-agent/main/install.sh | bash
```

The installer downloads the GitHub release source archive and records its tag
and commit in `local-agent version`. Install a specific release with:

```sh
curl -fsSL https://raw.githubusercontent.com/Dauno/local-agent/main/install.sh \
  | VERSION=v0.2.0 bash
```

From a local clone, `./install.sh` builds the checkout. An untagged checkout is
reported as `dev`:

```sh
./install.sh
```

The binary is placed in `$HOME/.local-agent/bin/local-agent`. Make sure the
directory is on your `PATH`:

```sh
export PATH="$HOME/.local-agent/bin:$PATH"
```

Override the destination with `PREFIX`:

```sh
PREFIX=$HOME/.local/bin ./install.sh
```

Build metadata includes the selected tag, current commit, and build date.

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

### Slack message formatting

Responses and configured public messages use standard Markdown. Slack receives
them through `chat.postMessage.markdown_text`; links and media do not unfurl.
Long responses are split into labeled parts without knowingly cutting tables,
code fences, inline links, or inline code. Model-generated Slack mention and
broadcast control sequences are neutralized outside code.

When upgrading from a binary that published Slack `mrkdwn`, stop the process,
back up `.local-agent/` if its local history matters, then run:

```sh
local-agent init --reset-state
```

The reset deletes conversation and dedupe records, durable ADK sessions and
events, prepared assistant exchanges, pending confirmation deliveries, tool
execution audit records, curated memory, and memory file projections. It keeps
configuration, provider and agent definitions, generated setup artifacts, and
secrets. Customized `busy_message`, `model_error_message`, and
`unauthorized_message` values must be converted from Slack-specific `mrkdwn` to
standard Markdown. Rolling back to an older renderer requires another explicit
state reset; `run` never resets state automatically.

### Workspace inspection

Filesystem inspection is disabled by default. To let authorized Slack users
inspect selected projects, explicitly enable the sandbox and register logical
project names in `.local-agent/config.yaml`:

```yaml
sandbox:
  enabled: true
  projects:
    workspace: .
  command_timeout_seconds: 30
  max_output_bytes: 65536
```

Relative project paths are resolved against the directory where `local-agent`
started. The agent can discover registered names, list one directory at a time,
and read bounded UTF-8 text files. It cannot execute commands or modify files.

The filesystem boundary rejects absolute and parent-traversal paths, access
outside registered roots, and unsafe symlinks. `.env`, `.local-agent`, and
`.git` are unavailable at every depth; similarly named paths such as
`.env.example`, `.gitignore`, and `.github` remain available. Source files can
still contain embedded secrets, so register only projects whose source may be
sent to the configured model endpoint.

## Privacy

Recent authorized Slack conversation messages are stored locally in SQLite.
Message content sent to the bot is sent to Slack and to the configured model
endpoint when producing a response. Channel and thread replies are visible to
everyone who can read that Slack conversation, including people who are not
authorized to invoke the bot.

Recognizable credentials are redacted from persisted message content, logs,
setup summaries, doctor output, and application errors.

When workspace inspection is enabled, requested source content is also sent to
the configured model endpoint. Restricted local state and credential paths are
blocked before content reaches the model; generic secret scanning of allowed
source files is not provided.

## Verification

```sh
go test ./...
go vet ./...
go build -trimpath ./cmd/local-agent
```

The suite uses temporary SQLite databases and local HTTP/Slack fakes; live
provider credentials are not needed.

## Changelog

- 2026-07-15: Test entry for CI/CD workflow demo.
