# Memory Recall Integration TRD

## Purpose

Augment every primary model call with curated long-term memory retrieved from
the global topic store. Memory is recalled synchronously before the ADK agent
turn and injected as untrusted reference data through the existing before-model
callback. The feature reuses the global topic namespace defined in the base
Memory TRD and adds personal-memory recall for first-person questions.

## Product Decisions

- Recall happens synchronously before every primary model call. A failure or
  empty result produces a `"memory unavailable"` status hint that reaches the
  model as a brief advisory line.
- The bot use case calculates the effective render budget for memory before
  calling `Recall`. The budget is `min(MaxMemoryChars, remaining context)`
  when `MaxMemoryChars > 0`; when `MaxMemoryChars == 0` no memory is recalled.
- Personal topics (`bundle_path = 'people'`) are excluded from ordinary FTS
  recall. They are added only by the first-person fan-out branch when the query
  matches a defined set of first-person patterns.
- The ADK runtime receives a pre-rendered memory reference string, not raw
  snippets. The bot use case renders the reference after fitting snippets into
  budget, and the runtime injects it once into `SystemInstruction`.
- Configuration introduces a new key `memory.max_memory_chars` (zero disables
  memory injection; nonzero caps the rendered reference size). The existing
  `memory.max_chars_recall` continues to bound raw snippet retrieval.
- Follow-up queries (`"cuéntame más"`, `"tell me more"`, `"¿qué hay de eso?"`)
  reuse the prior recalled topic set without a new FTS round-trip.

## Goals

- Recall relevant curated topics on every primary model call within a strict
  character budget.
- Render memory as untrusted reference data delimited by a preamble, never as
  instructions or fabricated messages.
- Let the model see a `"memory unavailable"` hint when retrieval fails so it
  can acknowledge the limitation naturally.
- Support personal-memory recall for first-person questions without exposing
  another user's personal topics.
- Keep recall idempotent, budget-enforcing, and safe under concurrent
  invocations.
- Distinguish follow-up queries and reuse prior recall results without
  additional store I/O.

## Non-Goals

- Semantic or vector retrieval.
- Per-channel or per-team memory visibility rules beyond the existing global
  scope.
- Memory tool invocation by the model at runtime.
- Logging raw query text or personal topic content.

---

## Architecture

```
Slack invocation
  → bot.Service
    → resolveRecallQuery(raw, prior) → (query, intent)
    → budget = min(MaxMemoryChars, remaining context)    [zero → skip]
    → memory.Recall(ctx, query, ownerKey, budget, opts)
        → FTS (searchTopics, ownerKey, exclude people)
        → if first-person: fan-out personal topics
        → fit into budget (MinSnippetRunes, ellipsis reservation)
        → return RecallResult{snippets, StatusHint, Truncated}
    → render memory reference from RecallResult
    → AgentRequest{…, MemoryReference: rendered}
  → ADK runtime
    → BeforeModelCallback injects into SystemInstruction
  → primary model call receives memory in system instruction
```

## Integration Boundaries

### port.AgentRequest (modified)

```go
type AgentRequest struct {
    ConversationKey domain.ConversationKey
    Messages        []domain.Message
    MemoryReference string            // Pre-rendered, budget-fitted reference or "".
    Context         domain.AgentContext
}
```

`Memory` (the `[]MemorySnippet` slice) is removed. The bot use case renders
the memory reference before calling the runtime. The runtime receives an opaque
string that is injected verbatim (after prepending a newline separator) into
`LLMRequest.Config.SystemInstruction`.

### port.MemoryRetriever (modified)

```go
type MemoryRetriever interface {
    Recall(ctx context.Context, query, ownerKey string, budget int, opts RecallOptions) (RecallResult, error)
}

type RecallOptions struct {
    MinSnippetRunes   int  // Minimum rune count a snippet must contribute; shorter snippets are skipped.
    EllipsisReserve   int  // Runes reserved for the "…" truncation marker in the reference.
    PriorResult       *RecallResult // Non-nil when this is a follow-up query.
}

type RecallResult struct {
    Snippets   []domain.MemorySnippet
    StatusHint string // "" on normal hit; "memory unavailable" on error or empty.
    Truncated  bool   // True when snippets were trimmed to fit budget.
}
```

### domain.MemoryRecallConfig (extended)

