package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/acpclient"
	"github.com/Dauno/slack-local-agent/internal/adapter/openaillm"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/doctor"
)

type liveChecker struct{}

func (liveChecker) CheckSlackBot(ctx context.Context, botToken string) error {
	response, err := slackapi.New(botToken).AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("Slack auth.test failed: %w", err)
	}
	if response == nil || response.UserID == "" {
		return errors.New("Slack auth.test returned no bot user ID")
	}
	return nil
}

func (liveChecker) CheckSlackApp(ctx context.Context, botToken, appToken string) error {
	api := slackapi.New(botToken, slackapi.OptionAppLevelToken(appToken))
	_, websocketURL, err := socketmode.New(api).OpenContext(ctx)
	if err != nil {
		return fmt.Errorf("Slack apps.connections.open failed: %w", err)
	}
	if strings.TrimSpace(websocketURL) == "" {
		return errors.New("Slack apps.connections.open returned no WebSocket URL")
	}
	return nil
}

func (liveChecker) CheckSlackContext(ctx context.Context, botToken string) error {
	api := slackapi.New(botToken)
	auth, err := api.AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("Slack auth.test for context check failed: %w", err)
	}
	if auth == nil || auth.UserID == "" {
		return errors.New("Slack auth.test for context check returned no bot user ID")
	}
	if _, err := api.GetUserInfoContext(ctx, auth.UserID); err != nil {
		return fmt.Errorf("Slack users.info failed: %w", err)
	}
	return nil
}

func (liveChecker) CheckSlackCanvas(ctx context.Context, botToken string) error {
	return checkSlackBotScope(ctx, botToken, "canvases:write", "Canvas")
}

func (liveChecker) CheckSlackExports(ctx context.Context, botToken string) error {
	return checkSlackBotScope(ctx, botToken, "files:write", "generated file")
}

func checkSlackBotScope(ctx context.Context, botToken, requiredScope, subject string) error {
	var grantedScopes string
	api := slackapi.New(botToken, slackapi.OptionOnResponseHeaders(func(path string, headers http.Header) {
		if path == "auth.test" {
			grantedScopes = headers.Get("X-OAuth-Scopes")
		}
	}))
	if _, err := api.AuthTestContext(ctx); err != nil {
		return fmt.Errorf("Slack auth.test for %s check failed: %w", subject, err)
	}
	if hasSlackScope(grantedScopes, requiredScope) {
		return nil
	}
	return fmt.Errorf("Slack bot token is missing %s", requiredScope)
}

func hasSlackScope(grantedScopes, required string) bool {
	for _, scope := range strings.Split(grantedScopes, ",") {
		if strings.TrimSpace(scope) == required {
			return true
		}
	}
	return false
}

func (liveChecker) CheckModel(ctx context.Context, cfg config.ModelConfig, apiKey string) error {
	llm, err := newModel(cfg, apiKey)
	if err != nil {
		return err
	}
	request := &model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("Reply with OK.", genai.RoleUser)},
	}
	for response, generateErr := range llm.GenerateContent(ctx, request, false) {
		if generateErr != nil {
			return generateErr
		}
		if response == nil || response.Content == nil {
			return errors.New("model endpoint returned no assistant content")
		}
		return nil
	}
	return errors.New("model endpoint returned no response")
}

func (liveChecker) CheckResolvedModel(ctx context.Context, resolved *agentdef.ResolvedModel, apiKey string) error {
	llm, err := newModelFromResolved(resolved, apiKey)
	if err != nil {
		return err
	}
	request := &model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("Reply with OK.", genai.RoleUser)},
	}
	for response, generateErr := range llm.GenerateContent(ctx, request, false) {
		if generateErr != nil {
			return generateErr
		}
		if response == nil || response.Content == nil {
			return errors.New("model endpoint returned no assistant content")
		}
		return nil
	}
	return errors.New("model endpoint returned no response")
}

