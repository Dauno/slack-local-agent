package bot

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const incrementalRendererVersion = "standard_incremental_v1"

func (s *Service) handleStreamingTurn(
	ctx, modelCtx context.Context,
	cancel func(),
	invocation domain.Invocation,
	key domain.ConversationKey,
	modelContext []domain.Message,
	memory []domain.MemorySnippet,
	agentContext domain.AgentContext,
	metadata domain.ConversationMetadata,
	modelRelease func(),
	progress *domain.ProgressOperation,
) (Outcome, error) {
	now := s.clock.Now().UTC()
	operation := domain.IncrementalOperation{
		ID:              "incremental:" + invocation.TeamID + ":" + invocation.ChannelID + ":" + invocation.EventTS,
		ConversationKey: key, ChannelID: invocation.ChannelID, ThreadTS: invocation.ReplyTarget().ThreadTS,
		RendererVersion: incrementalRendererVersion, Status: domain.IncrementalPrepared,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.standardStore.PrepareIncremental(ctx, operation); err != nil {
		modelRelease()
		cancel()
		return "", fmt.Errorf("prepare incremental delivery: %w", err)
	}

	sanitizer := newIncrementalSanitizer(s.sanitize, s.cfg.StreamingCarryRunes)
	var terminal port.AgentStreamEvent
	var deliveryErr error
	lastSnapshot := ""
	lastWrite := time.Time{}
	createAttempted := false

	func() {
		defer modelRelease()
		s.streamingRuntime.Stream(modelCtx, port.AgentRequest{
			ConversationKey: key, Messages: modelContext, Memory: memory, Context: agentContext,
		}, func(event port.AgentStreamEvent) bool {
			switch event.Kind {
			case port.AgentStreamTextDelta:
				sanitizer.Add(event.TextDelta)
				snapshot := sanitizer.Snapshot(false)
				if snapshot == "" || snapshot == lastSnapshot {
					return true
				}
				if operation.MessageTS != "" && s.clock.Now().Sub(lastWrite) < s.cfg.UpdateInterval {
					return true
				}
				if operation.MessageTS == "" {
					createAttempted = true
				}
				if err := s.deliverIncremental(ctx, invocation.ReplyTarget(), &operation, snapshot); err != nil {
					deliveryErr = err
					_ = s.standardStore.AdvanceIncremental(ctx, operation.ID, domain.IncrementalUnknown, operation.Sequence, operation.PrefixDigest, s.clock.Now().UTC())
					return false
				}
				lastSnapshot, lastWrite = snapshot, s.clock.Now()
				return true
			case port.AgentStreamPendingConfirmation, port.AgentStreamCompleted, port.AgentStreamError:
				terminal = event
				return event.Kind != port.AgentStreamError
			default:
				deliveryErr = fmt.Errorf("unsupported agent stream event %q", event.Kind)
				return false
			}
		})
	}()
	cancel()

	if deliveryErr != nil {
		s.updateProgress(ctx, progress, domain.ProgressFailed)
		if operation.MessageTS == "" && !createAttempted {
			if _, err := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.cfg.ModelErrorMessage); err != nil {
				return OutcomePublishFailed, nil
			}
			return OutcomeModelFailed, nil
		}
		return OutcomePublishFailed, nil
	}
	if terminal.Kind == port.AgentStreamError {
		s.logger.Error("streaming model call failed", "conversation_key", key, "error", terminal.Err)
		s.updateProgress(ctx, progress, domain.ProgressFailed)
		if operation.MessageTS == "" {
			_ = s.standardStore.AdvanceIncremental(ctx, operation.ID, domain.IncrementalInterrupted, operation.Sequence, operation.PrefixDigest, s.clock.Now().UTC())
			if _, err := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.cfg.ModelErrorMessage); err != nil {
				return OutcomePublishFailed, nil
			}
			return OutcomeModelFailed, nil
		}
		s.interruptIncremental(ctx, &operation, sanitizer.Snapshot(false), "_Interrupted._")
		return OutcomeModelFailed, nil
	}
	if terminal.Kind == port.AgentStreamPendingConfirmation && terminal.Turn != nil && terminal.Turn.PendingConfirmation != nil {
		s.updateProgress(ctx, progress, domain.ProgressWaitingConfirmation)
		s.interruptIncremental(ctx, &operation, sanitizer.Snapshot(false), "_Waiting for approval._")
		outcome, err := s.handlePendingConfirmation(ctx, invocation, key, *terminal.Turn)
		if outcome != OutcomeResponded || err != nil {
			s.updateProgress(ctx, progress, domain.ProgressFailed)
		}
		return outcome, err
	}
	if terminal.Kind != port.AgentStreamCompleted || terminal.Turn == nil {
		s.updateProgress(ctx, progress, domain.ProgressFailed)
		return OutcomeModelFailed, nil
	}

	if sanitizer.raw.Len() == 0 {
		sanitizer.Add(terminal.Turn.Text)
	}
	finalText := sanitizer.Snapshot(true)
	if strings.TrimSpace(finalText) == "" || finalText != s.sanitize(terminal.Turn.Text) {
		s.updateProgress(ctx, progress, domain.ProgressFailed)
		s.interruptIncremental(ctx, &operation, sanitizer.Snapshot(false), "_Interrupted._")
		return OutcomeModelFailed, nil
	}
	s.updateProgress(ctx, progress, domain.ProgressFinalizing)
	if operation.MessageTS == "" {
		outcome, err := s.finalizeTurn(ctx, invocation, key, terminal.Turn.Text, metadata)
		status := domain.IncrementalInterrupted
		if err == nil && outcome == OutcomeResponded {
			status = domain.IncrementalFinalized
			s.updateProgress(ctx, progress, domain.ProgressCleared)
		} else {
			s.updateProgress(ctx, progress, domain.ProgressFailed)
		}
		_ = s.standardStore.AdvanceIncremental(ctx, operation.ID, status, operation.Sequence, operation.PrefixDigest, s.clock.Now().UTC())
		return outcome, err
	}

	outcome, err := s.finalizeIncrementalTurn(ctx, invocation, key, metadata, &operation, finalText)
	if err == nil && outcome == OutcomeResponded {
		s.updateProgress(ctx, progress, domain.ProgressCleared)
	} else {
		s.updateProgress(ctx, progress, domain.ProgressFailed)
	}
	return outcome, err
}

