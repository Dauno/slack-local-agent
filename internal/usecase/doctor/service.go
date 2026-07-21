package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

const (
	SlackBotTokenKey = "SLACK_BOT_TOKEN"
	SlackAppTokenKey = "SLACK_APP_TOKEN"
)

type SecretResolver interface {
	Resolve(keys ...string) (map[string]string, error)
}

type DatabaseChecker interface {
	CheckDatabase(ctx context.Context, path string) error
}

type RemediableError interface {
	error
	Remediation() string
}

type ActionableError struct {
	Err error
	Fix string
}

func (e *ActionableError) Error() string       { return e.Err.Error() }
func (e *ActionableError) Unwrap() error       { return e.Err }
func (e *ActionableError) Remediation() string { return e.Fix }

type LiveChecker interface {
	CheckSlackBot(ctx context.Context, botToken string) error
	CheckSlackApp(ctx context.Context, botToken, appToken string) error
	CheckSlackContext(ctx context.Context, botToken string) error
	CheckSlackCanvas(ctx context.Context, botToken string) error
	CheckSlackExports(ctx context.Context, botToken string) error
	CheckModel(ctx context.Context, model config.ModelConfig, apiKey string) error
	CheckResolvedModel(ctx context.Context, resolved *agentdef.ResolvedModel, apiKey string) error
}

// CLIProviderCheck is the typed result of one offline agent_cli provider
// check. ShimName is the trusted mapper identity returned by the cli-v1
// describe exchange; it is empty when describe was not performed.
type CLIProviderCheck struct {
	Detail   string
	ShimName string
}

// CLIProviderChecker validates a selected agent_cli provider. The offline
// check covers shim command resolution, project canonicalization, and the
// cli-v1 describe/validate handshake. The live check reports the CLI's saved
// authentication status without making a model call; it selects the
// application-owned command from the trusted mapper identity, never from the
// configurable provider name or shim-supplied argv.
type CLIProviderChecker interface {
	CheckProvider(ctx context.Context, resolved *agentdef.ResolvedModel, cfg config.Config, projectRoot string, describe bool) (CLIProviderCheck, error)
	CheckAuthentication(ctx context.Context, resolved *agentdef.ResolvedModel, shimName string) (string, error)
}

type ACPProviderChecker interface {
	CheckProvider(ctx context.Context, resolved *agentdef.ResolvedModel, projectRoots map[string]string) (string, error)
}

type Dependencies struct {
	ConfigPath string
	LoadConfig func(path string) (config.Config, error)
	Secrets    SecretResolver
	Database   DatabaseChecker
	Live       LiveChecker
	CLI        CLIProviderChecker
	ACP        ACPProviderChecker
}

type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
)

type Result struct {
	Name        string
	Status      Status
	Detail      string
	Remediation string
	Fatal       bool
}

type Report struct {
	Results []Result
}

func (r Report) ExitCode() int {
	code := 0
	for _, result := range r.Results {
		if result.Status != StatusFail {
			continue
		}
		if result.Fatal {
			return 2
		}
		code = 1
	}
	return code
}

func (r Report) Passed() bool { return r.ExitCode() == 0 }

type Service struct {
	deps Dependencies
}

type selectedModel struct {
	agent    string
	resolved *agentdef.ResolvedModel
}

func New(deps Dependencies) (*Service, error) {
	if strings.TrimSpace(deps.ConfigPath) == "" {
		return nil, errors.New("doctor config path is required")
	}
	if deps.LoadConfig == nil {
		deps.LoadConfig = config.Load
	}
	if deps.Secrets == nil {
		return nil, errors.New("doctor secret resolver is required")
	}
	if deps.Database == nil {
		return nil, errors.New("doctor database checker is required")
	}
	return &Service{deps: deps}, nil
}

