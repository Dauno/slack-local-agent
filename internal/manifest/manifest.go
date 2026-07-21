// Package manifest renders the Slack application manifest managed by
// local-agent. It does not read configuration or write project files.
package manifest

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"text/template"
)

const slackCreateAppURL = "https://api.slack.com/apps"

//go:embed app-manifest.yaml.tmpl
var assets embed.FS

var appTemplate = template.Must(
	template.New("app-manifest.yaml.tmpl").
		Funcs(template.FuncMap{"quote": strconv.Quote}).
		ParseFS(assets, "app-manifest.yaml.tmpl"),
)

// Identity contains the user-configurable names rendered into Slack.
type Identity struct {
	AppName         string
	BotDisplayName  string
	CanvasesEnabled bool
	ExportsEnabled  bool
}

// Render returns a deterministic Slack manifest for the supplied identity.
func Render(identity Identity) (string, error) {
	if err := identity.validate(); err != nil {
		return "", err
	}

	var output bytes.Buffer
	if err := appTemplate.ExecuteTemplate(&output, "app-manifest.yaml.tmpl", identity); err != nil {
		return "", fmt.Errorf("render Slack manifest: %w", err)
	}
	return output.String(), nil
}

// CreationURL returns Slack's create-app URL with the rendered YAML embedded.
func CreationURL(rendered string) (string, error) {
	if strings.TrimSpace(rendered) == "" {
		return "", errors.New("rendered Slack manifest is required")
	}

	target, err := url.Parse(slackCreateAppURL)
	if err != nil {
		return "", fmt.Errorf("parse Slack creation URL: %w", err)
	}
	query := target.Query()
	query.Set("new_app", "1")
	query.Set("manifest_yaml", rendered)
	target.RawQuery = query.Encode()
	return target.String(), nil
}

// RenderCreationURL renders a manifest and embeds it in Slack's create-app URL.
func RenderCreationURL(identity Identity) (string, error) {
	rendered, err := Render(identity)
	if err != nil {
		return "", err
	}
	return CreationURL(rendered)
}

func (i Identity) validate() error {
	if strings.TrimSpace(i.AppName) == "" {
		return errors.New("Slack app name is required")
	}
	if strings.TrimSpace(i.BotDisplayName) == "" {
		return errors.New("Slack bot display name is required")
	}
	if strings.ContainsAny(i.AppName, "\r\n\x00") {
		return errors.New("Slack app name must be a single line")
	}
	if strings.ContainsAny(i.BotDisplayName, "\r\n\x00") {
		return errors.New("Slack bot display name must be a single line")
	}
	return nil
}