```go
type MemoryRecallConfig struct {
    Enabled        bool
    MaxTopics      int
    MaxChars       int           // Raw retrieval budget (max_chars_recall).
    MaxMemoryChars int           // Rendered reference budget; 0 disables injection.
    MinSnippetRunes int          // Minimum snippet contribution; defaults to 60.
    Timeout        time.Duration
}
```

### config.MemoryConfig (new key)

```yaml
memory:
  max_memory_chars: 0       # int; 0 disables memory injection into model context.
```

`max_memory_chars` defaults to 0 (disabled). When positive, it caps the
rendered memory reference. The existing `max_chars_recall` is the raw snippet
retrieval budget and remains positive (its current validation requires >0).

---

## Recall Pipeline

### 1. Query Resolution

```go
// resolveRecallQuery normalises the raw user text and detects follow-up intent.
// Returns the resolved FTS query and the recall intent.
func resolveRecallQuery(raw string, prior *RecallResult) (query string, intent RecallIntent)
```

**Intent classification:**

| Intent | Trigger examples | Behaviour |
|--------|------------------|-----------|
| `Explicit` | `"recuerda que…"`, `"remember…"`, `"guarda que…"` | Match topic by subject terms only. |
| `FollowUp` | `"cuéntame más"`, `"tell me more"`, `"¿qué hay de eso?"`, `"y eso?"`, `"what about that?"` | Reuse `prior` snippets; no new FTS. |
| `FirstPerson` | `"¿qué sabes de mí?"`, `"what do you know about me?"`, `"quién soy"` | Ordinary FTS + personal fan-out. |
| `Ordinary` | Everything else. | Ordinary FTS, no personal fan-out. |

**Follow-up detection** must handle:
- Leading `¿` and accented forms: `"¿qué hay de eso?"`, `"¿y eso?"`
- English: `"tell me more"`, `"what about that?"`, `"and that?"`, `"more details"`, `"elaborate"`
- Spanish: `"cuéntame más"`, `"explícate"`, `"más detalles"`, `"ahonda"`, `"continúa"`, `"sigue"`
- Intent is `FollowUp` only when `prior != nil` and the query matches a
  follow-up pattern. Otherwise it falls through to `Ordinary`.

`resolveRecallQuery` lives in `internal/domain/` and is tested in
`internal/domain/memory_test.go`. The bot use case calls it before `Recall`.

### 2. Budget Calculation (bot.Service)

```go
if s.cfg.Memory.MaxMemoryChars <= 0 {
    // No memory injection configured; skip recall entirely.
    memoryRef = ""
} else {
    remaining := s.cfg.ContextLimits.MaxChars - messageChars(modelContext)
    budget := min(s.cfg.Memory.MaxMemoryChars, remaining)
    if budget > 0 {
        opts := port.RecallOptions{
            MinSnippetRunes: s.cfg.Memory.MinSnippetRunes,
            EllipsisReserve: ellipsisLen,
            PriorResult:     priorResult,
        }
        result, err := s.recall.Recall(ctx, resolvedQuery, ownerKey, budget, opts)
        memoryRef = renderMemoryReferenceFromResult(result, err)
    }
}
```

### 3. FTS Query Construction (recall-specific)

The recall path uses a dedicated builder, `buildRecallFTSQuery`, separate from
the curator's `buildFTSQuery`. It:

1. Strips structural prefixes: `"what is"`, `"tell me about"`, `"¿qué es"`,
   `"cuéntame sobre"`, `"qué sabes de"`, `"habla de"`, `"explícame"`, etc.
2. Removes tokens matching a bilingual stoplist: `is`, `are`, `was`, `the`,
   `a`, `an`, `of`, `in`, `on`, `to`, `for`, `with`, `about`, `me`, `my`,
   `de`, `la`, `el`, `los`, `las`, `un`, `una`, `en`, `con`, `por`, `para`,
   `sobre`, `mi`, `mí`, `yo`, `tú`, `qué`, `cómo`, `cuál`, `dónde`, `quién`,
   `tell`, `know`, `sabes`, `sabe`.
3. Wraps remaining terms as double-quoted FTS5 phrases joined by `OR`.

The shared `buildFTSQuery` in the SQLite adapter is unchanged. The curator
continues to use it for topic-reference lookup, accepting that stop words may
produce no-match results for natural-language curator queries. That behaviour
is tested and accepted.

