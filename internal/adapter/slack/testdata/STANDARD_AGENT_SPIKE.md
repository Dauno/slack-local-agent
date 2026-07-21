# Standard Agent Slack Contract Spike

Observed 2026-07-21 in a development workspace through Socket Mode and the
standard messaging APIs. Identifiers, timestamps, text, tokens, URLs, block IDs,
and client message IDs in committed fixtures are synthetic or sanitized.

## Observed Contracts

- App Home must enable a writable Messages Tab. Event subscriptions and
  `im:history`/`im:write` alone leave user-to-app messaging disabled.
- A root `message.im` has no `thread_ts`; its `ts` is the conversation root.
- A DM reply carries `thread_ts` equal to the root `ts` and a distinct `ts`.
- An unacknowledged root was redelivered with the same event/message identity,
  `retry_attempt: 2`, and `retry_reason: timeout`.
- User edits arrive as hidden `message_changed` events containing current and
  previous messages. Deletes arrive as hidden `message_deleted` events with
  `deleted_ts` and `previous_message`.
- Deleting a reply also emits a root `message_changed` reflecting the new reply
  count. These mutation events must not invoke the model.
- `chat.update` accepts `markdown_text`, preserves message/thread identity, and
  exposes an `edited` marker in history.
- Metadata event types containing dots are accepted with warning
  `invalid_metadata_schema` but discarded. Underscore-only event types persist
  through post, update, history, and Events API payloads.
- An update at 11,900 Unicode code points succeeded. An update at 12,001 failed
  with `msg_too_long`. A three-second update interval did not receive a 429.
- The configured `deepseek/flash-reasoning` OpenAI-compatible profile produced
  nine genuine SSE text deltas before one authoritative final response. The
  first delta arrived before completion; no reasoning content was exposed.

## Not Yet Proven

- Free-plan eligibility and behavior.
- Mobile thread navigation and edited-marker presentation.
- Exact rate-limit threshold or retry window under sustained updates.
- Ambiguous network failure after Slack accepts an update.
- History shape after a process crash at each durable delivery state.

No production capability should claim these unproven contracts.
