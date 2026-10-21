package provider

import (
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

func TestConvertReasoningItemPreservesRawReasoningWithSummary(t *testing.T) {
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
	content := item["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want original reasoning", content)
	}
	if part := content[0].(map[string]any); part["type"] != "reasoning_text" || part["text"] != "step" {
		t.Errorf("reasoning content = %v, want original reasoning_text", part)
	}
}
