package main

import (
	"bufio"
	"bytes"
	"codex-go-router/src/provider"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

// The router serves the union catalog and forwards requests by model prefix.
const (
	baseCatalogModel = "gpt-6-luna"
	maxSSELineBytes  = 16 << 20
)

var providers = []provider.Provider{provider.OpenCode()}
var upstream = &http.Client{Timeout: 0}

func providerForModel(model string) *provider.Provider {
	for i := range providers {
		if strings.HasPrefix(model, providers[i].ID+"-") {
			return &providers[i]
		}
	}
	return nil
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	authorization := r.Header.Get("Authorization")
	if authorization == "" {
		http.Error(w, "missing authorization", http.StatusUnauthorized)
		return
	}
	request, err := chatgptCatalogRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	response, err := upstream.Do(request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if response.StatusCode == http.StatusOK {
		var catalog map[string]any
		if json.Unmarshal(body, &catalog) == nil {
			if merged, err := buildUnionCatalog(catalog); err == nil {
				body = merged
			}
		}
	}
	log.Printf("GET /models -> %d", response.StatusCode)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	w.Write(body)
}

func buildUnionCatalog(catalog map[string]any) ([]byte, error) {
	models, _ := catalog["models"].([]any)
	if len(models) == 0 {
		return json.Marshal(catalog)
	}
	base := findModel(models, baseCatalogModel)
	if base == nil {
		base, _ = models[0].(map[string]any)
	}
	if base == nil {
		return json.Marshal(catalog)
	}
	for _, p := range providers {
		for _, spec := range p.Models {
			clone, err := cloneMap(base)
			if err != nil {
				continue
			}
			clone["slug"] = p.ID + "-" + spec.ID
			clone["display_name"] = spec.Name
			if spec.Name == "" {
				clone["display_name"] = p.Name + "/" + spec.ID
			}
			clone["description"] = p.Name + " model via the Responses API."
			clone["default_reasoning_summary"] = "auto"
			clone["service_tiers"] = []any{}
			clone["additional_speed_tiers"] = []any{}
			clone["use_responses_lite"] = false
			clone["multi_agent_version"] = nil
			if spec.ContextWindow > 0 {
				clone["context_window"] = spec.ContextWindow
				maxWindow := spec.ContextWindow
				if spec.MaxContextWindow > 0 {
					maxWindow = spec.MaxContextWindow
				}
				clone["max_context_window"] = maxWindow
			}
			if spec.ClearToolMode {
				clone["tool_mode"] = nil
			}
			if priority, ok := clone["priority"].(float64); ok {
				clone["priority"] = priority + 20
			}
			models = append(models, clone)
		}
	}
	catalog["models"] = models
	return json.Marshal(catalog)
}

func findModel(models []any, slug string) map[string]any {
	for _, entry := range models {
		if model, ok := entry.(map[string]any); ok && model["slug"] == slug {
			return model
		}
	}
	return nil
}

func cloneMap(source map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	var clone map[string]any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		return nil, err
	}
	return clone, nil
}

func handleResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	model, _ := payload["model"].(string)
	p := providerForModel(model)
	destination := ""
	if p != nil {
		destination = p.ID
	}
	stripForeignReasoning(payload, destination)

	endpoint := chatgptOrigin + "/backend-api/codex/responses"
	headers := http.Header{}
	if p != nil {
		endpoint, headers = prepareProviderRequest(p, payload, r.Header)
	} else {
		headers, err = chatgptHeaders(r.Header)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
	}
	body, _ = json.Marshal(payload)
	headers.Set("Accept-Encoding", "identity")

	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	request.Header = headers
	if os.Getenv("ROUTER_DEBUG_HEADERS") == "1" {
		if p != nil {
			tools, _ := payload["tools"].([]any)
			log.Printf("provider request model=%s provider=%s tools=%d", model, destination, len(tools))
		} else {
			last := lastInputText(payload["input"])
			log.Printf("chatgpt request model=%s thread=%s last_input=%q",
				model, r.Header.Get("Thread-Id"), truncate(last, 200))
		}
	}
	response, err := upstream.Do(request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	log.Printf("%s %s model=%s provider=%s -> %d", r.Method, r.URL.Path, model, destination, response.StatusCode)
	if response.StatusCode >= http.StatusBadRequest {
		errorBody, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		log.Printf("upstream error body: %s", strings.TrimSpace(string(errorBody)))
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(response.StatusCode)
		w.Write(errorBody)
		return
	}

	if p != nil {
		if err := rewriteProviderResponse(p, response); err != nil {
			http.Error(w, "invalid provider JSON response", http.StatusBadGateway)
			return
		}
	}

	copyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)

	flusher, _ := w.(http.Flusher)
	if p != nil && strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		relayProviderStream(w, response.Body, flusher, p)
	} else {
		relay(w, response.Body, flusher)
	}
}

func relay(w io.Writer, body io.Reader, flusher http.Flusher) {
	buffer := make([]byte, 32<<10)
	for {
		n, err := body.Read(buffer)
		if n > 0 {
			w.Write(buffer[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func relayProviderStream(w io.Writer, body io.Reader, flusher http.Flusher, p *provider.Provider) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), maxSSELineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.HasPrefix(line, []byte("event: ")) {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data: ")) {
			if len(bytes.TrimSpace(line)) > 0 {
				w.Write(append(append([]byte{}, line...), '\n'))
			}
			continue
		}
		var payload map[string]any
		if json.Unmarshal(line[len("data: "):], &payload) != nil {
			w.Write(append(append([]byte{}, line...), '\n'))
			continue
		}
		if p.RewriteEvent != nil {
			p.RewriteEvent(payload)
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", payload["type"], encoded)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func lastInputText(input any) string {
	items, ok := input.([]any)
	if !ok {
		return ""
	}
	for i := len(items) - 1; i >= 0; i-- {
		item, ok := items[i].(map[string]any)
		if !ok || item["role"] != "user" {
			continue
		}
		switch content := item["content"].(type) {
		case string:
			return content
		case []any:
			for _, part := range content {
				if p, ok := part.(map[string]any); ok && p["type"] == "input_text" {
					if text, ok := p["text"].(string); ok {
						return text
					}
				}
			}
		}
	}
	return ""
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
