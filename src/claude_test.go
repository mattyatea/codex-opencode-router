package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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
printf '%s' '[{"id":"claude-sonnet-5-5","name":"Claude Sonnet 5.5","context_window":200000,"clear_tool_mode":true,"reasoning_levels":["low","medium","high","xhigh","max"]}]'
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
	original := providers
	providers = loaded
	t.Cleanup(func() { providers = original })
	encoded, err := buildUnionCatalog(map[string]any{"models": []any{map[string]any{"slug": baseCatalogModel, "tool_mode": "code_mode_only"}}})
	if err != nil {
		t.Fatal(err)
	}
	var catalog map[string]any
	if err := json.Unmarshal(encoded, &catalog); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude-sonnet-5-5"} {
		model := findModel(catalog["models"].([]any), "claude/"+name)
		if model == nil || model["tool_mode"] != nil || model["context_window"] != float64(200_000) || model["display_name"] != "Claude Sonnet 5.5" || len(model["supported_reasoning_levels"].([]any)) != 5 {
			t.Fatalf("unexpected Claude catalog: %v", model)
		}
	}
}
