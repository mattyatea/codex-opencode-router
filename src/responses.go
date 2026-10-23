package main

import (
	"bytes"
	"codex-go-router/src/provider"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// providerRequestHeaders exclude ChatGPT credentials and account headers.
var providerRequestHeaders = []string{
	"Content-Type", "Accept", "User-Agent", "Originator", "Session-Id",
	"Thread-Id", "X-Client-Request-Id", "OpenAI-Beta", "Idempotency-Key",
}

var hopByHopHeaders = map[string]bool{
	"connection": true, "transfer-encoding": true, "content-length": true,
	"content-encoding": true, "host": true, "authorization": true,
}

func prepareProviderRequest(p *provider.Provider, payload map[string]any, incoming http.Header) (string, http.Header) {
	payload["model"] = strings.TrimPrefix(payload["model"].(string), p.ModelPrefix())
	headers := http.Header{}
	if p.APIKey != "" {
		headers.Set("Authorization", "Bearer "+p.APIKey)
	}
	if p.PrepareRequest != nil {
		p.PrepareRequest(payload, incoming, headers)
	}
	for _, name := range providerRequestHeaders {
		if value := incoming.Get(name); value != "" {
			headers.Set(name, value)
		}
	}
	return p.BaseURL + "/responses", headers
}

func rewriteProviderResponse(p *provider.Provider, response *http.Response) error {
	if p.RewriteResponse == nil || !strings.Contains(response.Header.Get("Content-Type"), "application/json") {
		return nil
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return err
	}
	p.RewriteResponse(payload)
	encoded, _ := json.Marshal(payload)
	response.Body = io.NopCloser(bytes.NewReader(encoded))
	return nil
}

// copyHeaders omits transport headers and credentials supplied separately.
func copyHeaders(destination, source http.Header) {
	for name, values := range source {
		if hopByHopHeaders[strings.ToLower(name)] {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}
