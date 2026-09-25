package main

import (
	"encoding/json"
	"testing"
)

func TestBuildUnionCatalogAppendsGoModels(t *testing.T) {
	catalog := map[string]any{
		"models": []any{
			map[string]any{
				"slug":       "gpt-6-luna",
				"priority":   float64(3),
				"tool_mode":  "code_mode_only",
				"models_tmp": "ignored",
			},
		},
	}
	merged, err := buildUnionCatalog(catalog)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(merged, &result); err != nil {
		t.Fatal(err)
	}
	models, _ := result["models"].([]any)
	if len(models) != 1+len(goModels) {
		t.Fatalf("got %d models, want %d", len(models), 1+len(goModels))
	}
	bySlug := map[string]map[string]any{}
	for _, entry := range models {
		model, _ := entry.(map[string]any)
		bySlug[model["slug"].(string)] = model
	}
	for _, spec := range goModels {
		model := bySlug[goPrefix+spec.ID]
		if model == nil {
			t.Fatalf("missing model %s", goPrefix+spec.ID)
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
	}
	base := bySlug["gpt-6-luna"]
	if base["tool_mode"] != "code_mode_only" {
		t.Errorf("base model was modified: %v", base["tool_mode"])
	}
}

func TestConvertReasoningItemMovesRawReasoningToSummary(t *testing.T) {
	item := map[string]any{
		"type": "reasoning",
		"content": []any{
			map[string]any{"type": "reasoning_text", "text": "step"},
		},
	}
	convertReasoningItem(item)
	summary, _ := item["summary"].([]any)
	if len(summary) != 1 {
		t.Fatalf("summary = %v, want one entry", item["summary"])
	}
	if part := summary[0].(map[string]any); part["type"] != "summary_text" || part["text"] != "step" {
		t.Errorf("summary part = %v", part)
	}
	if content := item["content"].([]any); len(content) != 0 {
		t.Errorf("content = %v, want empty", content)
	}
}