**Acceptance test for stoplist:**
- `"What is Project Apollo?"` → FTS query `"project" OR "apollo"` → matches topic `project-apollo`.
- `"Tell me about Project Apollo"` → FTS query `"project" OR "apollo"` → matches.
- `"¿Qué es el Proyecto Apolo?"` → FTS query `"proyecto" OR "apolo"` → matches.

### 4. Personal-Memory Gating

**Base FTS search (`searchTopics`):** unconditionally excludes all topics
where `bundle_path = 'people'`.

```sql
WHERE … AND t.bundle_path != 'people'
```

The `ownerKey` parameter is removed from `searchTopics`. The base search never
returns personal topics.

**First-person fan-out** is a separate call to `SearchPersonTopicsByOwner`,
guarded by `isFirstPersonMemoryQuery(query)`. It returns only topics where
`bundle_path = 'people' AND owner_key = ?`.

**`SearchTopicsForOwner`** is simplified to delegate to `searchTopics` (no
owner-based people inclusion). Its signature remains for interface
compatibility but its implementation no longer mixes personal topics into FTS
results.

**Negative AC:** User U2 asks `"¿qué sabes de mí?"` (first-person). The
fan-out returns only U2's personal topics. U1's personal topics are never
returned. A separate test verifies that `"What is Project Apollo?"` from U2
never surfaces U1's personal topic `person-dauno` because the base FTS
excludes `people` topics and the query is not first-person.

### 5. Snippet Fitting and Budget Enforcement

`FitMemorySnippets` is extended with options:

```go
type FitOptions struct {
    Budget          int  // Required.
    MinSnippetRunes int  // Snippets contributing fewer runes are dropped.
    EllipsisReserve int  // Runes subtracted from budget before fitting.
}
```

Algorithm:
1. Reserve `EllipsisReserve` runes from `Budget`.
2. For each snippet, test whether the full snippet fits within remaining budget
   (including reference framing overhead: title line, separator).
3. If not, binary-search the snippet content for the largest prefix that fits.
4. If the fitted prefix is shorter than `MinSnippetRunes`, drop the snippet.
5. Append a `…` marker to the last snippet that was truncated.
6. Return fitted snippets + `Truncated` flag.

The reference preamble (`[CURATED BACKGROUND]\n…`) is counted in the budget
during fitting. The preamble is a fixed cost.

### 6. Reference Rendering

```go
func RenderMemoryReferenceFromResult(result RecallResult, err error) string
```

| Condition | Output |
|-----------|--------|
| `err != nil` | `"[CURATED BACKGROUND]\nmemory unavailable\n"` |
| `len(result.Snippets) == 0 && result.StatusHint != ""` | `"[CURATED BACKGROUND]\nmemory unavailable\n"` |
| `len(result.Snippets) == 0` | `""` |
| Normal hit | `"[CURATED BACKGROUND]\n…"` + topic sections |

When `StatusHint != ""` and snippets are present, the hint is not injected; the
snippets speak for themselves.

### 7. beforeModelData and Runtime Injection

```go
type beforeModelData struct {
    memoryReference string
    context         domain.AgentContext
}

func (d beforeModelData) reference() string {
    // Existing context rendering + memory reference separated by "\n\n".
}
```

`injectEphemeralReference` prepends or appends to `SystemInstruction` only
when `memoryReference != ""`. When the reference is the status hint `"memory
unavailable"`, it is still injected so the model can reply naturally (e.g.,
"I don't have any stored memory about that").

**Confirmation `Resume`** is excluded from "before every primary model call."
It constructs `beforeModelData{}` (empty) and does not inject memory. This is
consistent with the existing code at `runtime.go:218`.

### 8. Store-Error Contract

A single error contract replaces the contradictory ones:

- `Recall` returns `(RecallResult, error)`.
- On **any store error** (including `context.Canceled`/`DeadlineExceeded` from
  the personal fan-out), the bot use case:
  1. Logs the redacted error at `Warn` level.
  2. Constructs `RecallResult{StatusHint: "memory unavailable"}`.
  3. The reference renderer emits `"[CURATED BACKGROUND]\nmemory unavailable\n"`.
  4. The model sees the hint. The normal response path continues.
- `context.Canceled`/`DeadlineExceeded` from the personal fan-out is **swallowed**
  as a partial failure. The FTS result (if any) is used; the personal result is
  discarded. The outcome is treated as a normal recall hit with available
  snippets.

