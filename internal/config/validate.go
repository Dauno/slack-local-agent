package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var (
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	slackUserIDPattern     = regexp.MustCompile(`^[UW][A-Z0-9]{8,}$`)
	slackTeamIDPattern     = regexp.MustCompile(`^T[A-Z0-9]{8,}$`)
	slackChannelIDPattern  = regexp.MustCompile(`^[CG][A-Z0-9]{8,}$`)
	projectNamePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// FieldError identifies one invalid configuration field.
type FieldError struct {
	Field   string
	Problem string
}

func (e FieldError) Error() string {
	return fmt.Sprintf("%s %s", e.Field, e.Problem)
}

// ValidationError aggregates all configuration problems found in one pass.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	if e == nil || len(e.Fields) == 0 {
		return "invalid configuration"
	}

	parts := make([]string, 0, len(e.Fields))
	for _, field := range e.Fields {
		parts = append(parts, field.Error())
	}
	return "invalid configuration: " + strings.Join(parts, "; ")
}

// Has reports whether validation found a problem for field.
func (e *ValidationError) Has(field string) bool {
	if e == nil {
		return false
	}
	for _, problem := range e.Fields {
		if problem.Field == field {
			return true
		}
	}
	return false
}

// Validate checks cfg without mutating it and reports all actionable problems.
func Validate(cfg Config) error {
	var problems []FieldError
	add := func(field, problem string) {
		problems = append(problems, FieldError{Field: field, Problem: problem})
	}
	requireText := func(field, value string) {
		if strings.TrimSpace(value) == "" {
			add(field, "must not be empty")
		}
	}

	requireText("agent.name", cfg.Agent.Name)
	requirePath(&problems, "state.dir", cfg.State.Dir)
	requirePath(&problems, "state.db", cfg.State.DB)

	if cfg.Context.MaxMessages <= 0 {
		add("context.max_messages", "must be greater than zero")
	}
	if cfg.Context.MaxChars <= 0 {
		add("context.max_chars", "must be greater than zero")
	}
	if cfg.Context.RetainMessagesPerConversation <= 0 {
		add("context.retain_messages_per_conversation", "must be greater than zero")
	}

	switch cfg.Runtime.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		add("runtime.log_level", "must be one of debug, info, warn, or error")
	}
	if cfg.Runtime.ModelTimeoutSeconds < 0 {
		add("runtime.model_timeout_seconds", "must be non-negative (0 disables the application-level timeout)")
	}
	if cfg.Runtime.SlackAPITimeoutSeconds < 0 {
		add("runtime.slack_api_timeout_seconds", "must be non-negative")
	}
	if cfg.Runtime.MaxConcurrentModelCalls <= 0 {
		add("runtime.max_concurrent_model_calls", "must be greater than zero")
	}
	requireText("runtime.busy_message", cfg.Runtime.BusyMessage)
	requireText("runtime.model_error_message", cfg.Runtime.ModelErrorMessage)

	requireText("model.name", cfg.Model.Name)
	validateBaseURL(&problems, cfg.Model.BaseURL)
	if !environmentNamePattern.MatchString(cfg.Model.APIKeyEnv) {
		add("model.api_key_env", "must be a valid environment variable name such as DEEPSEEK_API_KEY")
	}
	switch cfg.Model.ReasoningEffort {
	case "none", "minimal", "low", "medium", "high", "xhigh":
	default:
		add("model.reasoning_effort", "must be one of none, minimal, low, medium, high, or xhigh")
	}
	validateHeaders(&problems, cfg.Model.Headers)
	if _, err := json.Marshal(cfg.Model.ExtraBody); err != nil {
		add("model.extra_body", fmt.Sprintf("must contain JSON-compatible values: %v", err))
	}
	if _, present := cfg.Model.ExtraBody["stream"]; present {
		add("model.extra_body.stream", "is reserved; streaming is not supported in the MVP")
	}

	requireText("slack.app_name", cfg.Slack.AppName)
	requireText("slack.bot_display_name", cfg.Slack.BotDisplayName)
	requireText("slack.unauthorized_message", cfg.Slack.UnauthorizedMessage)
	validateIDs(&problems, "slack.allowed_user_ids", cfg.Slack.AllowedUserIDs, slackUserIDPattern, "a plausible Slack user ID beginning with U or W")
	validateIDs(&problems, "slack.allowed_team_ids", cfg.Slack.AllowedTeamIDs, slackTeamIDPattern, "a plausible Slack team ID beginning with T")
	validateIDs(&problems, "slack.allowed_channel_ids", cfg.Slack.AllowedChannelIDs, slackChannelIDPattern, "a plausible Slack public or private channel ID beginning with C or G")
	validateIDs(&problems, "opencode.management.allowed_user_ids", cfg.OpenCode.Management.AllowedUserIDs, slackUserIDPattern, "a plausible Slack user ID beginning with U or W")

	const maxFileBytes = 5 * 1024 * 1024
	const maxFileChars = 20_000
	if cfg.Slack.Files.MaxBytesPerFile <= 0 {
		add("slack.files.max_bytes_per_file", "must be greater than zero")
	} else if cfg.Slack.Files.MaxBytesPerFile > maxFileBytes {
		add("slack.files.max_bytes_per_file", fmt.Sprintf("must not exceed %d", maxFileBytes))
	}
	if cfg.Slack.Files.MaxProcessedChars <= 0 {
		add("slack.files.max_processed_chars", "must be greater than zero")
	} else if cfg.Slack.Files.MaxProcessedChars > maxFileChars {
		add("slack.files.max_processed_chars", fmt.Sprintf("must not exceed %d", maxFileChars))
	}

	if cfg.Slack.Context.Enabled {
		if cfg.Slack.Context.MaxChars <= 0 {
			add("slack.context.max_chars", "must be greater than zero when enabled")
		}
		if cfg.Slack.Context.TimeoutSeconds <= 0 {
			add("slack.context.timeout_seconds", "must be greater than zero when enabled")
		} else if cfg.Runtime.SlackAPITimeoutSeconds > 0 && cfg.Slack.Context.TimeoutSeconds > cfg.Runtime.SlackAPITimeoutSeconds {
			add("slack.context.timeout_seconds", "must not exceed runtime.slack_api_timeout_seconds when that timeout is enabled")
		}
		if cfg.Slack.Context.ProfileCacheTTLMinutes <= 0 {
			add("slack.context.profile_cache_ttl_minutes", "must be greater than zero when enabled")
		}
		if cfg.Slack.Context.ConversationCacheTTLMinutes <= 0 {
			add("slack.context.conversation_cache_ttl_minutes", "must be greater than zero when enabled")
		}
	} else {
		if cfg.Slack.Context.MaxChars < 0 {
			add("slack.context.max_chars", "must not be negative")
		}
		if cfg.Slack.Context.TimeoutSeconds < 0 {
			add("slack.context.timeout_seconds", "must not be negative")
		}
		if cfg.Slack.Context.ProfileCacheTTLMinutes < 0 {
			add("slack.context.profile_cache_ttl_minutes", "must not be negative")
		}
		if cfg.Slack.Context.ConversationCacheTTLMinutes < 0 {
			add("slack.context.conversation_cache_ttl_minutes", "must not be negative")
		}
	}

	if cfg.Memory.MaxTopicsRecall <= 0 {
		add("memory.max_topics_recall", "must be greater than zero")
	}
	if cfg.Memory.MaxCharsRecall <= 0 {
		add("memory.max_chars_recall", "must be greater than zero")
	}
	if cfg.Memory.CuratorTimeoutSeconds <= 0 {
		add("memory.curator_timeout_seconds", "must be greater than zero")
	}
	if cfg.Memory.RecallTimeoutSeconds <= 0 {
		add("memory.recall_timeout_seconds", "must be greater than zero")
	}
	if cfg.Memory.CuratorMaxRetries <= 0 {
		add("memory.curator_max_retries", "must be greater than zero")
	}
	if cfg.Memory.WorkerIntervalSeconds <= 0 {
		add("memory.worker_interval_seconds", "must be greater than zero")
	}
	if cfg.Memory.RetentionDays <= 0 {
		add("memory.retention_days", "must be greater than zero")
	}
	if cfg.Memory.MaxTopics <= 0 {
		add("memory.max_topics", "must be greater than zero")
	}
	if cfg.Memory.MaxLinks < 0 {
		add("memory.max_links", "must not be negative")
	}
	if cfg.Memory.MaxTopicChars <= 0 {
		add("memory.max_topic_chars", "must be greater than zero")
	}
	if cfg.Memory.MaxPatchOps <= 0 {
		add("memory.max_patch_ops", "must be greater than zero")
	}
	if cfg.Sandbox.Enabled {
		if len(cfg.Sandbox.Projects) == 0 {
			add("sandbox.projects", "must contain at least one registered project when enabled")
		}
		for name, path := range cfg.Sandbox.Projects {
			if strings.TrimSpace(name) == "" || len(name) > 64 || !projectNamePattern.MatchString(name) {
				add("sandbox.projects", "project names must use 1-64 letters, digits, dots, underscores, or hyphens")
			}
			requirePath(&problems, fmt.Sprintf("sandbox.projects[%q]", name), path)
		}
		if cfg.Sandbox.CommandTimeoutSeconds <= 0 {
			add("sandbox.command_timeout_seconds", "must be greater than zero when enabled")
		}
		if cfg.Sandbox.MaxOutputBytes <= 0 {
			add("sandbox.max_output_bytes", "must be greater than zero when enabled")
		}
	}
	if cfg.Canvases.Enabled {
		if cfg.Canvases.MaxTitleChars <= 0 {
			add("canvases.max_title_chars", "must be greater than zero when enabled")
		}
		if cfg.Canvases.MaxContentChars <= 0 {
			add("canvases.max_content_chars", "must be greater than zero when enabled")
		}
		if cfg.Canvases.MaxContentBytes <= 0 {
			add("canvases.max_content_bytes", "must be greater than zero when enabled")
		}
		if cfg.Canvases.TimeoutSeconds <= 0 {
			add("canvases.timeout_seconds", "must be greater than zero when enabled")
		}
	}

	if len(problems) > 0 {
		return &ValidationError{Fields: problems}
	}
	return nil
}

