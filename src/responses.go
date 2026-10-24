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

const maxSSELineBytes = 16 << 20

// providerRequestHeaders exclude ChatGPT credentials and account headers.
var providerRequestHeaders = []string{
	"Content-Type", "Accept", "User-Agent", "Originator", "Session-Id",
	"Thread-Id", "X-Client-Request-Id", "OpenAI-Beta", "Idempotency-Key",
}

var hopByHopHeaders = map[string]bool{
	"connection": true, "transfer-encoding": true, "content-length": true,
	"content-encoding": true, "host": true, "authorization": true,
}

func (rt *router) handleResponses(w http.ResponseWriter, r *http.Request) {
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
	p := rt.providerForModel(model)
	destination := ""
	if p != nil {
		destination = p.ID
	}
	stripForeignReasoning(payload, destination)

	var endpoint string
	var headers http.Header
	if p != nil {
		endpoint, headers = prepareProviderRequest(p, payload, r.Header)
	} else if endpoint, headers, err = prepareChatGPTRequest(r.Header); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
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
		logRequest(model, destination, payload, r.Header)
	}

	var response *http.Response
	if p != nil && p.RoundTrip != nil {
		response, err = p.RoundTrip(rt.client, request)
	} else {
		response, err = rt.client.Do(request)
	}
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

func prepareProviderRequest(p *provider.Provider, payload map[string]any, incoming http.Header) (string, http.Header) {
	payload["model"] = strings.TrimPrefix(payload["model"].(string), p.ModelPrefix())
	replaceOriginalImageDetail(payload["input"])
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

// replaceOriginalImageDetail downgrades Codex's ChatGPT-only image detail
// "original" (sent by view_image, also inside tool outputs) to "high", because
// public Responses API providers accept only auto, low, and high.
func replaceOriginalImageDetail(value any) {
	switch node := value.(type) {
	case map[string]any:
		if node["type"] == "input_image" && node["detail"] == "original" {
			node["detail"] = "high"
		}
		for _, child := range node {
			replaceOriginalImageDetail(child)
		}
	case []any:
		for _, child := range node {
			replaceOriginalImageDetail(child)
		}
	}
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

// logRequest is enabled by ROUTER_DEBUG_HEADERS=1.
func logRequest(model, destination string, payload map[string]any, incoming http.Header) {
	if destination != "" {
		tools, _ := payload["tools"].([]any)
		log.Printf("provider request model=%s provider=%s tools=%d", model, destination, len(tools))
		return
	}
	last := lastInputText(payload["input"])
	if len(last) > 200 {
		last = last[:200] + "..."
	}
	log.Printf("chatgpt request model=%s thread=%s last_input=%q", model, incoming.Get("Thread-Id"), last)
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
