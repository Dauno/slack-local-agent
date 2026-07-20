# Memory Recall in Agent Tool Invocation TRD

## Purpose

Enable the agent to recall curated long-term memory snippets during every invocation and present them as ephemeral, untrusted reference material in the model call. Memory recall is not an ADK function tool—it is a deterministic, pre-call operation whose results are injected via a before-model callback and never written to durable `adk_events`.

## Design Principle

Per `AGENTS.md`: *"Slack enrichment and memory snippets are injected per-turn via the user message text; they must never become durable ADK events."*

Function-tool responses are persisted in `adk_events`. Channel-thread ADK sessions may contain multiple Slack actors, so one actor's person-memory result entering durable events would leak into later actors' model context. Memory snippets must therefore travel through the `port.AgentRequest.Memory` field → `beforeModelData` → `BeforeModelCallback` path. They are attached to the system instruction for the current turn only and never appear in `adk_events` or any durable session state.

## Scope

1. Memory recall runs synchronously in `bot.Service.Handle` before the primary model call, exactly as it does today.
2. The bot use case passes recalled snippets through `port.AgentRequest.Memory`.
3. The ADK runtime (`adkagent.Runtime`) renders them as ephemeral system-instruction text via `BeforeModelCallback`.
4. A new tool is NOT registered. The `toolfactory.Factory` is unchanged.
5. A new test proves that memory snippets never enter durable ADK events, even across multiple actors and a process restart.

## Wiring

### Memory-service construction guard

Memory service construction (`memoryusecase.New`) must remain inside the existing `if cfg.Memory.Enabled` guard in `internal/app/run.go`. It must not be hoisted above or outside that guard. When `cfg.Memory.Enabled` is false, `memorySvc` must be nil and the `port.MemoryRetriever` reference in the bot service must remain nil.

The construction site (run.go, inside `if cfg.Memory.Enabled`):

```go
if cfg.Memory.Enabled {
    // ... curator model setup ...

    memorySvc, memErr := memoryusecase.New(memoryusecase.Config{
        Recall: domain.MemoryRecallConfig{
            Enabled:   true,
            MaxTopics: cfg.Memory.MaxTopicsRecall,
            MaxChars:  cfg.Memory.MaxCharsRecall,
            Timeout:   time.Duration(cfg.Memory.RecallTimeoutSeconds) * time.Second,
        },
        Limits:      domain.MemoryLimits{MaxTopics: cfg.Memory.MaxTopics, MaxLinks: cfg.Memory.MaxLinks, MaxTopicChars: cfg.Memory.MaxTopicChars},
        MaxPatchOps: cfg.Memory.MaxPatchOps,
    }, memoryusecase.Dependencies{Store: store, Logger: logger, SanitizeContent: redactor.String})
    if memErr != nil {
        return redactor.Error(fmt.Errorf("initialize memory service: %w", memErr))
    }
    // ...
    service.AddMemory(memorySvc, store)
}
```

`service.AddMemory` sets the `recall` and `exchange` fields on the bot service. When memory is disabled, both remain nil and no recall occurs.

### CLI-root gate

When the root agent is an `agent_cli` provider (`rootIsAgentCLI == true`), the `toolFactory` variable in `run.go` is never assigned—the entire `if !rootIsAgentCLI` block is skipped. The ADK runtime receives a nil `ToolFactory` and registers no ADK function tools. Memory recall is unaffected because it runs in the bot service, not in the tool factory. The CLI-root path must never create a `toolfactory.Factory`.

### OpenAI-compatible root

For OpenAI-compatible roots, the existing `toolFactory` is created normally. Memory recall remains a pre-call operation in the bot service. No memory-related function tool is added to the factory.

## Budget Semantics

### `FitMemorySnippets` (domain/memory_core.go)

Uses an exact, configurable total Unicode-code-point limit (`maxChars`, default 2 000 in config). It attempts to fit complete snippets first. When a complete snippet would exceed the remaining budget, it performs a binary search on that snippet's content to find a truncated prefix that fits. A snippet that cannot fit at all (best == 0) is skipped.

### `mergeRecallSnippets` (usecase/memory/service.go)

Used when the query is a first-person memory question. Merges FTS results with personal provenance results. Applies both a topic-count cap (`maxTopics`) and a character budget (`maxChars`). Snippets whose content length exceeds the remaining budget are **skipped** entirely; they are never truncated. This is intentional: personal memory snippets are provenance records whose partial inclusion would be misleading.

### Recall path in bot service