### 9. Unicode Guarantee

The implementation guarantees: **snippet truncation never splits a rune (UTF-8
encoding)**. Grapheme clusters (combining sequences, emoji ZWJ sequences) may
be split. This is an accepted limitation documented in the TRD.

Boundary expansion for excerpt windows:
- `ExcerptWindowRunes` caps expansion per side.
- `MaxBoundaryExpansion` caps total expansion (both sides combined).
- These are consumed by the domain/usecase layer, not the SQLite adapter.

The `MemoryExcerpt` type remains in `domain/` for future excerpt-based
rendering but has no current API consumer. The TRD defers its implementation.

### 10. Excerpt Ownership

The domain/usecase layer (`memory.Service`) owns excerpt policy (window size,
boundary expansion). The SQLite adapter is responsible for:
- Executing FTS queries.
- Returning full topic content rows.

The adapter never truncates content to an excerpt window. The use case applies
excerpt policy deterministically.

### 11. Configuration Delivery

**New key:** `memory.max_memory_chars` (int, default 0).

| File | Change |
|------|--------|
| `config/config.go` `MemoryConfig` | Add `MaxMemoryChars int \`yaml:"max_memory_chars"\`` |
| `config/config.go` `Default()` | `MaxMemoryChars: 0` |
| `config/yaml.go` `configSchema` | Add `{name: "max_memory_chars"}` to memory children |
| `config/validate.go` | Allow zero (disables injection); when positive, require `>= MinSnippetRunes + preamble` |
| `config/yaml.go` `MemoryConfig` struct | Add field |
| `app/run.go` | Wire `MaxMemoryChars`, `MinSnippetRunes` into `domain.MemoryRecallConfig` and `botusecase.Config` |

**`memory.max_chars_recall`** retains its current semantics (raw snippet
retrieval budget, must be >0) and is not reused. The new key is additive.

**`MinSnippetRunes`** defaults to 60. It is not exposed as a YAML key in the
initial implementation; it is a sensible constant. A follow-up can add
`memory.min_snippet_runes` if needed.

### 12. Debug Logging

The debug gate (`log_level: debug`) logs:

- Number of snippets recalled.
- Whether the result was truncated.
- Total rendered reference length in runes.

It must **never** log:
- Raw query text (may contain PII).
- Snippet content (may contain sensitive curated facts).
- Personal topic titles or content.

All log statements pass through the redactor. The implementation uses
`logger.Debug("memory recall matched", "topics", len(snippets), "truncated", truncated, "reference_runes", runeCount)`.

---

## Follow-Up Intent: Full Specification

`resolveRecallQuery(raw, prior) (string, RecallIntent)`:

```go
type RecallIntent int

const (
    IntentOrdinary    RecallIntent = iota
    IntentExplicit
    IntentFirstPerson
    IntentFollowUp
)
```

**Detection algorithm:**

1. If `raw` matches an explicit-memory prefix (`"recuerda que"`, `"guarda que"`,
   `"remember that"`, `"save that"`, etc.) → `IntentExplicit`. Query is the
   remainder after prefix removal and deictic normalization (existing
   `normalizeExplicitFact`).

2. If `raw` matches a first-person pattern (`isFirstPersonMemoryQuery`) →
   `IntentFirstPerson`. Query is `raw` with structural prefix removed.

3. If `prior != nil` and `raw` matches a follow-up pattern → `IntentFollowUp`.
   Query is `""` (no FTS needed; prior result reused).

4. Otherwise → `IntentOrdinary`. Query is `raw` with structural prefix removed
   and stop words removed.

**Follow-up patterns (bilingual):**

| Language | Patterns |
|----------|----------|
| Spanish | `"cuéntame más"`, `"explícate"`, `"más detalles"`, `"ahonda"`, `"continúa"`, `"sigue"`, `"¿qué hay de eso?"`, `"¿y eso?"`, `"¿y qué más?"`, `"¿algo más?"`, `"dime más"`, `"amplía"`, `"profundiza"` |
| English | `"tell me more"`, `"what about that?"`, `"and that?"`, `"more details"`, `"elaborate"`, `"go on"`, `"continue"`, `"anything else?"`, `"what else?"` |

Leading `¿` and trailing `?` are stripped before matching. Accented characters
are normalised (e.g., `"qué"` matches `"qué"`).

