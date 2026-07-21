package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type sourceDocument struct {
	node *yaml.Node
}

type schemaField struct {
	name     string
	children []schemaField
}

var configSchema = []schemaField{
	{name: "agent", children: []schemaField{
		{name: "name"},
	}},
	{name: "state", children: []schemaField{
		{name: "dir"},
		{name: "db"},
	}},
	{name: "context", children: []schemaField{
		{name: "max_messages"},
		{name: "max_chars"},
		{name: "retain_messages_per_conversation"},
	}},
	{name: "runtime", children: []schemaField{
		{name: "log_level"},
		{name: "model_timeout_seconds"},
		{name: "slack_api_timeout_seconds"},
		{name: "max_concurrent_model_calls"},
		{name: "busy_message"},
		{name: "model_error_message"},
	}},
	{name: "model", children: []schemaField{
		{name: "name"},
		{name: "base_url"},
		{name: "api_key_env"},
		{name: "headers"},
		{name: "reasoning_effort"},
		{name: "extra_body"},
	}},
	{name: "slack", children: []schemaField{
		{name: "app_name"},
		{name: "bot_display_name"},
		{name: "unauthorized_message"},
		{name: "allow_all_users"},
		{name: "allowed_user_ids"},
		{name: "allowed_team_ids"},
		{name: "allowed_channel_ids"},
		{name: "part_labels"},
		{name: "standard_agent", children: []schemaField{
			{name: "threaded_dm"},
			{name: "progress_enabled"},
			{name: "prompts_enabled"},
			{name: "suggested_prompts"},
			{name: "streaming_enabled"},
			{name: "update_interval_seconds"},
		}},
		{name: "context", children: []schemaField{
			{name: "enabled"},
			{name: "max_chars"},
			{name: "timeout_seconds"},
			{name: "profile_cache_ttl_minutes"},
			{name: "conversation_cache_ttl_minutes"},
		}},
		{name: "files", children: []schemaField{
			{name: "max_bytes_per_file"},
			{name: "max_processed_chars"},
		}},
	}},
	{name: "memory", children: []schemaField{
		{name: "enabled"},
		{name: "directory"},
		{name: "max_topics_recall"},
		{name: "max_chars_recall"},
		{name: "recall_timeout_seconds"},
		{name: "curator_timeout_seconds"},
		{name: "curator_max_retries"},
		{name: "worker_interval_seconds"},
		{name: "retention_days"},
		{name: "max_topics"},
		{name: "max_links"},
		{name: "max_topic_chars"},
		{name: "max_patch_ops"},
	}},
	{name: "sandbox", children: []schemaField{
		{name: "enabled"},
		{name: "projects"},
		{name: "command_timeout_seconds"},
		{name: "max_output_bytes"},
	}},
	{name: "canvases", children: []schemaField{
		{name: "enabled"},
		{name: "max_title_chars"},
		{name: "max_content_chars"},
		{name: "max_content_bytes"},
		{name: "timeout_seconds"},
	}},
	{name: "exports", children: []schemaField{
		{name: "enabled"},
		{name: "max_filename_chars"},
		{name: "max_content_bytes"},
		{name: "timeout_seconds"},
	}},
	{name: "opencode", children: []schemaField{
		{name: "management", children: []schemaField{
			{name: "allowed_user_ids"},
		}},
	}},
}

