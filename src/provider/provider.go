package provider

import "net/http"

// Context windows override the ChatGPT catalog template used by Codex.
type Model struct {
	AutoCompactTokenLimit *int     `json:"auto_compact_token_limit,omitempty"`
	ID                    string   `json:"id"`
	Name                  string   `json:"name"`
	ClearToolMode         bool     `json:"clear_tool_mode"`
	ContextWindow         int      `json:"context_window"`
	MaxContextWindow      int      `json:"max_context_window"`
	InputModalities       []string `json:"input_modalities,omitempty"`
	ReasoningLevels       []string `json:"reasoning_levels,omitempty"`
}

// Provider describes an external model endpoint and its protocol adjustments.
type Provider struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	BaseURL   string  `json:"base_url"`
	APIKeyEnv string  `json:"api_key_env"`
	Models    []Model `json:"models"`
	APIKey    string  `json:"-"`
	Prefix    string  `json:"-"`

	// Nil callbacks leave the corresponding payload unchanged.
	PrepareRequest  func(payload map[string]any, incoming, headers http.Header) `json:"-"`
	RewriteEvent    func(event map[string]any)                                  `json:"-"`
	RewriteResponse func(response map[string]any)                               `json:"-"`
	// RoundTrip overrides HTTP forwarding when the provider uses a different protocol.
	RoundTrip func(client *http.Client, request *http.Request) (*http.Response, error) `json:"-"`
}

func (p Provider) ModelPrefix() string {
	if p.Prefix != "" {
		return p.Prefix
	}
	return p.ID + "-"
}