func (s *Service) deliverIncremental(ctx context.Context, target domain.ReplyTarget, operation *domain.IncrementalOperation, snapshot string) error {
	operation.Sequence++
	operation.PrefixDigest = incrementalDigest(snapshot)
	operation.UpdatedAt = s.clock.Now().UTC()
	if operation.MessageTS == "" {
		published, err := s.incrementalPublisher.CreateIncremental(ctx, target, *operation, snapshot)
		if err != nil {
			return err
		}
		if published.LastMessageTS == "" {
			return fmt.Errorf("incremental publisher returned no timestamp")
		}
		operation.MessageTS = published.LastMessageTS
		operation.Status = domain.IncrementalMessageCreated
		if err := s.standardStore.MarkIncrementalCreated(ctx, operation.ID, operation.MessageTS, operation.UpdatedAt); err != nil {
			return err
		}
	} else {
		operation.Status = domain.IncrementalUpdating
		if err := s.incrementalPublisher.UpdateIncremental(ctx, *operation, snapshot); err != nil {
			return err
		}
	}
	return s.standardStore.AdvanceIncremental(ctx, operation.ID, operation.Status, operation.Sequence, operation.PrefixDigest, operation.UpdatedAt)
}

func (s *Service) interruptIncremental(ctx context.Context, operation *domain.IncrementalOperation, visible, marker string) {
	operation.Sequence++
	operation.Status = domain.IncrementalInterrupted
	text := fitIncrementalMarker(visible, marker)
	operation.PrefixDigest = incrementalDigest(text)
	operation.UpdatedAt = s.clock.Now().UTC()
	if operation.MessageTS != "" {
		if err := s.incrementalPublisher.InterruptIncremental(ctx, *operation, text); err != nil {
			s.logger.Warn("incremental interruption update failed", "operation_id", operation.ID, "error", err)
			_ = s.standardStore.AdvanceIncremental(ctx, operation.ID, domain.IncrementalUnknown, operation.Sequence, operation.PrefixDigest, operation.UpdatedAt)
			return
		}
	}
	if err := s.standardStore.AdvanceIncremental(ctx, operation.ID, domain.IncrementalInterrupted, operation.Sequence, operation.PrefixDigest, operation.UpdatedAt); err != nil {
		s.logger.Warn("incremental interruption persistence failed", "operation_id", operation.ID, "error", err)
	}
}