// Load reads, applies defaults to, and validates a YAML config file.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("load configuration %q: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, fmt.Errorf("load configuration %q: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes one YAML document, applies defaults to omitted fields, and
// validates the result. Unknown YAML fields are retained for later Marshal or
// Save calls.
func Parse(data []byte) (Config, error) {
	document, err := decodeDocument(data)
	if err != nil {
		return Config{}, err
	}
	root, err := mappingRoot(document)
	if err != nil {
		return Config{}, err
	}

	// Decoding once independently catches duplicate keys, including unknown
	// extension fields that are intentionally preserved.
	var syntax any
	if err := document.Decode(&syntax); err != nil {
		return Config{}, fmt.Errorf("decode configuration YAML: %w", err)
	}

	effective, err := configNode(Default())
	if err != nil {
		return Config{}, fmt.Errorf("encode configuration defaults: %w", err)
	}
	overlayPresentFields(effective, root, configSchema)

	var cfg Config
	if err := effective.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode typed configuration: %w", err)
	}
	normalizeCollections(&cfg)
	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	cfg.document = &sourceDocument{node: cloneNode(document)}
	return cfg, nil
}

// Marshal validates cfg and renders deterministic two-space-indented YAML.
// When cfg came from Parse or Load, unknown fields and comments are retained.
func Marshal(cfg Config) ([]byte, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	known, err := configNode(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode typed configuration: %w", err)
	}

	var document *yaml.Node
	if cfg.document == nil || cfg.document.node == nil {
		document = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{known}}
	} else {
		document = cloneNode(cfg.document.node)
		root, rootErr := mappingRoot(document)
		if rootErr != nil {
			return nil, rootErr
		}
		mergeKnownFields(root, known, configSchema)
	}

	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, fmt.Errorf("encode configuration YAML: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("finish configuration YAML: %w", err)
	}
	return output.Bytes(), nil
}

