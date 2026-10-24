package main

import (
	"codex-go-router/src/provider"
	"encoding/json"
	"testing"
)

func TestBuildUnionCatalogAppendsGoModels(t *testing.T) {
	catalog := map[string]any{
		"models": []any{
			map[string]any{
				"slug":                             "gpt-6-luna",
				"priority":                         float64(3),
				"tool_mode":                        "code_mode_only",
				"context_window":                   float64(272000),
				"max_context_window":               float64(872000),
				"effective_context_window_percent": float64(95),
				"models_tmp":                       "ignored",
			},
		},
	}
	merged, err := buildUnionCatalog(catalog, []provider.Provider{provider.OpenCode()})
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(merged, &result); err != nil {
		t.Fatal(err)
	}
	models, _ := result["models"].([]any)
	if len(models) != 1+len(provider.OpenCode().Models) {
		t.Fatalf("got %d models, want %d", len(models), 1+len(provider.OpenCode().Models))
	}
	bySlug := map[string]map[string]any{}
	for _, entry := range models {
		model, _ := entry.(map[string]any)
		bySlug[model["slug"].(string)] = model
	}
	for _, spec := range provider.OpenCode().Models {
		model := bySlug["go-"+spec.ID]
		if model == nil {
			t.Fatalf("missing model %s", "go-"+spec.ID)
		}
		if model["display_name"] != spec.Name {
			t.Errorf("%s display_name = %v, want %q", spec.ID, model["display_name"], spec.Name)
		}
		if model["default_reasoning_summary"] != "auto" {
			t.Errorf("%s default_reasoning_summary = %v, want auto", spec.ID, model["default_reasoning_summary"])
		}
		if model["use_responses_lite"] != false {
			t.Errorf("%s use_responses_lite = %v, want false", spec.ID, model["use_responses_lite"])
		}
		if spec.ClearToolMode && model["tool_mode"] != nil {
			t.Errorf("%s tool_mode = %v, want nil", spec.ID, model["tool_mode"])
		}
		if spec.ContextWindow > 0 {
			if got := model["context_window"]; got != float64(spec.ContextWindow) {
				t.Errorf("%s context_window = %v, want %d", spec.ID, got, spec.ContextWindow)
			}
			if got := model["max_context_window"]; got != float64(spec.ContextWindow) {
				t.Errorf("%s max_context_window = %v, want %d", spec.ID, got, spec.ContextWindow)
			}
			if model["effective_context_window_percent"] != float64(95) {
				t.Errorf("%s effective_context_window_percent = %v, want 95", spec.ID, model["effective_context_window_percent"])
			}
		}
	}
	base := bySlug["gpt-6-luna"]
	if base["tool_mode"] != "code_mode_only" {
		t.Errorf("base model was modified: %v", base["tool_mode"])
	}
	if base["context_window"] != float64(272000) || base["max_context_window"] != float64(872000) {
		t.Errorf("base model context was modified: %v / %v", base["context_window"], base["max_context_window"])
	}
}
