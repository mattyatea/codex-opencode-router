package provider

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"sort"
	"time"
)

const opencodeOrigin = "https://opencode.ai"

var goModels = []Model{
	{ID: "deepseek-v4.1-flash", Name: "Go/DeepSeek V4.1 Flash", ClearToolMode: true, ContextWindow: 1_000_000},
	{ID: "gpt-6-luna", Name: "Go/GPT-6 Luna", ContextWindow: 1_000_000},
	{ID: "muse-spark-1.3-contributor", Name: "Go/Muse Spark 1.3 (Train)", ClearToolMode: true, ContextWindow: 1_000_000},
}

// sessionHeaderCandidates are checked in order for a stable per-conversation id.
var sessionHeaderCandidates = []string{
	"x-opencode-session", "session-id", "x-codex-session", "thread-id",
	"x-codex-thread-id", "session_id", "x-codex-conversation-id",
}

func OpenCode() Provider {
	return Provider{
		ID:             "go",
		Name:           "OpenCode Go",
		BaseURL:        opencodeOrigin + "/zen/go/v1",
		APIKeyEnv:      "OPENCODE_GO_API_KEY",
		Models:         goModels,
		PrepareRequest: prepareOpenCodeRequest,
		RewriteEvent:   rewriteReasoningEvents,
	}
}

func prepareOpenCodeRequest(payload map[string]any, incoming, headers http.Header) {
	model, _ := payload["model"].(string)
	normalizeToolSchemas(payload, model)
	normalizeReasoning(payload, model)
	headers.Set("x-opencode-session", opencodeSessionID(incoming))
}

// functionOnlyModels are Go models whose endpoint only accepts function tools.
// Codex also sends custom, tool_search, web_search, and image tools, which make
// these upstreams fail.
var functionOnlyModels = map[string]bool{
	"muse-spark-1.3-contributor": true,
	"muse-spark-1.2-contributor": true,
}

// normalizeToolSchemas makes tool schemas satisfy providers that validate
// strict JSON Schema, where every key in `properties` must also appear in
// `required`. Codex marks optional parameters with descriptions and defaults
// instead, so the keys are added while their meaning stays optional.
func normalizeToolSchemas(payload map[string]any, model string) {
	tools, ok := payload["tools"].([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(tools))
	for _, entry := range tools {
		tool, ok := entry.(map[string]any)
		if !ok {
			kept = append(kept, entry)
			continue
		}
		if functionOnlyModels[model] && tool["type"] != "function" {
			continue
		}
		// Strict providers reject these ChatGPT-only web_search fields.
		if tool["type"] != "web_search_preview" {
			delete(tool, "search_content_types")
			delete(tool, "external_web_access")
		}
		normalizeToolParameters(tool["parameters"])
		kept = append(kept, tool)
	}
	payload["tools"] = kept
}

func normalizeToolParameters(value any) {
	parameters, ok := value.(map[string]any)
	if !ok {
		return
	}
	properties, _ := parameters["properties"].(map[string]any)
	if len(properties) == 0 {
		return
	}
	required := make([]string, 0, len(properties))
	seen := map[string]bool{}
	if existing, ok := parameters["required"].([]any); ok {
		for _, name := range existing {
			text := fmt.Sprint(name)
			if !seen[text] {
				required = append(required, text)
				seen[text] = true
			}
		}
	}
	missing := make([]string, 0)
	for name := range properties {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	for _, name := range missing {
		required = append(required, name)
	}
	list := make([]any, 0, len(required))
	for _, name := range required {
		list = append(list, name)
	}
	parameters["required"] = list
}

// noReasoningEffortModels are Go models whose endpoint rejects reasoning.effort.
var noReasoningEffortModels = map[string]bool{
	"muse-spark-1.3-contributor": true,
	"muse-spark-1.2-contributor": true,
}

func normalizeReasoning(payload map[string]any, model string) {
	if !noReasoningEffortModels[model] {
		return
	}
	if reasoning, ok := payload["reasoning"].(map[string]any); ok {
		delete(reasoning, "effort")
	}
}

// rewriteReasoningEvents maps DeepSeek-style raw reasoning onto the reasoning
// summary events Codex renders. OpenAI-backed Go models already send summaries
// and are left untouched.
func rewriteReasoningEvents(payload map[string]any) {
	eventType, _ := payload["type"].(string)
	part, _ := payload["part"].(map[string]any)
	partType, _ := part["type"].(string)
	switch {
	case eventType == "response.reasoning_text.delta":
		payload["type"] = "response.reasoning_summary_text.delta"
		moveSummaryIndex(payload)
	case eventType == "response.reasoning_text.done":
		payload["type"] = "response.reasoning_summary_text.done"
		moveSummaryIndex(payload)
	case partType == "reasoning_text":
		part["type"] = "summary_text"
		moveSummaryIndex(payload)
		switch eventType {
		case "response.content_part.added":
			payload["type"] = "response.reasoning_summary_part.added"
		case "response.content_part.done":
			payload["type"] = "response.reasoning_summary_part.done"
		}
	}
	convertReasoningItem(payload["item"])
	if response, ok := payload["response"].(map[string]any); ok {
		if output, ok := response["output"].([]any); ok {
			for _, item := range output {
				convertReasoningItem(item)
			}
		}
	}
}

func moveSummaryIndex(payload map[string]any) {
	if index, ok := payload["content_index"]; ok {
		payload["summary_index"] = index
		delete(payload, "content_index")
	}
}

func convertReasoningItem(value any) {
	item, ok := value.(map[string]any)
	if !ok || item["type"] != "reasoning" {
		return
	}
	content, ok := item["content"].([]any)
	if !ok || len(content) == 0 {
		return
	}
	hasRawReasoning := false
	for _, entry := range content {
		if part, ok := entry.(map[string]any); ok && part["type"] == "reasoning_text" {
			hasRawReasoning = true
		}
	}
	if !hasRawReasoning {
		return
	}
	summary := make([]any, 0, len(content))
	for _, entry := range content {
		if part, ok := entry.(map[string]any); ok && part["type"] == "reasoning_text" {
			summary = append(summary, map[string]any{"type": "summary_text", "text": part["text"]})
		}
	}
	// Codex retains reasoning_text content and sends it back on subsequent turns.
	item["summary"] = summary
}

func opencodeSessionID(incoming http.Header) string {
	for _, name := range sessionHeaderCandidates {
		if value := incoming.Get(name); value != "" {
			return value
		}
	}
	return randomUUID()
}

func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("router-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
