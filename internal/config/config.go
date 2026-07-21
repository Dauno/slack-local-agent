// Package config owns the non-sensitive, typed project configuration.
package config

const (
	DefaultProjectStateDir = ".local-agent"
	DefaultDatabaseFile    = ".local-agent/local-agent.db"
	DefaultConfigFile      = ".local-agent/config.yaml"
	DefaultManifestFile    = ".local-agent/app-manifest.local.yaml"
	DefaultEnvExampleFile  = ".local-agent/local.env.example"
	DefaultEnvFile         = ".env"
)

const (
	DefaultBusyMessage         = "El bot está ocupado procesando otras solicitudes. Intenta de nuevo en unos minutos."
	DefaultModelErrorMessage   = "No pude completar la respuesta por un error del modelo. Intenta de nuevo."
	DefaultUnauthorizedMessage = "No tienes permiso para usar este bot. Pide acceso a quien administra local-agent."
)

// Config is the complete non-sensitive configuration stored in config.yaml.
// Secrets are resolved separately through Model.APIKeyEnv and Slack's fixed
// environment variable names.
type Config struct {
	Agent    AgentConfig    `yaml:"agent"`
	State    StateConfig    `yaml:"state"`
	Context  ContextConfig  `yaml:"context"`
	Runtime  RuntimeConfig  `yaml:"runtime"`
	Model    ModelConfig    `yaml:"model"`
	Slack    SlackConfig    `yaml:"slack"`
	Memory   MemoryConfig   `yaml:"memory"`
	Sandbox  SandboxConfig  `yaml:"sandbox"`
	Canvases CanvasesConfig `yaml:"canvases"`
	OpenCode OpenCodeConfig `yaml:"opencode"`

	document *sourceDocument
}

type OpenCodeConfig struct {
	Management OpenCodeManagementConfig `yaml:"management"`
}

type OpenCodeManagementConfig struct {
	AllowedUserIDs []string `yaml:"allowed_user_ids"`
}

type AgentConfig struct {
	Name string `yaml:"name"`
}

type StateConfig struct {
	Dir string `yaml:"dir"`
	DB  string `yaml:"db"`
}

type ContextConfig struct {
	MaxMessages                   int `yaml:"max_messages"`
	MaxChars                      int `yaml:"max_chars"`
	RetainMessagesPerConversation int `yaml:"retain_messages_per_conversation"`
}

type RuntimeConfig struct {
	LogLevel                string `yaml:"log_level"`
	ModelTimeoutSeconds     int    `yaml:"model_timeout_seconds"`
	SlackAPITimeoutSeconds  int    `yaml:"slack_api_timeout_seconds"`
	MaxConcurrentModelCalls int    `yaml:"max_concurrent_model_calls"`
	BusyMessage             string `yaml:"busy_message"`
	ModelErrorMessage       string `yaml:"model_error_message"`
}

type ModelConfig struct {
	Name            string            `yaml:"name"`
	BaseURL         string            `yaml:"base_url"`
	APIKeyEnv       string            `yaml:"api_key_env"`
	Headers         map[string]string `yaml:"headers,omitempty"`
	ReasoningEffort string            `yaml:"reasoning_effort"`
	ExtraBody       map[string]any    `yaml:"extra_body,omitempty"`
}

type SlackConfig struct {
	AppName             string             `yaml:"app_name"`
	BotDisplayName      string             `yaml:"bot_display_name"`
	UnauthorizedMessage string             `yaml:"unauthorized_message"`
	AllowAllUsers       bool               `yaml:"allow_all_users"`
	AllowedUserIDs      []string           `yaml:"allowed_user_ids"`
	AllowedTeamIDs      []string           `yaml:"allowed_team_ids"`
	AllowedChannelIDs   []string           `yaml:"allowed_channel_ids"`
	PartLabels          bool               `yaml:"part_labels"`
	Context             SlackContextConfig `yaml:"context"`
	Files               SlackFilesConfig   `yaml:"files"`
}

type SlackFilesConfig struct {
	MaxBytesPerFile   int `yaml:"max_bytes_per_file"`
	MaxProcessedChars int `yaml:"max_processed_chars"`
}