**`IntentFollowUp` behaviour in `Recall`:**

When `opts.PriorResult != nil` and the resolved intent is `IntentFollowUp`:
- Return `opts.PriorResult` unchanged (snippets, StatusHint, Truncated).
- Do not execute FTS or personal fan-out.

---

## Acceptance Criteria

| ID | Criterion |
|----|-----------|
| AC1 | `"What is Project Apollo?"` recalls a topic with title/slug matching `Project Apollo` and injects a rendered reference into the model's system instruction. |
| AC2 | When no topic matches, the model receives no memory reference (empty string). |
| AC3 | When the store returns an error, the model receives `"[CURATED BACKGROUND]\nmemory unavailable\n"` as the memory reference. |
| AC4 | `"Tell me about Project Apollo"` (with stop words) produces the same FTS match as AC1. |
| AC5 | `"¿Qué es el Proyecto Apolo?"` (Spanish structural prefix + stop words) matches the same topic. |
| AC6 | `"cuéntame más"` after a prior recall reuses the prior snippets without a new store call. |
| AC7 | `"¿qué hay de eso?"` after a prior recall reuses the prior snippets. |
| AC8 | A first-person query (`"¿qué sabes de mí?"`) returns the caller's personal topic in addition to FTS results. |
| AC9 | **Negative:** U2 asking `"¿qué sabes de mí?"` never receives U1's personal topic. |
| AC10 | **Negative:** U2 asking `"What is Project Apollo?"` never receives U1's personal topic (base FTS excludes `people`). |
| AC11 | When `max_memory_chars` is 0, no memory is recalled and the bot does not call `Recall`. |
| AC12 | The memory reference appears in `LLMRequest.Config.SystemInstruction` as inspected by a recording LLM. |
| AC13 | **Negative:** The memory reference is absent from durable ADK events (`adk_events` table). |
| AC14 | A snippet shorter than `MinSnippetRunes` after truncation is dropped entirely. |
| AC15 | Truncated snippets produce `Truncated = true` and the last included snippet ends with `…`. |
| AC16 | The confirmation `Resume` path does not inject memory (empty `beforeModelData`). |
| AC17 | Configuration validation: `max_memory_chars: 0` is valid; `max_memory_chars: 100` requires it to be ≥ preamble + MinSnippetRunes. |
| AC18 | Debug logs contain snippet count, truncation flag, and reference length but never raw query text or snippet content. |

---

## Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| **Privacy isolation:** A bug in personal-memory gating could leak U1's personal topic to U2. | Negative ACs (AC9, AC10) gate every build. Base FTS unconditionally excludes `bundle_path = 'people'`. |
| **FTS precision regression:** The stoplist may remove terms that are meaningful in topic titles. | The stoplist is conservative (only closed-class words). Topic titles are indexed with their original case and stop words; FTS5 still matches on the remaining terms. |
| **Configuration compatibility:** The new `max_memory_chars: 0` default disables memory injection for existing configs that have `memory.enabled: true`. | Operators must explicitly set `max_memory_chars` to a positive value after reviewing the privacy disclosure. Migration is opt-in. |
| **Budget edge cases:** A very small budget (< preamble + MinSnippetRunes) must not panic. | The fitter returns 0 snippets when budget is insufficient. The reference is empty (not a status hint). |
| **Diagnostic data exposure:** Debug logs could leak sensitive curated facts. | Log only aggregate counts/lengths. All log statements pass through the redactor. |
| **`buildFTSQuery` behavioural change:** The recall-specific builder uses a different code path from the curator. | The curator's `buildFTSQuery` is unchanged. The recall builder is tested with the exact acceptance queries. |
| **Grapheme cluster splitting:** Rune-safe slicing may split emoji ZWJ sequences. | Documented limitation. Memory topics are not expected to contain complex emoji sequences. A future iteration may adopt `uax29`-based segmentation (already a transitive dependency via `tk`). |

---

## Deferred Work

- Excerpt-based rendering using `MemoryExcerpt` with grapheme-aware truncation.
- `memory.min_snippet_runes` as a user-facing YAML key.
- Follow-up intent that spans multiple conversation turns (currently limited to
  the prior result within the same invocation).
- Personal-memory recall for non-first-person queries that explicitly name a
  user (e.g., `"What do we know about Dauno?"`).
