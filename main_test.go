package main

import (
	"encoding/json"
	"testing"
)

func TestNormalizeReasoningRemovesEffortForMuse(t *testing.T) {
	payload := map[string]any{"reasoning": map[string]any{"effort": "max", "summary": "auto"}}
	normalizeReasoning(payload, "muse-spark-1.3-contributor")
	reasoning, _ := payload["reasoning"].(map[string]any)
	if _, ok := reasoning["effort"]; ok {
		t.Fatal("effort still present for muse")
	}
	if reasoning["summary"] != "auto" {
		t.Fatalf("summary = %v, want auto", reasoning["summary"])
	}

	payload = map[string]any{"reasoning": map[string]any{"effort": "high"}}
	normalizeReasoning(payload, "gpt-6-luna")
	reasoning, _ = payload["reasoning"].(map[string]any)
	if _, ok := reasoning["effort"]; !ok {
		t.Fatal("effort removed for a model that supports it")
	}
}

func TestNormalizeToolSchemasAddsMissingRequired(t *testing.T) {
	payload := map[string]any{"tools": []any{
		map[string]any{
			"type": "tool_search",
			"parameters": map[string]any{
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
					"limit": map[string]any{"type": "number"},
				},
				"required": []any{"query"},
			},
		},
	}}
	normalizeToolSchemas(payload, "tool_search")
	tool, _ := payload["tools"].([]any)[0].(map[string]any)
	parameters, _ := tool["parameters"].(map[string]any)
	required, _ := parameters["required"].([]any)
	if len(required) != 2 || required[0] != "query" || required[1] != "limit" {
		t.Fatalf("required = %v, want [query limit]", required)
	}
}

func TestStripForeignReasoningKeepsOwnProviderOnly(t *testing.T) {
	chatgptReasoning := map[string]any{"type": "reasoning", "id": "rs_abc", "encrypted_content": "gAAAAAB"}
	goReasoning := map[string]any{"type": "reasoning", "id": "4f97e6be-717a-4d16-9f28-d282d6b056e8-0", "encrypted_content": "4f97e6be-717a-4d16-9f28-d282d6b056e8-0"}
	message := map[string]any{"type": "message", "role": "user", "content": []any{}}

	payload := map[string]any{"input": []any{message, chatgptReasoning, goReasoning}}
	stripForeignReasoning(payload, false)
	kept := payload["input"].([]any)
	if len(kept) != 2 {
		t.Fatalf("chatgpt request kept %d items, want 2", len(kept))
	}
	first, _ := kept[0].(map[string]any)
	second, _ := kept[1].(map[string]any)
	if first["type"] != "message" || second["id"] != "rs_abc" {
		t.Fatalf("chatgpt request kept %v, want message and chatgpt reasoning", kept)
	}

	payload = map[string]any{"input": []any{message, chatgptReasoning, goReasoning}}
	stripForeignReasoning(payload, true)
	kept = payload["input"].([]any)
	if len(kept) != 2 {
		t.Fatalf("go request kept %d items, want 2", len(kept))
	}
	first, _ = kept[0].(map[string]any)
	second, _ = kept[1].(map[string]any)
	if first["type"] != "message" || second["id"] != goReasoning["id"] {
		t.Fatalf("go request kept %v, want message and go reasoning", kept)
	}
}

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
