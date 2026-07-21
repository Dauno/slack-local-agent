package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/adapter/envfile"
	"github.com/Dauno/slack-local-agent/internal/adapter/fsproject"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/buildinfo"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/manifest"
	"github.com/Dauno/slack-local-agent/internal/usecase/bootstrap"
	"github.com/Dauno/slack-local-agent/internal/usecase/doctor"
)

type Application struct {
	root      string
	logOutput io.Writer
}

func New(projectRoot string, logOutput io.Writer) (*Application, error) {
	if strings.TrimSpace(projectRoot) == "" {
		return nil, errors.New("project root is required")
	}
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve project root: %w", err)
	}
	if logOutput == nil {
		logOutput = io.Discard
	}
	return &Application{root: filepath.Clean(root), logOutput: logOutput}, nil
}

func (a *Application) PrepareSetup(ctx context.Context) (bootstrap.Snapshot, bootstrap.Secrets, error) {
	service, err := a.bootstrapService()
	if err != nil {
		return bootstrap.Snapshot{}, bootstrap.Secrets{}, err
	}
	snapshot, err := service.EnsureBaseArtifacts(ctx, a.root)
	if err != nil {
		return bootstrap.Snapshot{}, bootstrap.Secrets{}, err
	}
	values, err := envfile.NewResolver(snapshot.Paths.EnvFile).Resolve(
		snapshot.Config.Model.APIKeyEnv,
		bootstrap.SlackBotTokenEnv,
		bootstrap.SlackAppTokenEnv,
	)
	if err != nil {
		return bootstrap.Snapshot{}, bootstrap.Secrets{}, err
	}
	return snapshot, bootstrap.Secrets{
		ModelAPIKey:   values[snapshot.Config.Model.APIKeyEnv],
		SlackBotToken: values[bootstrap.SlackBotTokenEnv],
		SlackAppToken: values[bootstrap.SlackAppTokenEnv],
	}, nil
}

func (a *Application) ApplySetup(
	ctx context.Context,
	snapshot bootstrap.Snapshot,
	identity bootstrap.Identity,
	access bootstrap.AccessControl,
	secrets bootstrap.Secrets,
) error {
	service, err := a.bootstrapService()
	if err != nil {
		return err
	}
	_, err = service.ApplyConfirmedUpdates(ctx, snapshot, identity, access, secrets)
	return err
}

func (a *Application) Doctor(ctx context.Context, includeLive bool) (doctor.Report, error) {
	configPath, err := config.ConfigPath(a.root)
	if err != nil {
		return doctor.Report{}, err
	}
	dependencies := doctor.Dependencies{
		ConfigPath: configPath,
		Secrets:    envfile.NewResolver(filepath.Join(a.root, config.DefaultEnvFile)),
		Database:   databaseChecker{},
		CLI:        cliProviderChecker{},
		ACP:        acpProviderChecker{},
	}
	if includeLive {
		dependencies.Live = liveChecker{}
	}
	service, err := doctor.New(dependencies)
	if err != nil {
		return doctor.Report{}, err
	}
	return service.Run(ctx, includeLive), nil
}

func (a *Application) Manifest(ctx context.Context, write bool) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	configPath, err := config.ConfigPath(a.root)
	if err != nil {
		return "", "", err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", errors.New("Configuration not found. Run: local-agent init")
		}
		return "", "", err
	}
	paths, err := cfg.ResolvePaths(a.root)
	if err != nil {
		return "", "", err
	}
	rendered, err := manifest.Render(manifest.Identity{
		AppName: cfg.Slack.AppName, BotDisplayName: cfg.Slack.BotDisplayName, CanvasesEnabled: cfg.Canvases.Enabled, ExportsEnabled: cfg.Exports.Enabled,
	})
	if err != nil {
		return "", "", err
	}
	if write {
		files := fsproject.New()
		if err := files.WriteBatch(ctx,
			map[string][]byte{paths.ManifestFile: []byte(rendered)},
			map[string]os.FileMode{paths.ManifestFile: 0o644},
			nil,
		); err != nil {
			return "", "", fmt.Errorf("write Slack manifest: %w", err)
		}
	}
	return rendered, paths.ManifestFile, nil
}

func (*Application) Version() string { return buildinfo.String() }

// ResetState implements the destructive init --reset-state command.
// It deletes the SQLite database and generated memory projections.
// Slack messages and remote sandbox resources are not affected.
func (a *Application) ResetState(ctx context.Context) error {
	configPath, err := config.ConfigPath(a.root)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("configuration not found — nothing to reset")
		}
		return fmt.Errorf("load config: %w", err)
	}
	paths, err := cfg.ResolvePaths(a.root)
	if err != nil {
		return fmt.Errorf("resolve paths: %w", err)
	}

	dbPath := paths.DatabaseFile
	if _, statErr := os.Stat(dbPath); errors.Is(statErr, os.ErrNotExist) {
		return errors.New("no existing database found — nothing to reset")
	}

	if err := os.Remove(dbPath); err != nil {
		return fmt.Errorf("delete database %s: %w", dbPath, err)
	}
	store, err := adaptersqlite.Initialize(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("initialize fresh database: %w", err)
	}
	if err := store.Close(); err != nil {
		return fmt.Errorf("close fresh database: %w", err)
	}

	// Clean up memory projections if they exist.
	memoryDir := filepath.Join(a.root, ".local-agent", "memory")
	if _, statErr := os.Stat(memoryDir); statErr == nil {
		if err := os.RemoveAll(memoryDir); err != nil {
			return fmt.Errorf("delete memory projections: %w", err)
		}
	}

	fmt.Fprintf(a.logOutput, "State reset complete. Fresh database initialized and memory projections deleted.\n")
	return nil
}

func (a *Application) bootstrapService() (*bootstrap.Service, error) {
	return bootstrap.New(fsproject.New(), bootstrap.DatabaseInitializerFunc(func(ctx context.Context, path string) error {
		store, err := adaptersqlite.Initialize(ctx, path)
		if err != nil {
			return err
		}
		return store.Close()
	}), bootstrap.SecretEditorFunc(envfile.Render))
}

type databaseChecker struct{}

func (databaseChecker) CheckDatabase(ctx context.Context, path string) error {
	store, err := adaptersqlite.OpenExisting(ctx, path)
	if err != nil {
		if errors.Is(err, adaptersqlite.ErrFutureSchema) {
			return &doctor.ActionableError{
				Err: err,
				Fix: "Install a local-agent version that supports this database. To discard local conversation state, stop the agent, back up and delete only the configured database file, then run init.",
			}
		}
		if errors.Is(err, adaptersqlite.ErrStateResetNeeded) {
			return &doctor.ActionableError{
				Err: err,
				Fix: "Run: local-agent init --reset-state to discard incompatible local state.",
			}
		}
		return err
	}
	defer store.Close()
	return store.ProbeReadWrite(ctx)
}