type SlackContextConfig struct {
	Enabled                     bool `yaml:"enabled"`
	MaxChars                    int  `yaml:"max_chars"`
	TimeoutSeconds              int  `yaml:"timeout_seconds"`
	ProfileCacheTTLMinutes      int  `yaml:"profile_cache_ttl_minutes"`
	ConversationCacheTTLMinutes int  `yaml:"conversation_cache_ttl_minutes"`
}

type MemoryConfig struct {
	Enabled               bool   `yaml:"enabled"`
	Directory             string `yaml:"directory"`
	MaxTopicsRecall       int    `yaml:"max_topics_recall"`
	MaxCharsRecall        int    `yaml:"max_chars_recall"`
	RecallTimeoutSeconds  int    `yaml:"recall_timeout_seconds"`
	CuratorTimeoutSeconds int    `yaml:"curator_timeout_seconds"`
	CuratorMaxRetries     int    `yaml:"curator_max_retries"`
	WorkerIntervalSeconds int    `yaml:"worker_interval_seconds"`
	RetentionDays         int    `yaml:"retention_days"`
	MaxTopics             int    `yaml:"max_topics"`
	MaxLinks              int    `yaml:"max_links"`
	MaxTopicChars         int    `yaml:"max_topic_chars"`
	MaxPatchOps           int    `yaml:"max_patch_ops"`
}

type SandboxConfig struct {
	Enabled               bool              `yaml:"enabled"`
	Projects              map[string]string `yaml:"projects"`
	CommandTimeoutSeconds int               `yaml:"command_timeout_seconds"`
	MaxOutputBytes        int               `yaml:"max_output_bytes"`
}

type CanvasesConfig struct {
	Enabled           bool `yaml:"enabled"`
	MaxTitleChars     int  `yaml:"max_title_chars"`
	MaxContentChars   int  `yaml:"max_content_chars"`
	MaxContentBytes   int  `yaml:"max_content_bytes"`
	TimeoutSeconds    int  `yaml:"timeout_seconds"`
}

// Default returns a new Config populated with the PRD defaults.
func Default() Config {
	return Config{
		Agent: AgentConfig{
			Name: "Dev Agent",
		},
		State: StateConfig{
			Dir: DefaultProjectStateDir,
			DB:  DefaultDatabaseFile,
		},
		Context: ContextConfig{
			MaxMessages:                   30,
			MaxChars:                      20_000,
			RetainMessagesPerConversation: 100,
		},
		Runtime: RuntimeConfig{
			LogLevel:                "info",
			ModelTimeoutSeconds:     0,
			SlackAPITimeoutSeconds:  30,
			MaxConcurrentModelCalls: 4,
			BusyMessage:             DefaultBusyMessage,
			ModelErrorMessage:       DefaultModelErrorMessage,
		},
		Model: ModelConfig{
			Name:            "deepseek-v4-flash",
			BaseURL:         "https://api.deepseek.com",
			APIKeyEnv:       "DEEPSEEK_API_KEY",
			ReasoningEffort: "high",
			ExtraBody: map[string]any{
				"thinking": map[string]any{
					"type": "enabled",
				},
			},
		},
		Slack: SlackConfig{
			AppName:             "Local Agent",
			BotDisplayName:      "Dev Agent",
			UnauthorizedMessage: DefaultUnauthorizedMessage,
			AllowAllUsers:       false,
			AllowedUserIDs:      []string{},
			AllowedTeamIDs:      []string{},
			AllowedChannelIDs:   []string{},
			PartLabels:          true,
			Context: SlackContextConfig{
				Enabled:                     false,
				MaxChars:                    1500,
				TimeoutSeconds:              5,
				ProfileCacheTTLMinutes:      60,
				ConversationCacheTTLMinutes: 15,
			},
			Files: SlackFilesConfig{
				MaxBytesPerFile:   5 * 1024 * 1024,
				MaxProcessedChars: 20_000,
			},
		},
		Memory: MemoryConfig{
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
		Sandbox:  SandboxConfig{Projects: map[string]string{}, CommandTimeoutSeconds: 30, MaxOutputBytes: 64 * 1024},
		Canvases: CanvasesConfig{MaxTitleChars: 150, MaxContentChars: 50000, MaxContentBytes: 5 * 1024 * 1024, TimeoutSeconds: 30},
		OpenCode: OpenCodeConfig{Management: OpenCodeManagementConfig{AllowedUserIDs: []string{}}},
	}
}
