package main

import (
	"codex-go-router/src/provider"
	"encoding/base64"
	"encoding/json"
	"strings"
)

// Tag reasoning state statelessly so switching endpoints never forwards another
// provider's encrypted content. This is an encoding, not encryption.
const reasoningPrefix = "codex-router:"

type providerReasoning struct {
	Provider string `json:"provider"`
	Content  any    `json:"content"`
}

func tagProviderReasoning(value any, id string) {
	item, ok := value.(map[string]any)
	if !ok || item["type"] != "reasoning" {
		return
	}
	encoded, _ := json.Marshal(providerReasoning{Provider: id, Content: item["encrypted_content"]})
	item["encrypted_content"] = reasoningPrefix + base64.RawURLEncoding.EncodeToString(encoded)
}

func tagResponseReasoning(response map[string]any, id string) {
	output, _ := response["output"].([]any)
	for _, item := range output {
		tagProviderReasoning(item, id)
	}
}

// stripForeignReasoning keeps only reasoning state belonging to the destination.
// Untagged state retains the existing ChatGPT/OpenCode ID heuristics.
func stripForeignReasoning(payload map[string]any, destination string) {
	input, ok := payload["input"].([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(input))
	for _, entry := range input {
		item, ok := entry.(map[string]any)
		if !ok || item["type"] != "reasoning" {
			kept = append(kept, entry)
			continue
		}
		if token, ok := item["encrypted_content"].(string); ok && strings.HasPrefix(token, reasoningPrefix) {
			data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, reasoningPrefix))
			var state providerReasoning
			if err == nil && json.Unmarshal(data, &state) == nil && state.Provider == destination {
				item["encrypted_content"] = state.Content
				kept = append(kept, entry)
			}
			continue
		}
		if (destination == "" && isChatGPTReasoning(item)) || (destination == "go" && !isChatGPTReasoning(item)) {
			kept = append(kept, entry)
		}
	}
	payload["input"] = kept
}

func isChatGPTReasoning(item map[string]any) bool {
	if id, ok := item["id"].(string); ok && strings.HasPrefix(id, "rs_") {
		return true
	}
	if encrypted, ok := item["encrypted_content"].(string); ok && strings.HasPrefix(encrypted, "gAAAA") {
		return true
	}
	return false
}

// withReasoningTags marks state so only the originating endpoint receives it back.
func withReasoningTags(p provider.Provider) provider.Provider {
	p.RewriteEvent = func(payload map[string]any) {
		tagProviderReasoning(payload["item"], p.ID)
		if response, ok := payload["response"].(map[string]any); ok {
			tagResponseReasoning(response, p.ID)
		}
	}
	p.RewriteResponse = func(payload map[string]any) { tagResponseReasoning(payload, p.ID) }
	return p
}
