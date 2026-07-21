package config_test

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/config"
)

func TestDefaultMatchesPRD(t *testing.T) {
	t.Parallel()

	want := config.Config{
		Agent: config.AgentConfig{Name: "Dev Agent"},
		State: config.StateConfig{
			Dir: ".local-agent",
			DB:  ".local-agent/local-agent.db",
		},
		Context: config.ContextConfig{
			MaxMessages:                   30,
			MaxChars:                      20_000,
			RetainMessagesPerConversation: 100,
		},
		Runtime: config.RuntimeConfig{
			LogLevel:                "info",
			ModelTimeoutSeconds:     0,
			SlackAPITimeoutSeconds:  30,
			MaxConcurrentModelCalls: 4,
			BusyMessage:             "El bot está ocupado procesando otras solicitudes. Intenta de nuevo en unos minutos.",
			ModelErrorMessage:       "No pude completar la respuesta por un error del modelo. Intenta de nuevo.",
		},
		Model: config.ModelConfig{
			Name:            "deepseek-v4-flash",
			BaseURL:         "https://api.deepseek.com",
			APIKeyEnv:       "DEEPSEEK_API_KEY",
			ReasoningEffort: "high",
			ExtraBody: map[string]any{
				"thinking": map[string]any{"type": "enabled"},
			},
		},
		Slack: config.SlackConfig{
			AppName:             "Local Agent",
			BotDisplayName:      "Dev Agent",
			UnauthorizedMessage: "No tienes permiso para usar este bot. Pide acceso a quien administra local-agent.",
			AllowedUserIDs:      []string{},
			AllowedTeamIDs:      []string{},
			AllowedChannelIDs:   []string{},
			PartLabels:          true,
			Context: config.SlackContextConfig{
				Enabled:                     false,
				MaxChars:                    1500,
				TimeoutSeconds:              5,
				ProfileCacheTTLMinutes:      60,
				ConversationCacheTTLMinutes: 15,
			},
			Files: config.SlackFilesConfig{MaxBytesPerFile: 5 * 1024 * 1024, MaxProcessedChars: 20_000},
		},
		Memory: config.MemoryConfig{
			Enabled:               false,
			Directory:             "",
			MaxTopicsRecall:       3,
			MaxCharsRecall:        2000,
			RecallTimeoutSeconds:  2,
			CuratorTimeoutSeconds: 30,
			CuratorMaxRetries:     3,
			WorkerIntervalSeconds: 60,
			RetentionDays:         90,
			MaxTopics:             100,
			MaxLinks:              50,
			MaxTopicChars:         10000,
			MaxPatchOps:           10,
		},
		Sandbox:  config.SandboxConfig{Projects: map[string]string{}, CommandTimeoutSeconds: 30, MaxOutputBytes: 65536},
		Canvases: config.CanvasesConfig{MaxTitleChars: 150, MaxContentChars: 50000, MaxContentBytes: 5 * 1024 * 1024, TimeoutSeconds: 30},
		Exports:  config.ExportsConfig{MaxFilenameChars: 128, MaxContentBytes: 1024 * 1024, TimeoutSeconds: 30},
		OpenCode: config.OpenCodeConfig{Management: config.OpenCodeManagementConfig{AllowedUserIDs: []string{}}},
	}

	got := config.Default()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Default() mismatch\n got: %#v\nwant: %#v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

func TestDefaultDoesNotShareExtraBody(t *testing.T) {
	t.Parallel()

	first := config.Default()
	second := config.Default()
	first.Model.ExtraBody["thinking"].(map[string]any)["type"] = "disabled"

	got := second.Model.ExtraBody["thinking"].(map[string]any)["type"]
	if got != "enabled" {
		t.Fatalf("defaults share mutable extra_body state: got %v", got)
	}
}

func TestMarshalDefaultYAML(t *testing.T) {
	t.Parallel()

	got, err := config.Marshal(config.Default())
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	want := `agent:
  name: Dev Agent
state:
  dir: .local-agent
  db: .local-agent/local-agent.db
context:
  max_messages: 30
  max_chars: 20000
  retain_messages_per_conversation: 100
runtime:
  log_level: info
  model_timeout_seconds: 0
  slack_api_timeout_seconds: 30
  max_concurrent_model_calls: 4
  busy_message: El bot está ocupado procesando otras solicitudes. Intenta de nuevo en unos minutos.
  model_error_message: No pude completar la respuesta por un error del modelo. Intenta de nuevo.
model:
  name: deepseek-v4-flash
  base_url: https://api.deepseek.com
  api_key_env: DEEPSEEK_API_KEY
  reasoning_effort: high
  extra_body:
    thinking:
      type: enabled
slack:
  app_name: Local Agent
  bot_display_name: Dev Agent
  unauthorized_message: No tienes permiso para usar este bot. Pide acceso a quien administra local-agent.
  allow_all_users: false
  allowed_user_ids: []
  allowed_team_ids: []
  allowed_channel_ids: []
  part_labels: true
  context:
    enabled: false
    max_chars: 1500
    timeout_seconds: 5
    profile_cache_ttl_minutes: 60
    conversation_cache_ttl_minutes: 15
  files:
    max_bytes_per_file: 5242880
    max_processed_chars: 20000
memory:
  enabled: false
  directory: ""
  max_topics_recall: 3
  max_chars_recall: 2000
  recall_timeout_seconds: 2
  curator_timeout_seconds: 30
  curator_max_retries: 3
  worker_interval_seconds: 60
  retention_days: 90
  max_topics: 100
  max_links: 50
  max_topic_chars: 10000
  max_patch_ops: 10
sandbox:
  enabled: false
  projects: {}
  command_timeout_seconds: 30
  max_output_bytes: 65536
canvases:
  enabled: false
  max_title_chars: 150
  max_content_chars: 50000
  max_content_bytes: 5242880
  timeout_seconds: 30
exports:
  enabled: false
  max_filename_chars: 128
  max_content_bytes: 1048576
  timeout_seconds: 30
opencode:
  management:
    allowed_user_ids: []
`

	if string(got) != want {
		t.Fatalf("default YAML mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestParseAppliesOnlyMissingDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse([]byte(`agent:
  name: Release Agent
model:
  headers:
    X-Client: local-agent
  extra_body: {}
slack:
  allow_all_users: true
  allowed_user_ids: null
  files:
    max_bytes_per_file: 1048576
    max_processed_chars: 4096
`))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if cfg.Agent.Name != "Release Agent" {
		t.Fatalf("agent.name = %q", cfg.Agent.Name)
	}
	if cfg.Model.Name != "deepseek-v4-flash" {
		t.Fatalf("missing model.name did not receive default: %q", cfg.Model.Name)
	}
	if len(cfg.Model.ExtraBody) != 0 {
		t.Fatalf("explicit empty extra_body was overwritten: %#v", cfg.Model.ExtraBody)
	}
	if cfg.Model.Headers["X-Client"] != "local-agent" {
		t.Fatalf("model headers not decoded: %#v", cfg.Model.Headers)
	}
	if cfg.Slack.AllowedUserIDs == nil || len(cfg.Slack.AllowedUserIDs) != 0 {
		t.Fatalf("allowed_user_ids should normalize to an empty slice: %#v", cfg.Slack.AllowedUserIDs)
	}
	if cfg.Slack.Files.MaxBytesPerFile != 1048576 || cfg.Slack.Files.MaxProcessedChars != 4096 {
		t.Fatalf("slack.files overrides not decoded: %#v", cfg.Slack.Files)
	}
}

func TestParseEmptyOrCommentOnlyUsesDefaults(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "   \n", "# intentionally using defaults\n"} {
		cfg, err := config.Parse([]byte(input))
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", input, err)
		}
		want := config.Default()
		if !reflect.DeepEqual(cfg.Agent, want.Agent) ||
			!reflect.DeepEqual(cfg.State, want.State) ||
			!reflect.DeepEqual(cfg.Context, want.Context) ||
			!reflect.DeepEqual(cfg.Runtime, want.Runtime) ||
			!reflect.DeepEqual(cfg.Model, want.Model) ||
			!reflect.DeepEqual(cfg.Slack, want.Slack) {
			t.Fatalf("Parse(%q) did not produce defaults: %#v", input, cfg)
		}
	}
}

func TestParseAndMarshalPreserveUnknownFieldsAndComments(t *testing.T) {
	t.Parallel()

	input := []byte(`# operator note
agent:
  name: Old Name # keep this comment
  tone: terse
plugin_extension:
  enabled: true
model:
  headers:
    X-Trace: enabled
`)
	cfg, err := config.Parse(input)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	cfg.Agent.Name = "New Name"
	cfg.Model.Headers = nil

	output, err := config.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	text := string(output)
	for _, fragment := range []string{
		"# operator note",
		"name: New Name # keep this comment",
		"tone: terse",
		"plugin_extension:",
		"enabled: true",
	} {
		if !strings.Contains(text, fragment) {
			t.Errorf("output lost %q:\n%s", fragment, text)
		}
	}
	if strings.Contains(text, "headers:") || strings.Contains(text, "X-Trace") {
		t.Fatalf("cleared known headers were retained:\n%s", text)
	}
}

func TestParseRejectsMalformedDocuments(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"sequence root":      "- invalid\n",
		"multiple documents": "agent: {}\n---\nagent: {}\n",
		"duplicate key":      "agent:\n  name: one\n  name: two\n",
		"wrong typed value":  "context:\n  max_messages: many\n",
	}
	for name, input := range tests {
		name, input := name, input
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := config.Parse([]byte(input)); err == nil {
				t.Fatal("Parse() unexpectedly succeeded")
			}
		})
	}
}

func TestValidationReportsTypedFieldErrors(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Agent.Name = " "
	cfg.State.DB = ""
	cfg.Context.MaxMessages = 0
	cfg.Runtime.LogLevel = "verbose"
	cfg.Runtime.ModelTimeoutSeconds = -1
	cfg.Runtime.SlackAPITimeoutSeconds = -1
	cfg.Runtime.MaxConcurrentModelCalls = 0
	cfg.Model.BaseURL = "ftp://example.com"
	cfg.Model.APIKeyEnv = "NOT-AN-ENV"
	cfg.Model.ReasoningEffort = "maximum"
	cfg.Model.Headers = map[string]string{"Bad Header": "line\nbreak"}
	cfg.Model.ExtraBody = map[string]any{"bad": math.NaN(), "stream": true}
	cfg.Slack.AllowedUserIDs = []string{"not-a-user"}
	cfg.Slack.AllowedTeamIDs = []string{"U12345678"}
	cfg.Slack.AllowedChannelIDs = []string{"D12345678"}
	cfg.Slack.Files.MaxBytesPerFile = 5*1024*1024 + 1
	cfg.Slack.Files.MaxProcessedChars = 20_001

	err := cfg.Validate()
	var validation *config.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("Validate() error type = %T, want *config.ValidationError: %v", err, err)
	}
	for _, field := range []string{
		"agent.name",
		"state.db",
		"context.max_messages",
		"runtime.log_level",
		"runtime.model_timeout_seconds",
		"runtime.slack_api_timeout_seconds",
		"runtime.max_concurrent_model_calls",
		"model.base_url",
		"model.api_key_env",
		"model.reasoning_effort",
		`model.headers["Bad Header"]`,
		"model.extra_body",
		"model.extra_body.stream",
		"slack.allowed_user_ids[0]",
		"slack.allowed_team_ids[0]",
		"slack.allowed_channel_ids[0]",
		"slack.files.max_bytes_per_file",
		"slack.files.max_processed_chars",
	} {
		if !validation.Has(field) {
			t.Errorf("validation did not report %s: %v", field, err)
		}
	}
}

func TestValidateAcceptsConfiguredAccessListsAndHeaders(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Model.Headers = map[string]string{"X-Client-Version": "1", "X_Custom": "ok"}
	cfg.Slack.AllowedUserIDs = []string{"U12345678", "W12345678"}
	cfg.Slack.AllowedTeamIDs = []string{"T12345678"}
	cfg.Slack.AllowedChannelIDs = []string{"C12345678", "G12345678"}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
}

func TestValidateRejectsContextTimeoutAboveSlackAPITimeout(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Slack.Context.Enabled = true
	cfg.Runtime.SlackAPITimeoutSeconds = 1
	cfg.Slack.Context.TimeoutSeconds = 2
	err := cfg.Validate()
	var validation *config.ValidationError
	if !errors.As(err, &validation) || !validation.Has("slack.context.timeout_seconds") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsSensitiveModelHeaders(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Model.Headers = map[string]string{"Authorization": "Bearer secret"}
	err := cfg.Validate()
	var validation *config.ValidationError
	if !errors.As(err, &validation) || !validation.Has(`model.headers["Authorization"]`) {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsInvalidMemoryLimits(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Memory.RecallTimeoutSeconds = 0
	cfg.Memory.MaxPatchOps = 0
	err := cfg.Validate()
	var validation *config.ValidationError
	if !errors.As(err, &validation) || !validation.Has("memory.recall_timeout_seconds") || !validation.Has("memory.max_patch_ops") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestResolvePaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := config.Default()
	cfg.State.Dir = "var/state"
	cfg.State.DB = filepath.Join(root, "outside", "agent.db")

	paths, err := cfg.ResolvePaths(root)
	if err != nil {
		t.Fatalf("ResolvePaths() error: %v", err)
	}
	want := config.Paths{
		ProjectRoot:         root,
		StateDir:            filepath.Join(root, "var", "state"),
		DatabaseFile:        filepath.Join(root, "outside", "agent.db"),
		ConfigFile:          filepath.Join(root, ".local-agent", "config.yaml"),
		ManifestFile:        filepath.Join(root, ".local-agent", "app-manifest.local.yaml"),
		EnvExampleFile:      filepath.Join(root, ".local-agent", "local.env.example"),
		EnvFile:             filepath.Join(root, ".env"),
		MemoryDir:           filepath.Join(root, "var", "state", "memory"),
		OpenCodeWorktreeDir: filepath.Join(root, "var", "state", "worktrees"),
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("ResolvePaths()\n got: %#v\nwant: %#v", paths, want)
	}

	configPath, err := config.ConfigPath(root)
	if err != nil {
		t.Fatalf("ConfigPath() error: %v", err)
	}
	if configPath != want.ConfigFile {
		t.Fatalf("ConfigPath() = %q, want %q", configPath, want.ConfigFile)
	}
}

func TestSaveAndLoadPreserveFileModeAndExtensions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	input := []byte("agent:\n  name: Existing\nextension:\n  value: retained\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, input, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	cfg.Agent.Name = "Updated"
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("saved mode = %04o, want 0600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "name: Updated") || !strings.Contains(string(data), "value: retained") {
		t.Fatalf("saved data lost changes or extensions:\n%s", data)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("re-Load() error: %v", err)
	}
	if reloaded.Agent.Name != "Updated" {
		t.Fatalf("reloaded agent.name = %q", reloaded.Agent.Name)
	}
}

func TestSaveCreatesParentAndUsesNonSensitiveMode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing", "config.yaml")
	if err := config.Save(path, config.Default()); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("new config mode = %04o, want 0644", got)
	}
}

func TestParseAppliesSandboxConfig(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse([]byte(`sandbox:
  enabled: true
  projects:
    workspace: .
    api: ../api
  command_timeout_seconds: 60
  max_output_bytes: 32768
`))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if !cfg.Sandbox.Enabled {
		t.Fatal("sandbox.enabled should be true")
	}
	if len(cfg.Sandbox.Projects) != 2 {
		t.Fatalf("sandbox.projects count = %d, want 2", len(cfg.Sandbox.Projects))
	}
	if cfg.Sandbox.Projects["workspace"] != "." {
		t.Fatalf("sandbox.projects[workspace] = %q", cfg.Sandbox.Projects["workspace"])
	}
	if cfg.Sandbox.CommandTimeoutSeconds != 60 {
		t.Fatalf("sandbox.command_timeout_seconds = %d", cfg.Sandbox.CommandTimeoutSeconds)
	}
	if cfg.Sandbox.MaxOutputBytes != 32768 {
		t.Fatalf("sandbox.max_output_bytes = %d", cfg.Sandbox.MaxOutputBytes)
	}
}

func TestParseAppliesOpenCodeManagementAllowlist(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(`opencode:
  management:
    allowed_user_ids: [U12345678]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.OpenCode.Management.AllowedUserIDs; len(got) != 1 || got[0] != "U12345678" {
		t.Fatalf("allowed user IDs = %v", got)
	}
}

func TestParseSandboxDisabledByDefault(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse([]byte(`agent:
  name: minimal
`))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if cfg.Sandbox.Enabled {
		t.Fatal("sandbox should be disabled by default")
	}
	if len(cfg.Sandbox.Projects) != 0 {
		t.Fatalf("sandbox projects should be empty by default")
	}
}

func TestParseAppliesCanvasConfig(t *testing.T) {
	cfg, err := config.Parse([]byte(`canvases:
  enabled: true
  max_title_chars: 100
  max_content_chars: 2000
  max_content_bytes: 4096
  timeout_seconds: 12
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Canvases.Enabled || cfg.Canvases.MaxTitleChars != 100 || cfg.Canvases.MaxContentChars != 2000 || cfg.Canvases.MaxContentBytes != 4096 || cfg.Canvases.TimeoutSeconds != 12 {
		t.Fatalf("parsed canvases config = %#v", cfg.Canvases)
	}
}

func TestParseAndValidateExportConfig(t *testing.T) {
	cfg, err := config.Parse([]byte(`exports:
  enabled: true
  max_filename_chars: 80
  max_content_bytes: 65536
  timeout_seconds: 12
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Exports.Enabled || cfg.Exports.MaxFilenameChars != 80 || cfg.Exports.MaxContentBytes != 65536 || cfg.Exports.TimeoutSeconds != 12 {
		t.Fatalf("parsed exports config = %#v", cfg.Exports)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	cfg.Exports.MaxFilenameChars = 129
	cfg.Exports.MaxContentBytes = 1024*1024 + 1
	err = cfg.Validate()
	var validation *config.ValidationError
	if !errors.As(err, &validation) || !validation.Has("exports.max_filename_chars") || !validation.Has("exports.max_content_bytes") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsEnabledSandboxWithoutProjects(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Sandbox.Enabled = true
	err := cfg.Validate()
	var validation *config.ValidationError
	if !errors.As(err, &validation) || !validation.Has("sandbox.projects") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestResolvePathsResolvesSandboxProjects(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := config.Default()
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.Projects = map[string]string{
		"workspace": ".",
		"api":       "../api",
		"frontend":  "/absolute/frontend",
	}

	paths, err := cfg.ResolvePaths(root)
	if err != nil {
		t.Fatalf("ResolvePaths() error: %v", err)
	}
	if paths.SandboxProjectRoots["workspace"] != root {
		t.Fatalf("workspace = %q, want %q", paths.SandboxProjectRoots["workspace"], root)
	}
	wantAPI := filepath.Join(filepath.Dir(root), "api")
	if paths.SandboxProjectRoots["api"] != wantAPI {
		t.Fatalf("api = %q, want %q", paths.SandboxProjectRoots["api"], wantAPI)
	}
	wantFrontend := "/absolute/frontend"
	if paths.SandboxProjectRoots["frontend"] != wantFrontend {
		t.Fatalf("frontend = %q, want %q", paths.SandboxProjectRoots["frontend"], wantFrontend)
	}
}

func TestPathResolvesEmptySandboxToNil(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	paths, err := config.Default().ResolvePaths(root)
	if err != nil {
		t.Fatalf("ResolvePaths() error: %v", err)
	}
	if paths.SandboxProjectRoots != nil {
		t.Fatalf("sandbox project roots should be nil for empty projects: %#v", paths.SandboxProjectRoots)
	}
}

func TestResolvePathsUsesCanonicalProjectRootForSandboxProjects(t *testing.T) {
	parent := t.TempDir()
	physicalRoot := filepath.Join(parent, "physical", "workspace")
	if err := os.MkdirAll(physicalRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(parent, "workspace-link")
	if err := os.Symlink(physicalRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.Projects = map[string]string{"workspace": ".", "api": "../api"}
	paths, err := cfg.ResolvePaths(linkedRoot)
	if err != nil {
		t.Fatalf("ResolvePaths() error: %v", err)
	}
	if paths.ProjectRoot != physicalRoot {
		t.Fatalf("project root = %q, want %q", paths.ProjectRoot, physicalRoot)
	}
	if paths.SandboxProjectRoots["workspace"] != physicalRoot {
		t.Fatalf("workspace = %q, want %q", paths.SandboxProjectRoots["workspace"], physicalRoot)
	}
	wantAPI := filepath.Join(filepath.Dir(physicalRoot), "api")
	if paths.SandboxProjectRoots["api"] != wantAPI {
		t.Fatalf("api = %q, want %q", paths.SandboxProjectRoots["api"], wantAPI)
	}
}

func TestParsePartLabelsDefaultsToTrue(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(`agent:
  name: test
`))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if !cfg.Slack.PartLabels {
		t.Fatal("part_labels should default to true")
	}
}

func TestParsePartLabelsExplicitFalse(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(`slack:
  part_labels: false
`))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if cfg.Slack.PartLabels {
		t.Fatal("part_labels should be false when explicitly set")
	}
}

func TestParsePartLabelsExplicitTrue(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(`slack:
  part_labels: true
`))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if !cfg.Slack.PartLabels {
		t.Fatal("part_labels should be true when explicitly set")
	}
}