func (s *Service) finalizeIncrementalTurn(ctx context.Context, invocation domain.Invocation, key domain.ConversationKey, metadata domain.ConversationMetadata, operation *domain.IncrementalOperation, finalText string) (Outcome, error) {
	var prepared port.PreparedAssistantExchange
	if s.exchange != nil {
		var err error
		prepared, err = s.exchange.PrepareAssistantExchange(ctx, metadata, domain.Message{
			Role: domain.RoleAssistant, Content: finalText, CreatedAt: s.clock.Now().UTC(),
		}, s.cfg.RetainMessages, s.memoryEnabled && len(invocation.Attachments) == 0)
		if err != nil {
			s.interruptIncremental(ctx, operation, "", "_Interrupted._")
			return "", fmt.Errorf("prepare streamed assistant exchange: %w", err)
		}
	}
	correlationID := prepared.CorrelationID
	if correlationID == "" {
		correlationID = operation.ID
	}
	operation.Sequence++
	operation.PrefixDigest = incrementalDigest(finalText)
	operation.UpdatedAt = s.clock.Now().UTC()
	if err := s.incrementalPublisher.FinalizeIncremental(ctx, *operation, finalText, correlationID); err != nil {
		_ = s.standardStore.AdvanceIncremental(ctx, operation.ID, domain.IncrementalUnknown, operation.Sequence, operation.PrefixDigest, operation.UpdatedAt)
		return OutcomePublishFailed, nil
	}
	if s.exchange != nil && prepared.ID != "" {
		if err := s.exchange.MarkAssistantExchangePublished(ctx, prepared.ID, operation.MessageTS); err != nil {
			return "", fmt.Errorf("mark streamed assistant exchange published: %w", err)
		}
		if err := s.exchange.FinalizeAssistantExchange(ctx, prepared.ID); err != nil {
			return "", fmt.Errorf("persist streamed assistant exchange: %w", err)
		}
	} else {
		metadata.LastTS = operation.MessageTS
		if err := s.store.AppendMessage(ctx, metadata, domain.Message{
			Role: domain.RoleAssistant, Content: finalText, ExternalTS: operation.MessageTS, CreatedAt: s.clock.Now().UTC(),
		}, s.cfg.RetainMessages); err != nil {
			return "", fmt.Errorf("persist streamed assistant message: %w", err)
		}
	}
	operation.Status = domain.IncrementalFinalized
	if err := s.standardStore.AdvanceIncremental(ctx, operation.ID, operation.Status, operation.Sequence, operation.PrefixDigest, operation.UpdatedAt); err != nil {
		return "", fmt.Errorf("finalize incremental operation: %w", err)
	}
	s.logger.Info("streaming Slack invocation completed", "conversation_key", key, "event_id", invocation.EventID)
	return OutcomeResponded, nil
}

func (s *Service) ReconcileIncremental(ctx context.Context) error {
	if !s.cfg.StreamingEnabled || s.standardStore == nil || s.incrementalPublisher == nil {
		return nil
	}
	operations, err := s.standardStore.ListUnfinishedIncremental(ctx)
	if err != nil {
		return fmt.Errorf("list unfinished incremental deliveries: %w", err)
	}
	for index := range operations {
		operation := &operations[index]
		published, found, err := s.incrementalPublisher.RecoverIncremental(ctx, *operation)
		if err != nil {
			return err
		}
		if operation.MessageTS == "" && found {
			operation.MessageTS = published.LastMessageTS
			if err := s.standardStore.MarkIncrementalCreated(ctx, operation.ID, operation.MessageTS, s.clock.Now().UTC()); err != nil {
				return err
			}
		}
		if !found {
			if err := s.standardStore.AdvanceIncremental(ctx, operation.ID, domain.IncrementalInterrupted, operation.Sequence, operation.PrefixDigest, s.clock.Now().UTC()); err != nil {
				return err
			}
			continue
		}
		s.interruptIncremental(ctx, operation, "", "_Interrupted after restart._")
	}
	return nil
}

func incrementalDigest(text string) string {
	digest := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", digest)
}

func fitIncrementalMarker(text, marker string) string {
	suffix := marker
	if strings.TrimSpace(text) != "" {
		suffix = "\n\n" + marker
	}
	available := 11900 - len([]rune(suffix))
	runes := []rune(text)
	if len(runes) > available {
		runes = runes[:available]
	}
	return string(runes) + suffix
}