func (s *Service) Run(ctx context.Context, includeLive bool) Report {
	report := Report{}
	cfg, err := s.deps.LoadConfig(s.deps.ConfigPath)
	if err != nil {
		result := Result{
			Name:        "configuration",
			Status:      StatusFail,
			Detail:      err.Error(),
			Remediation: "Run: local-agent init, then fix .local-agent/config.yaml as reported.",
		}
		var validation *config.ValidationError
		switch {
		case errors.Is(err, os.ErrNotExist):
			result.Detail = "configuration file is missing"
			result.Remediation = "Run: local-agent init"
		case errors.As(err, &validation):
			// Typed validation failures are ordinary health-check failures.
		default:
			result.Fatal = true
		}
		report.Results = append(report.Results, result)
		return report
	}
	report.pass("configuration", "typed configuration is valid")

	projectRoot := filepath.Dir(filepath.Dir(s.deps.ConfigPath))
	paths, pathErr := cfg.ResolvePaths(projectRoot)
	var (
		defs           *agentdef.Definitions
		resolvedModel  *agentdef.ResolvedModel
		selectedModels []selectedModel
		defsLoadFailed bool
	)
	if pathErr != nil {
		report.fail("SQLite", pathErr.Error(), "Fix state.dir and state.db in .local-agent/config.yaml.", false)
	} else {
		var defsErr error
		defs, defsErr = agentdef.Load(paths.StateDir)
		if defsErr != nil {
			defsLoadFailed = true
			report.fail("agent definitions", defsErr.Error(), "Fix .local-agent/agents/*.yaml and .local-agent/providers/*.yaml files.", false)
		} else if defs != nil {
			rootDef, ok := defs.Agents["root_agent"]
			if !ok {
				defsLoadFailed = true
				report.fail("agent definitions", "agent definition root_agent is required", "Add .local-agent/agents/root_agent.yaml.", false)
			} else if resolvedModel, defsErr = defs.ResolveModel(rootDef.Model); defsErr != nil {
				defsLoadFailed = true
				report.fail("agent definitions", defsErr.Error(), "Fix root_agent.model in .local-agent/agents/root_agent.yaml.", false)
			} else {
				selectedModels = append(selectedModels, selectedModel{agent: "root_agent", resolved: resolvedModel})
				for _, agentToolName := range rootDef.AgentTools {
					agentTool, exists := defs.Agents[agentToolName]
					if !exists {
						defsErr = fmt.Errorf("agent tool %q is not defined", agentToolName)
						break
					}
					modelRef := agentTool.Model
					if agentTool.AgentClass == "AcpAgent" {
						modelRef = agentTool.Runtime
					}
					agentToolResolved, resolveErr := defs.ResolveModel(modelRef)
					if resolveErr != nil {
						defsErr = fmt.Errorf("resolve agent tool %q model: %w", agentToolName, resolveErr)
						break
					}
					selectedModels = append(selectedModels, selectedModel{agent: agentToolName, resolved: agentToolResolved})
				}
				if defsErr == nil {
					blueprints := make([]*agentdef.WorkflowBlueprint, 0, len(rootDef.WorkflowTools))
					for _, workflowID := range rootDef.WorkflowTools {
						bp, loadErr := defs.LoadWorkflow(paths.StateDir, workflowID)
						if loadErr != nil {
							defsErr = fmt.Errorf("workflow %q: %w", workflowID, loadErr)
							break
						}
						blueprints = append(blueprints, bp)
						for _, doc := range bp.OrderedDocuments() {
							if doc.AgentClass == agentdef.AgentClassAcp && doc.ACP != nil {
								workflowResolved, resolveErr := defs.ResolveModel(doc.ACP.Runtime)
								if resolveErr != nil {
									defsErr = fmt.Errorf("workflow %q agent %q: resolve runtime %q: %w", workflowID, doc.Name, doc.ACP.Runtime, resolveErr)
									break
								}
								selectedModels = append(selectedModels, selectedModel{agent: "workflow:" + doc.Name, resolved: workflowResolved})
								continue
							}
							if doc.AgentClass != agentdef.AgentClassLLM || doc.LLM == nil {
								continue
							}
							workflowResolved, resolveErr := defs.ResolveModel(doc.LLM.Model)
							if resolveErr != nil {
								defsErr = fmt.Errorf("workflow %q agent %q: resolve model %q: %w", workflowID, doc.Name, doc.LLM.Model, resolveErr)
								break
							}
							selectedModels = append(selectedModels, selectedModel{agent: "workflow:" + doc.Name, resolved: workflowResolved})
						}
						if defsErr != nil {
							break
						}
					}
					if defsErr == nil {
						defsErr = defs.ValidateWorkflowComposition(rootDef, blueprints, cfg.Sandbox.Enabled)
					}
				}
				if defsErr == nil && cfg.Memory.Enabled {
					curator, exists := defs.Agents["memory_curator"]
					if !exists {
						defsErr = errors.New("agent definition memory_curator is required when memory is enabled")
					} else if curatorResolved, resolveErr := defs.ResolveModel(curator.Model); resolveErr != nil {
						defsErr = fmt.Errorf("resolve memory_curator model: %w", resolveErr)
					} else {
						selectedModels = append(selectedModels, selectedModel{agent: "memory_curator", resolved: curatorResolved})
					}
				}
				if defsErr == nil {
					if attachment, exists := defs.Agents["attachment_analyzer"]; exists {
						attachmentResolved, resolveErr := defs.ResolveModel(attachment.Model)
						switch {
						case resolveErr != nil:
							defsErr = fmt.Errorf("resolve attachment_analyzer model: %w", resolveErr)
						case attachmentResolved.IsAgentCLI():
							defsErr = errors.New("attachment_analyzer cannot use an agent_cli provider because it requires the ADK load_artifacts tool")
						default:
							selectedModels = append(selectedModels, selectedModel{agent: "attachment_analyzer", resolved: attachmentResolved})
						}
					}
				}
				if defsErr != nil {
					defsLoadFailed = true
					report.fail("agent definitions", defsErr.Error(), "Fix selected agent model references and provider compatibility.", false)
				} else {
					report.pass("agent definitions", fmt.Sprintf("%d providers, %d agents loaded", len(defs.Providers), len(defs.Agents)))
				}
			}
		}
	}

	modelAPIKeyEnv := cfg.Model.APIKeyEnv
	rootCLIProvider := resolvedModel != nil && resolvedModel.IsAgentCLI()
	if resolvedModel != nil && !rootCLIProvider {
		modelAPIKeyEnv = resolvedModel.APIKeyEnv
	}
	keys := []string{modelAPIKeyEnv, SlackBotTokenKey, SlackAppTokenKey}
	if defs != nil {
		keys = append(keys, defs.RequiredAPIKeyEnvs()...)
	}
	keys = uniqueStrings(keys)
	values, err := s.deps.Secrets.Resolve(keys...)
	if err != nil {
		report.fail("secrets", err.Error(), "Fix .env syntax or process environment values.", false)
		return report
	}
	redactionValues := make([]string, 0, len(keys))
	for _, key := range keys {
		redactionValues = append(redactionValues, values[key])
	}
	redactor := secure.NewRedactor(redactionValues...)

	validSecrets := make(map[string]bool, len(keys))
	checkSecret := func(name, key, expectedPrefix, remediation string) {
		value, exists := values[key]
		if !exists || strings.TrimSpace(value) == "" {
			report.fail(name, fmt.Sprintf("%s is not set", key), remediation, false)
			return
		}
		if expectedPrefix != "" && (len(value) <= len(expectedPrefix) || !strings.HasPrefix(value, expectedPrefix)) {
			report.fail(name, fmt.Sprintf("%s must start with %s", key, expectedPrefix), remediation, false)
			return
		}
		validSecrets[key] = true
		report.pass(name, fmt.Sprintf("%s is configured (%s)", key, secure.Mask(value)))
	}
	checkedModelAPIKeys := make(map[string]bool)
	if rootCLIProvider {
		// agent_cli providers require no model API key.
		report.pass("model API key", "agent CLI provider requires no model API key")
	} else {
		checkSecret("model API key", modelAPIKeyEnv, "", "Set "+modelAPIKeyEnv+" in the process environment or .env.")
		checkedModelAPIKeys[modelAPIKeyEnv] = true
	}
	for _, selected := range selectedModels {
		if selected.resolved.IsAgentCLI() || selected.resolved.IsACP() || checkedModelAPIKeys[selected.resolved.APIKeyEnv] {
			continue
		}
		key := selected.resolved.APIKeyEnv
		checkedModelAPIKeys[key] = true
		checkSecret("model API key ("+selected.agent+")", key, "", "Set "+key+" in the process environment or .env.")
	}
	checkSecret("Slack bot token", SlackBotTokenKey, "xoxb-", "Set a Bot User OAuth Token beginning with xoxb-.")
	checkSecret("Slack app token", SlackAppTokenKey, "xapp-", "Set an app-level Socket Mode token beginning with xapp- and connections:write.")

	if pathErr == nil {
		if err := s.deps.Database.CheckDatabase(ctx, paths.DatabaseFile); err != nil {
			remediation := "Run local-agent init or fix permissions for the configured database path."
			var actionable RemediableError
			if errors.As(err, &actionable) && actionable.Remediation() != "" {
				remediation = actionable.Remediation()
			}
			report.fail("SQLite", redactor.String(err.Error()), remediation, false)
		} else {
			report.pass("SQLite", "database exists, is migrated, and is readable/writable")
		}
	}
	if pathErr == nil {
		var acpModels []selectedModel
		for _, selected := range selectedModels {
			if selected.resolved.IsACP() {
				acpModels = append(acpModels, selected)
			}
		}
		if len(acpModels) > 0 && s.deps.ACP == nil {
			report.fail("ACP provider", "ACP checker is unavailable", "Reinstall local-agent with ACP support.", false)
		}
		for _, selected := range acpModels {
			if s.deps.ACP == nil {
				break
			}
			detail, err := s.deps.ACP.CheckProvider(ctx, selected.resolved, paths.SandboxProjectRoots)
			if err != nil {
				report.fail("ACP provider ("+selected.agent+")", redactor.String(err.Error()), "Verify the ACP command, saved OpenCode authentication, profile options, and registered projects.", false)
				continue
			}
			report.pass("ACP provider ("+selected.agent+")", detail)
		}
		report.pass("OpenCode management operators", fmt.Sprintf("%d configured", len(cfg.OpenCode.Management.AllowedUserIDs)))
	}

	shimNames := make(map[string]string)
	if pathErr == nil {
		var cliModels []selectedModel
		for _, selected := range selectedModels {
			if selected.resolved.IsAgentCLI() {
				cliModels = append(cliModels, selected)
			}
		}
		if len(cliModels) > 0 && s.deps.CLI == nil {
			report.fail("agent CLI provider", "agent CLI checker is unavailable", "Reinstall local-agent with agent CLI support.", false)
		}
		described := make(map[string]bool)
		for _, selected := range cliModels {
			if s.deps.CLI == nil {
				break
			}
			providerName := selected.resolved.Provider.Name
			describe := !described[providerName]
			resultName := "agent CLI provider (" + selected.agent + ")"
			check, err := s.deps.CLI.CheckProvider(ctx, selected.resolved, cfg, projectRoot, describe)
			if err != nil {
				report.fail(resultName, redactor.String(err.Error()),
					"Verify the shim command, the installed CLI version, selected profile, and sandbox.projects registration.", false)
				continue
			}
			if describe {
				described[providerName] = true
				shimNames[providerName] = check.ShimName
			}
			report.pass(resultName, check.Detail)
		}
	}

	if !includeLive {
		return report
	}
	if s.deps.Live == nil {
		report.fail("live checks", "live checker is unavailable", "Reinstall local-agent with live-check support.", false)
		return report
	}
	if value := values[SlackBotTokenKey]; validSecrets[SlackBotTokenKey] {
		liveCtx, cancel := checkTimeout(ctx, cfg.Runtime.SlackAPITimeoutSeconds)
		err := s.deps.Live.CheckSlackBot(liveCtx, value)
		cancel()
		if err != nil {
			report.fail("Slack bot connectivity", redactor.String(err.Error()), "Verify SLACK_BOT_TOKEN and Slack workspace access.", false)
		} else {
			report.pass("Slack bot connectivity", "Slack auth check passed")
		}
	}
	if botToken, appToken := values[SlackBotTokenKey], values[SlackAppTokenKey]; validSecrets[SlackBotTokenKey] && validSecrets[SlackAppTokenKey] {
		liveCtx, cancel := checkTimeout(ctx, cfg.Runtime.SlackAPITimeoutSeconds)
		err := s.deps.Live.CheckSlackApp(liveCtx, botToken, appToken)
		cancel()
		if err != nil {
			report.fail("Slack Socket Mode", redactor.String(err.Error()), "Verify SLACK_APP_TOKEN has connections:write and belongs to this app.", false)
		} else {
			report.pass("Slack Socket Mode", "app-level token can open a Socket Mode connection")
		}
	}
	if cfg.Slack.Context.Enabled && validSecrets[SlackBotTokenKey] {
		liveCtx, cancel := checkTimeout(ctx, cfg.Runtime.SlackAPITimeoutSeconds)
		err := s.deps.Live.CheckSlackContext(liveCtx, values[SlackBotTokenKey])
		cancel()
		if err != nil {
			report.fail("Slack context enrichment", redactor.String(err.Error()), "Reinstall the Slack app with users:read, then verify the bot token.", false)
		} else {
			report.pass("Slack context enrichment", "users:read capability check passed")
		}
	}
	if cfg.Canvases.Enabled && validSecrets[SlackBotTokenKey] {
		liveCtx, cancel := checkTimeout(ctx, cfg.Canvases.TimeoutSeconds)
		err := s.deps.Live.CheckSlackCanvas(liveCtx, values[SlackBotTokenKey])
		cancel()
		if err != nil {
			report.fail("Slack Canvas", redactor.String(err.Error()), "Regenerate the manifest, reinstall the Slack app with canvases:write, and verify Canvas availability for the workspace.", false)
		} else {
			report.pass("Slack Canvas", "canvases:write scope check passed")
		}
	}
	if cfg.Exports.Enabled && validSecrets[SlackBotTokenKey] {
		liveCtx, cancel := checkTimeout(ctx, cfg.Exports.TimeoutSeconds)
		err := s.deps.Live.CheckSlackExports(liveCtx, values[SlackBotTokenKey])
		cancel()
		if err != nil {
			report.fail("Slack generated files", redactor.String(err.Error()), "Regenerate the manifest, reinstall the Slack app with files:write, and verify the bot token.", false)
		} else {
			report.pass("Slack generated files", "files:write scope check passed")
		}
	}
	if s.deps.CLI != nil {
		authChecked := make(map[string]bool)
		for _, selected := range selectedModels {
			if !selected.resolved.IsAgentCLI() {
				continue
			}
			providerName := selected.resolved.Provider.Name
			shimName := shimNames[providerName]
			// A missing identity means describe failed and already produced the
			// actionable provider result. Do not add a misleading auth failure.
			if strings.TrimSpace(shimName) == "" || authChecked[providerName] {
				continue
			}
			authChecked[providerName] = true
			liveCtx, cancel := checkTimeout(ctx, cfg.Runtime.ModelTimeoutSeconds)
			detail, err := s.deps.CLI.CheckAuthentication(liveCtx, selected.resolved, shimName)
			cancel()
			if err != nil {
				report.fail("agent CLI authentication ("+providerName+")", redactor.String(err.Error()),
					"Log in to the agent CLI (for example: opencode auth login or codex login) so it can reuse its saved credentials.", false)
			} else {
				report.pass("agent CLI authentication ("+providerName+")", detail)
			}
		}
	}
	if !rootCLIProvider {
		if apiKey := values[modelAPIKeyEnv]; validSecrets[modelAPIKeyEnv] && !defsLoadFailed {
			liveCtx, cancel := checkTimeout(ctx, cfg.Runtime.ModelTimeoutSeconds)
			var err error
			if resolvedModel != nil {
				err = s.deps.Live.CheckResolvedModel(liveCtx, resolvedModel, apiKey)
			} else if defs == nil {
				err = s.deps.Live.CheckModel(liveCtx, cfg.Model, apiKey)
			}
			cancel()
			if err != nil {
				report.fail("model endpoint", redactor.String(err.Error()), "Verify model.base_url, model.name, request options, and the configured API key.", false)
			} else {
				report.pass("model endpoint", "minimal non-streaming Chat Completions request passed")
			}
		}
	}
	return report
}

func checkTimeout(ctx context.Context, seconds int) (context.Context, context.CancelFunc) {
	if seconds <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func (r *Report) pass(name, detail string) {
	r.Results = append(r.Results, Result{Name: name, Status: StatusPass, Detail: detail})
}

func (r *Report) fail(name, detail, remediation string, fatal bool) {
	r.Results = append(r.Results, Result{
		Name: name, Status: StatusFail, Detail: detail, Remediation: remediation, Fatal: fatal,
	})
}
