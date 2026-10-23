package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProvidersAndCatalog(t *testing.T) {
	t.Setenv("ROUTER_CONFIG", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")
	t.Setenv("INCEPTION_API_KEY", "")
	t.Setenv("TEST_PROVIDER_KEY", "from-process")
	path := filepath.Join(t.TempDir(), "router.json")
	data := `{"providers":[{"id":"mine","base_url":"https://example.com/v1/","api_key_env":"TEST_PROVIDER_KEY","models":[{"id":"model-a","context_window":128000,"clear_tool_mode":true}]}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadProviders(path, map[string]string{"TEST_PROVIDER_KEY": "from-file"})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].APIKey != "from-file" || loaded[0].BaseURL != "https://example.com/v1" {
		t.Fatalf("unexpected provider config (count=%d)", len(loaded))
	}
	encoded, err := buildUnionCatalog(map[string]any{"models": []any{map[string]any{"slug": baseCatalogModel, "tool_mode": "code_mode_only"}}}, loaded)
	if err != nil {
		t.Fatal(err)
	}
	var catalog map[string]any
	if err := json.Unmarshal(encoded, &catalog); err != nil {
		t.Fatal(err)
	}
	model := findModel(catalog["models"].([]any), "mine-model-a")
	if model == nil || model["context_window"] != float64(128000) || model["display_name"] != "mine/model-a" || model["tool_mode"] != nil {
		t.Fatalf("custom catalog entry = %v", model)
	}
	loaded, err = loadProviders(path, map[string]string{"OPENCODE_GO_API_KEY": "go-key"})
	if err != nil || len(loaded) != 2 || loaded[0].ID != "go" || loaded[1].APIKey != "from-process" {
		t.Fatalf("built-in plus custom load: count=%d, err=%v", len(loaded), err)
	}
}

func TestLoadProvidersRejectsInvalidConfig(t *testing.T) {
	t.Setenv("ROUTER_CONFIG", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")
	t.Setenv("MISSING_PROVIDER_KEY", "")
	for name, data := range map[string]string{
		"reserved id": `{"providers":[{"id":"go"}]}`,
		"bad URL":     `{"providers":[{"id":"mine","base_url":"file:///tmp/test"}]}`,
		"missing key": `{"providers":[{"id":"mine","base_url":"https://example.com","api_key_env":"MISSING_PROVIDER_KEY","models":[{"id":"test"}]}]}`,
		"bad JSON":    `{`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "router.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadProviders(path, nil); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestInceptionKeyAndCatalog(t *testing.T) {
	t.Setenv("OPENCODE_GO_API_KEY", "")
	t.Setenv("ROUTER_CONFIG", "")
	for _, source := range []string{"missing", "file", "process"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("INCEPTION_API_KEY", "")
			env := map[string]string{}
			if source == "file" {
				env["INCEPTION_API_KEY"] = "file-key"
				t.Setenv("INCEPTION_API_KEY", "process-key")
			} else if source == "process" {
				t.Setenv("INCEPTION_API_KEY", "process-key")
			}
			loaded, err := loadProviders(filepath.Join(t.TempDir(), "router.json"), env)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := buildUnionCatalog(map[string]any{"models": []any{map[string]any{"slug": baseCatalogModel, "input_modalities": []any{"text", "image"}, "tool_mode": "code_mode_only"}}}, loaded)
			if err != nil {
				t.Fatal(err)
			}
			var catalog map[string]any
			if err := json.Unmarshal(encoded, &catalog); err != nil {
				t.Fatal(err)
			}
			model := findModel(catalog["models"].([]any), "inception-labs/mercury-3")
			if source == "missing" {
				if len(loaded) != 0 || model != nil {
					t.Fatal("Inception enabled without an API key")
				}
				return
			}
			key := "process-key"
			if source == "file" {
				key = "file-key"
			}
			if len(loaded) != 1 || loaded[0].APIKey != key {
				t.Fatal("Inception key source or precedence is incorrect")
			}
			if model == nil || model["context_window"] != float64(1_000_000) || model["tool_mode"] != nil {
				t.Fatalf("unexpected Mercury catalog entry: %v", model)
			}
			modalities := model["input_modalities"].([]any)
			if len(modalities) != 1 || modalities[0] != "text" || len(model["supported_reasoning_levels"].([]any)) != 4 {
				t.Fatalf("unexpected Mercury capabilities: %v", model)
			}
		})
	}
}

func TestClaudeSubscriptionCatalog(t *testing.T) {
	t.Setenv("OPENCODE_GO_API_KEY", "")
	t.Setenv("INCEPTION_API_KEY", "")
	t.Setenv("CLAUDE_AGENT_PYTHON", "")
	t.Setenv("ROUTER_CONFIG", "")
	path := filepath.Join(t.TempDir(), "router.json")
	loaded, err := loadProviders(path, nil)
	if err != nil || len(loaded) != 0 {
		t.Fatalf("unexpected enabled providers: %v", err)
	}
	python := filepath.Join(t.TempDir(), "python")
	if err := os.WriteFile(python, []byte(`#!/bin/sh
printf '%s' '[{"id":"claude-sonnet-5-5","name":"CC/Sonnet 5.5","context_window":500000,"max_context_window":1000000,"auto_compact_token_limit":467000,"clear_tool_mode":true,"reasoning_levels":["low","medium","high","xhigh","max"]}]'
`), 0700); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadProviders(path, map[string]string{"CLAUDE_AGENT_PYTHON": python})
	if err != nil || len(loaded) != 1 {
		t.Fatalf("Claude not loaded: %v", err)
	}
	if loaded[0].APIKey != "" || loaded[0].RoundTrip == nil {
		t.Fatal("Claude must use SDK login, not an API key")
	}
	encoded, err := buildUnionCatalog(map[string]any{"models": []any{map[string]any{"slug": baseCatalogModel, "tool_mode": "code_mode_only", "auto_compact_token_limit": 10000}}}, loaded)
	if err != nil {
		t.Fatal(err)
	}
	var catalog map[string]any
	if err := json.Unmarshal(encoded, &catalog); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude-sonnet-5-5"} {
		model := findModel(catalog["models"].([]any), "claude/"+name)
		if model == nil || model["tool_mode"] != nil || model["context_window"] != float64(500_000) || model["max_context_window"] != float64(1_000_000) || model["auto_compact_token_limit"] != float64(467_000) || model["display_name"] != "CC/Sonnet 5.5" || len(model["supported_reasoning_levels"].([]any)) != 5 {
			t.Fatalf("unexpected Claude catalog: %v", model)
		}
	}
}