// Validate checks the receiver without mutating it.
func (cfg Config) Validate() error {
	return Validate(cfg)
}

func requirePath(problems *[]FieldError, field, value string) {
	if strings.TrimSpace(value) == "" {
		*problems = append(*problems, FieldError{Field: field, Problem: "must not be empty"})
		return
	}
	if strings.ContainsRune(value, '\x00') {
		*problems = append(*problems, FieldError{Field: field, Problem: "must not contain a NUL byte"})
	}
}

func validateBaseURL(problems *[]FieldError, value string) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		*problems = append(*problems, FieldError{
			Field:   "model.base_url",
			Problem: "must be an absolute http or https URL",
		})
		return
	}
	if parsed.User != nil {
		*problems = append(*problems, FieldError{
			Field:   "model.base_url",
			Problem: "must not contain credentials; configure secrets through environment variables",
		})
	}
	if parsed.Fragment != "" {
		*problems = append(*problems, FieldError{
			Field:   "model.base_url",
			Problem: "must not contain a URL fragment",
		})
	}
}

func validateHeaders(problems *[]FieldError, headers map[string]string) {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names) // Stable validation output also keeps redacted diagnostics stable.
	for _, name := range names {
		value := headers[name]
		field := fmt.Sprintf("model.headers[%q]", name)
		if !validHeaderName(name) {
			*problems = append(*problems, FieldError{Field: field, Problem: "must be a valid HTTP header name"})
		}
		if strings.ContainsAny(value, "\r\n") {
			*problems = append(*problems, FieldError{Field: field, Problem: "must not contain a newline"})
		}
		if sensitiveHeader(name) {
			*problems = append(*problems, FieldError{Field: field, Problem: "must not contain credentials; model.headers is non-sensitive"})
		}
	}
}

func sensitiveHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key":
		return true
	default:
		return false
	}
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		char := value[i]
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			continue
		}
		switch char {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func validateIDs(problems *[]FieldError, field string, values []string, pattern *regexp.Regexp, expected string) {
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		itemField := fmt.Sprintf("%s[%d]", field, index)
		if !pattern.MatchString(value) {
			*problems = append(*problems, FieldError{Field: itemField, Problem: "must be " + expected})
		}
		if _, exists := seen[value]; exists {
			*problems = append(*problems, FieldError{Field: itemField, Problem: fmt.Sprintf("duplicates %q", value)})
		}
		seen[value] = struct{}{}
	}
}
