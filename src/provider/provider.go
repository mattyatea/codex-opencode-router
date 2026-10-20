package provider

import "net/http"

// Context windows override the ChatGPT catalog template used by Codex.
type Model struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	ClearToolMode    bool   `json:"clear_tool_mode"`
	ContextWindow    int    `json:"context_window"`
	MaxContextWindow int    `json:"max_context_window"`
}

// Provider describes a Responses API endpoint and its optional protocol adjustments.
type Provider struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	BaseURL   string  `json:"base_url"`
	APIKeyEnv string  `json:"api_key_env"`
	Models    []Model `json:"models"`
	APIKey    string  `json:"-"`

	// Nil callbacks leave the corresponding payload unchanged.
	PrepareRequest  func(payload map[string]any, incoming, headers http.Header) `json:"-"`
	RewriteEvent    func(event map[string]any)                                  `json:"-"`
	RewriteResponse func(response map[string]any)                               `json:"-"`
}