// Save validates and atomically writes cfg. Existing file permissions are
// retained; a new non-sensitive configuration file is created with mode 0644.
func Save(path string, cfg Config) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("save configuration: path must not be empty")
	}
	if strings.ContainsRune(path, '\x00') {
		return errors.New("save configuration: path must not contain a NUL byte")
	}
	data, err := Marshal(cfg)
	if err != nil {
		return fmt.Errorf("save configuration %q: %w", path, err)
	}

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("save configuration %q: create parent directory: %w", path, err)
	}

	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		if info.IsDir() {
			return fmt.Errorf("save configuration %q: path is a directory", path)
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("save configuration %q: inspect existing file: %w", path, statErr)
	}

	temporary, err := os.CreateTemp(directory, ".config.yaml-*")
	if err != nil {
		return fmt.Errorf("save configuration %q: create temporary file: %w", path, err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return fmt.Errorf("save configuration %q: set file permissions: %w", path, err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("save configuration %q: write temporary file: %w", path, err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("save configuration %q: sync temporary file: %w", path, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("save configuration %q: close temporary file: %w", path, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("save configuration %q: replace file: %w", path, err)
	}
	return nil
}

func decodeDocument(data []byte) (*yaml.Node, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return emptyDocument(), nil
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			return emptyDocument(), nil
		}
		return nil, fmt.Errorf("decode configuration YAML: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err == nil {
		return nil, errors.New("decode configuration YAML: expected one YAML document")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode configuration YAML: %w", err)
	}
	return &document, nil
}

func emptyDocument() *yaml.Node {
	return &yaml.Node{
		Kind: yaml.DocumentNode,
		Content: []*yaml.Node{{
			Kind: yaml.MappingNode,
			Tag:  "!!map",
		}},
	}
}

func mappingRoot(document *yaml.Node) (*yaml.Node, error) {
	if document == nil || document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		return nil, errors.New("decode configuration YAML: expected one document")
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("decode configuration YAML: top level must be a mapping")
	}
	return root, nil
}

func configNode(cfg Config) (*yaml.Node, error) {
	var node yaml.Node
	if err := node.Encode(cfg); err != nil {
		return nil, err
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		return node.Content[0], nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected encoded config mapping, got YAML node kind %d", node.Kind)
	}
	return &node, nil
}

func normalizeCollections(cfg *Config) {
	if cfg.Slack.AllowedUserIDs == nil {
		cfg.Slack.AllowedUserIDs = []string{}
	}
	if cfg.Slack.AllowedTeamIDs == nil {
		cfg.Slack.AllowedTeamIDs = []string{}
	}
	if cfg.Slack.AllowedChannelIDs == nil {
		cfg.Slack.AllowedChannelIDs = []string{}
	}
	if cfg.Slack.StandardAgent.SuggestedPrompts == nil {
		cfg.Slack.StandardAgent.SuggestedPrompts = []string{}
	}
	if cfg.OpenCode.Management.AllowedUserIDs == nil {
		cfg.OpenCode.Management.AllowedUserIDs = []string{}
	}
}

// overlayPresentFields overlays only fields explicitly present in src. Nested
// typed sections merge, while maps with user-defined keys replace their default.
func overlayPresentFields(dst, src *yaml.Node, schema []schemaField) {
	for _, field := range schema {
		srcKey, srcValue, present := mappingEntry(src, field.name)
		if !present {
			continue
		}
		_, dstValue, destinationHasField := mappingEntry(dst, field.name)
		if !destinationHasField {
			dst.Content = append(dst.Content, cloneNode(srcKey), cloneNode(srcValue))
			continue
		}
		if len(field.children) > 0 && srcValue.Kind == yaml.MappingNode && dstValue.Kind == yaml.MappingNode {
			overlayPresentFields(dstValue, srcValue, field.children)
			continue
		}
		replaceMappingValue(dst, field.name, cloneNode(srcValue))
	}
}

// mergeKnownFields updates known values while leaving extension keys in place.
func mergeKnownFields(dst, src *yaml.Node, schema []schemaField) {
	for _, field := range schema {
		srcKey, srcValue, sourceHasField := mappingEntry(src, field.name)
		_, dstValue, destinationHasField := mappingEntry(dst, field.name)
		if !sourceHasField {
			if destinationHasField {
				removeMappingEntry(dst, field.name)
			}
			continue
		}
		if !destinationHasField {
			dst.Content = append(dst.Content, cloneNode(srcKey), cloneNode(srcValue))
			continue
		}
		if len(field.children) > 0 && dstValue.Kind == yaml.MappingNode && srcValue.Kind == yaml.MappingNode {
			mergeKnownFields(dstValue, srcValue, field.children)
			continue
		}

		replacement := cloneNode(srcValue)
		preservePresentation(replacement, dstValue)
		replaceMappingValue(dst, field.name, replacement)
	}
}

func mappingEntry(mapping *yaml.Node, name string) (key, value *yaml.Node, found bool) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, nil, false
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		candidate := mapping.Content[index]
		if candidate.Value == name {
			return candidate, mapping.Content[index+1], true
		}
	}
	return nil, nil, false
}

func replaceMappingValue(mapping *yaml.Node, name string, value *yaml.Node) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == name {
			mapping.Content[index+1] = value
			return
		}
	}
}

func removeMappingEntry(mapping *yaml.Node, name string) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == name {
			mapping.Content = append(mapping.Content[:index], mapping.Content[index+2:]...)
			return
		}
	}
}

func preservePresentation(replacement, original *yaml.Node) {
	replacement.HeadComment = original.HeadComment
	replacement.LineComment = original.LineComment
	replacement.FootComment = original.FootComment
	if replacement.Kind == original.Kind {
		replacement.Style = original.Style
	}
}

func cloneNode(node *yaml.Node) *yaml.Node {
	return cloneNodeWithMemo(node, make(map[*yaml.Node]*yaml.Node))
}

func cloneNodeWithMemo(node *yaml.Node, memo map[*yaml.Node]*yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	if cloned, ok := memo[node]; ok {
		return cloned
	}
	cloned := *node
	cloned.Content = nil
	memo[node] = &cloned
	for _, child := range node.Content {
		cloned.Content = append(cloned.Content, cloneNodeWithMemo(child, memo))
	}
	cloned.Alias = cloneNodeWithMemo(node.Alias, memo)
	return &cloned
}
