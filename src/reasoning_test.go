package main

import (
	"testing"
)

func TestStripForeignReasoningKeepsOwnProviderOnly(t *testing.T) {
	chatgptReasoning := map[string]any{"type": "reasoning", "id": "rs_abc", "encrypted_content": "gAAAAAB"}
	goReasoning := map[string]any{"type": "reasoning", "id": "4f97e6be-717a-4d16-9f28-d282d6b056e8-0", "encrypted_content": "4f97e6be-717a-4d16-9f28-d282d6b056e8-0"}
	message := map[string]any{"type": "message", "role": "user", "content": []any{}}

	payload := map[string]any{"input": []any{message, chatgptReasoning, goReasoning}}
	stripForeignReasoning(payload, "")
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
	stripForeignReasoning(payload, "go")
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
