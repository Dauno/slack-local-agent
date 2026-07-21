package manifest

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRenderIncludesIdentitySocketModeScopesAndEvents(t *testing.T) {
	t.Parallel()

	identity := Identity{AppName: "Local Agent: Dev", BotDisplayName: "Dev #1", CanvasesEnabled: true, ExportsEnabled: true}
	got, err := Render(identity)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	var parsed struct {
		DisplayInformation struct {
			Name string `yaml:"name"`
		} `yaml:"display_information"`
		Features struct {
			AppHome struct {
				MessagesTabEnabled         bool `yaml:"messages_tab_enabled"`
				MessagesTabReadOnlyEnabled bool `yaml:"messages_tab_read_only_enabled"`
			} `yaml:"app_home"`
			BotUser struct {
				DisplayName string `yaml:"display_name"`
			} `yaml:"bot_user"`
		} `yaml:"features"`
		OAuthConfig struct {
			Scopes struct {
				Bot []string `yaml:"bot"`
			} `yaml:"scopes"`
		} `yaml:"oauth_config"`
		Settings struct {
			Interactivity struct {
				IsEnabled bool `yaml:"is_enabled"`
			} `yaml:"interactivity"`
			EventSubscriptions struct {
				BotEvents []string `yaml:"bot_events"`
			} `yaml:"event_subscriptions"`
			SocketModeEnabled bool `yaml:"socket_mode_enabled"`
		} `yaml:"settings"`
	}
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("rendered manifest is invalid YAML: %v\n%s", err, got)
	}

	if parsed.DisplayInformation.Name != identity.AppName {
		t.Fatalf("app name = %q, want %q", parsed.DisplayInformation.Name, identity.AppName)
	}
	if parsed.Features.BotUser.DisplayName != identity.BotDisplayName {
		t.Fatalf("bot display name = %q, want %q", parsed.Features.BotUser.DisplayName, identity.BotDisplayName)
	}
	if !parsed.Features.AppHome.MessagesTabEnabled || parsed.Features.AppHome.MessagesTabReadOnlyEnabled {
		t.Fatalf("App Home messages tab must accept user messages: %#v", parsed.Features.AppHome)
	}
	if !parsed.Settings.SocketModeEnabled {
		t.Fatal("Socket Mode is not enabled")
	}
	if !parsed.Settings.Interactivity.IsEnabled {
		t.Fatal("interactivity is not enabled")
	}

	wantScopes := []string{
		"app_mentions:read",
		"channels:history",
		"channels:read",
		"chat:write",
		"canvases:write",
		"files:write",
		"files:read",
		"groups:history",
		"groups:read",
		"im:history",
		"im:write",
		"users:read",
	}
	if !reflect.DeepEqual(parsed.OAuthConfig.Scopes.Bot, wantScopes) {
		t.Fatalf("bot scopes = %#v, want %#v", parsed.OAuthConfig.Scopes.Bot, wantScopes)
	}
	wantEvents := []string{"app_mention", "message.channels", "message.groups", "message.im"}
	if !reflect.DeepEqual(parsed.Settings.EventSubscriptions.BotEvents, wantEvents) {
		t.Fatalf("bot events = %#v, want %#v", parsed.Settings.EventSubscriptions.BotEvents, wantEvents)
	}
	if !strings.Contains(got, "connections:write") {
		t.Fatal("manifest does not explain the required app-level token scope")
	}

	again, err := Render(identity)
	if err != nil {
		t.Fatal(err)
	}
	if again != got {
		t.Fatal("Render() is not deterministic")
	}
}

func TestCreationURLRoundTripsRenderedManifest(t *testing.T) {
	t.Parallel()

	rendered, err := Render(Identity{AppName: "Local Agent", BotDisplayName: "Dev Agent"})
	if err != nil {
		t.Fatal(err)
	}
	creationURL, err := CreationURL(rendered)
	if err != nil {
		t.Fatalf("CreationURL() error = %v", err)
	}

	parsed, err := url.Parse(creationURL)
	if err != nil {
		t.Fatalf("creation URL is invalid: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "api.slack.com" || parsed.Path != "/apps" {
		t.Fatalf("creation URL target = %s", parsed.String())
	}
	if got := parsed.Query().Get("new_app"); got != "1" {
		t.Fatalf("new_app = %q, want 1", got)
	}
	if got := parsed.Query().Get("manifest_yaml"); got != rendered {
		t.Fatal("manifest_yaml did not round-trip")
	}

	combined, err := RenderCreationURL(Identity{AppName: "Local Agent", BotDisplayName: "Dev Agent"})
	if err != nil {
		t.Fatal(err)
	}
	if combined != creationURL {
		t.Fatal("RenderCreationURL() differs from Render() plus CreationURL()")
	}
}

func TestRenderOmitsCanvasScopeWhenDisabled(t *testing.T) {
	rendered, err := Render(Identity{AppName: "Local Agent", BotDisplayName: "Dev Agent"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, "canvases:write") {
		t.Fatal("disabled Canvas feature requested canvases:write")
	}
}

func TestRenderOmitsGeneratedFileScopeWhenDisabled(t *testing.T) {
	rendered, err := Render(Identity{AppName: "Local Agent", BotDisplayName: "Dev Agent"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, "files:write") {
		t.Fatal("disabled exports requested files:write")
	}
}

func TestRenderRejectsInvalidIdentity(t *testing.T) {
	t.Parallel()

	tests := []Identity{
		{},
		{AppName: "Local Agent"},
		{AppName: "Local\nAgent", BotDisplayName: "Dev Agent"},
		{AppName: "Local Agent", BotDisplayName: "Dev\nAgent"},
	}
	for _, identity := range tests {
		if _, err := Render(identity); err == nil {
			t.Fatalf("Render(%#v) succeeded", identity)
		}
	}
	if _, err := CreationURL(" \n"); err == nil {
		t.Fatal("CreationURL() accepted an empty manifest")
	}
}