func newModelFromResolved(resolved *agentdef.ResolvedModel, apiKey string) (*openaillm.OpenAICompatibleLLM, error) {
	opts := []openaillm.Option{
		openaillm.WithAPIKey(apiKey),
		openaillm.WithBaseURL(resolved.BaseURL),
		openaillm.WithModel(resolved.Model),
	}
	if len(resolved.Headers) > 0 {
		opts = append(opts, openaillm.WithHeaders(resolved.Headers))
	}
	if resolved.ReasoningEffort != "" {
		opts = append(opts, openaillm.WithReasoningEffort(resolved.ReasoningEffort))
	}
	if len(resolved.ExtraBody) > 0 {
		opts = append(opts, openaillm.WithExtraBody(resolved.ExtraBody))
	}
	return openaillm.New(opts...)
}

// cliProviderChecker implements doctor.CLIProviderChecker for agent_cli
// providers through the same construction and handshake path used at startup.
type cliProviderChecker struct{}

func (cliProviderChecker) CheckProvider(ctx context.Context, resolved *agentdef.ResolvedModel, cfg config.Config, projectRoot string, describe bool) (doctor.CLIProviderCheck, error) {
	paths, err := cfg.ResolvePaths(projectRoot)
	if err != nil {
		return doctor.CLIProviderCheck{}, err
	}
	cliModel, err := buildAgentCLIModel(ctx, resolved, cfg, paths, nil, nil)
	if err != nil {
		return doctor.CLIProviderCheck{}, err
	}
	description, err := handshakeAgentCLI(ctx, cliModel, describe)
	if err != nil {
		return doctor.CLIProviderCheck{}, err
	}
	if describe {
		return doctor.CLIProviderCheck{
			Detail:   fmt.Sprintf("shim %s (%s) maps CLI version %s; profile validated", description.Name, description.ShimVersion, description.CLIVersion),
			ShimName: description.Name,
		}, nil
	}
	return doctor.CLIProviderCheck{Detail: "profile validated"}, nil
}

type acpProviderChecker struct{}

func (acpProviderChecker) CheckProvider(ctx context.Context, resolved *agentdef.ResolvedModel, projectRoots map[string]string) (string, error) {
	client := acpclient.New(resolved.Command, resolved.Args)
	description, err := client.Describe(ctx)
	if err != nil {
		return "", err
	}
	options := make([]domain.ACPConfigOption, 0, len(resolved.ConfigOptions))
	for _, option := range resolved.ConfigOptions {
		options = append(options, domain.ACPConfigOption{ID: option.ID, Value: option.Value})
	}
	if len(projectRoots) == 0 {
		return "", errors.New("ACP provider has no registered project to probe")
	}
	for name, path := range projectRoots {
		canonical, err := canonicalProjectPath(path)
		if err != nil {
			return "", fmt.Errorf("project %q: %w", name, err)
		}
		if err := client.Probe(ctx, canonical, nil, options); err != nil {
			return "", fmt.Errorf("project %q: %w", name, err)
		}
	}
	additional := "controlled fallback unavailable"
	if description.SessionCapabilities.AdditionalDirectories {
		additional = "native additionalDirectories available"
	}
	return fmt.Sprintf("%s %s uses ACP v%s; profile verified; %s", description.AgentInfo.Name, description.AgentInfo.Version, description.ProtocolVersion, additional), nil
}

// CheckAuthentication reports saved-login status for a known mapper identity
// without making a model call. The command is application-owned and selected
// from the trusted describe name; shims can never supply authentication argv.
// Native output can contain account identifiers, so both streams are drained
// and discarded.
func (cliProviderChecker) CheckAuthentication(ctx context.Context, _ *agentdef.ResolvedModel, shimName string) (string, error) {
	var (
		executable string
		args       []string
		success    string
	)
	switch shimName {
	case "opencode":
		executable, args = "opencode", []string{"auth", "list"}
		success = "opencode auth list succeeded; saved credentials are available"
	case "codex":
		executable, args = "codex", []string{"login", "status"}
		success = "codex login status succeeded; saved credentials are available"
	default:
		return "", fmt.Errorf("authentication status for shim %q is not supported by this release", shimName)
	}
	resolved, err := exec.LookPath(executable)
	if err != nil {
		return "", fmt.Errorf("%s executable not found: %w", executable, err)
	}
	command := exec.CommandContext(ctx, resolved, args...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("%s %s cancelled: %w", executable, strings.Join(args, " "), ctxErr)
		}
		return "", fmt.Errorf("%s %s failed: %w", executable, strings.Join(args, " "), err)
	}
	return success, nil
}