In `bot.Service.Handle`, after recall, the bot calls `domain.FitMemorySnippets(snippets, remainingBudget)` where `remainingBudget = contextMaxChars - messageChars(modelContext)`. This ensures the model prompt stays within the total configured context budget.

## Constructor Inventory

`toolfactory.New` is called in exactly seven locations:

| # | File | Location | Arguments |
|---|------|----------|-----------|
| 1 | `internal/app/run.go` | `toolFactory = toolfactory.New(store, sandboxService)` | `(store adaptersqlite.Store, sandboxService *sandboxusecase.Service)` |
| 2 | `internal/adapter/toolfactory/toolfactory_test.go` | `TestFactoryWithoutSandboxExposesOnlyConversationTools` | `(store, nil)` |
| 3 | `internal/adapter/toolfactory/toolfactory_test.go` | `TestFactoryWithSandboxExposesAllReadOnlyTools` | `(store, sb)` |
| 4 | `internal/adapter/toolfactory/toolfactory_test.go` | `TestFactoryNilStoreReturnsNil` | `(nil, nil)` |
| 5 | `internal/integration/e2e_fakes_test.go` | `newE2EService` | `(store, sandboxService)` |
| 6 | `internal/integration/e2e_test.go` | `TestE2E_ContextSurvivesRestart` (first) | `(store, nil)` |
| 7 | `internal/integration/e2e_test.go` | `TestE2E_ContextSurvivesRestart` (second) | `(reopened, nil)` |

All seven are unchanged by this design.

## Result Contract

When memory recall returns snippets, they are represented as the `domain.MemorySnippet` DTO with all fields mapped:

| Field | Type | Description |
|-------|------|-------------|
| `TopicID` | `TopicID` | Stable topic identifier |
| `Title` | `string` | Topic title |
| `Slug` | `string` | URL-safe topic slug |
| `Content` | `string` | Current curated content |
| `RevisionNumber` | `int` | Latest revision number |
| `RevisedAt` | `time.Time` | ISO 8601 timestamp in UTC (`2006-01-02T15:04:05Z`) |
| `Source` | `string` | Provenance label |

When serialized to JSON (e.g., for test assertions or debugging), an empty result set must encode as `{"snippets":[]}`, never `{"snippets":null}` or `null`.

The `RevisedAt` format is RFC 3339 / ISO 8601 with second precision and `Z` suffix: `2026-07-10T12:00:00Z`.

This DTO is distinct from the model-facing rendered text produced by `domain.RenderMemoryReference`. The DTO carries structured fields for programmatic use; the rendered text is the string injected into the system instruction via the before-model callback.

## Tests

### Enabled/disabled OpenAI root test

A test constructs the bot service with `cfg.Memory.Enabled = true` and verifies that `s.recall` is non-nil. A second test with `cfg.Memory.Enabled = false` verifies `s.recall` is nil. Both use an OpenAI-compatible root (non-CLI).

### CLI-root gate test

An application-level construction test proves that when the root agent is an `agent_cli` provider (`rootIsAgentCLI == true`), no `toolfactory.Factory` is constructed and the `toolFactory` variable remains nil. The test must reference the production `rootIsAgentCLI` gate, not a composite factory with a nil base.

### Cross-actor / restart ephemeral test

A test with two distinct Slack actors (e.g., `U111` and `U222`) in the same channel-thread ADK session:

1. Actor `U111` sends a message containing a first-person memory declaration ("Mi nombre es Dauno").
2. The curator processes the exchange and creates a scoped person topic (`person-dauno-slack-T12345678-user-U111`).
3. Actor `U111` sends a follow-up that triggers first-person recall; the model response contains the personal snippet.
4. Actor `U222` sends a message in the same thread. The test inspects the model request for `U222` and proves it contains NO scoped person-memory snippets belonging to `U111`.
5. The test restarts the process (closes and reopens SQLite) and repeats step 4, proving durable `adk_events` contain no leaked snippets.

### Budget tests

- **Aggregate multi-snippet test**: Provide more snippets than fit in the budget. Verify `FitMemorySnippets` returns only those that fit (including a truncated final snippet when partial content fits).
- **First-person merge test**: Verify `mergeRecallSnippets` deduplicates by `TopicID`, respects `maxTopics`, and skips—never truncates—snippets that exceed the remaining character budget.
- **Empty-budget test**: Budget of 0 returns nil.

## Non-Goals

- Adding a memory recall ADK function tool.
- Exposing memory recall results in durable ADK session events.
- Changing the `toolfactory.Factory` API or its constructor signature.
- Budget truncation in `mergeRecallSnippets` (deliberately skips, matching current code).
