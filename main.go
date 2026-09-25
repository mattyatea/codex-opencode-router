// Loopback relay that lets Codex use ChatGPT models and OpenCode Go models together.
//
// Codex binds one provider per process, so the desktop model picker can only
// show one catalog. This relay serves the union catalog at /models and routes
// each request by model prefix: go-* goes to OpenCode Go, everything else goes
// to the ChatGPT backend with the caller's own credentials.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	listenDefault    = "127.0.0.1:18789"
	goPrefix         = "go-"
	baseCatalogModel = "gpt-6-luna"
	chatgptOrigin    = "https://chatgpt.com"
	opencodeOrigin   = "https://opencode.ai"
	maxSSELineBytes  = 16 << 20
)

// goModel describes an OpenCode Go model that answers on the Responses API.
// Other Go models are chat-completions-only or are not used here.
// ContextWindow is the model's real context length in tokens; Codex uses it
// for the context meter and auto-compaction. Set it per model because the
// cloned ChatGPT catalog entry would otherwise inherit the GPT-6 Luna size.
type goModel struct {
	ID               string
	Name             string
	ClearToolMode    bool
	ContextWindow    int
	MaxContextWindow int
}

var goModels = []goModel{
	{ID: "deepseek-v4.1-flash", Name: "Go/DeepSeek V4.1 Flash", ClearToolMode: true, ContextWindow: 1_000_000},
	{ID: "gpt-6-luna", Name: "Go/GPT-6 Luna", ContextWindow: 1_000_000},
	{ID: "muse-spark-1.3-contributor", Name: "Go/Muse Spark 1.3 (Train)", ClearToolMode: true, ContextWindow: 1_000_000},
}

// sessionHeaderCandidates are checked in order for a stable per-conversation id.
var sessionHeaderCandidates = []string{
	"x-opencode-session", "session-id", "x-codex-session", "thread-id",
	"x-codex-thread-id", "session_id", "x-codex-conversation-id",
}

// goRequestHeaders are the Codex protocol headers worth forwarding to OpenCode
// Go. Credentials and account headers are deliberately not forwarded.
var goRequestHeaders = []string{
	"Content-Type", "Accept", "User-Agent", "Originator", "Session-Id",
	"Thread-Id", "X-Client-Request-Id", "OpenAI-Beta", "Idempotency-Key",
}

var hopByHopHeaders = map[string]bool{
	"connection": true, "transfer-encoding": true, "content-length": true,
	"content-encoding": true, "host": true, "authorization": true,
}

var (
	goKey    string
	upstream = &http.Client{Timeout: 0}
)

func main() {
	env := loadEnv(defaultEnvPath())
	goKey = env["OPENCODE_GO_API_KEY"]
	if goKey == "" {
		goKey = os.Getenv("OPENCODE_GO_API_KEY")
	}
	if goKey == "" {
		log.Fatal("OPENCODE_GO_API_KEY is missing from the Codex .env file and the process environment")
	}
	listenAddr := os.Getenv("ROUTER_LISTEN")
	if listenAddr == "" {
		listenAddr = listenDefault
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/models", handleModels)
	mux.HandleFunc("/responses", handleResponses)
	serveAPI()
	log.Printf("codex-go-router listening on %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}

// defaultEnvPath mirrors Codex's own CODEX_HOME convention so the router reads
// the same .env file as the Codex installation on this machine.
func defaultEnvPath() string {
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return filepath.Join(codexHome, ".env")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".codex", ".env")
	}
	return ".env"
}

func loadEnv(path string) map[string]string {
	env := map[string]string{}
	file, err := os.Open(path)
	if err != nil {
		log.Printf("no env file at %s; falling back to the process environment", path)
		return env
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		value, ok := strings.CutPrefix(line, "export ")
		if !ok {
			continue
		}
		key, val, found := strings.Cut(value, "=")
		if found {
			env[key] = val
		}
	}
	return env
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	authorization := r.Header.Get("Authorization")
	if authorization == "" {
		http.Error(w, "missing authorization", http.StatusUnauthorized)
		return
	}
	request, err := http.NewRequestWithContext(
		r.Context(), http.MethodGet, chatgptOrigin+"/backend-api/codex"+r.URL.RequestURI(), nil,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	request.Header.Set("Authorization", authorization)
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
	for _, spec := range goModels {
		clone, err := cloneMap(base)
		if err != nil {
			continue
		}
		clone["slug"] = goPrefix + spec.ID
		clone["display_name"] = spec.Name
		clone["description"] = "OpenCode Go subscription model via the Responses API."
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

// stripForeignReasoning removes reasoning items produced by the other backend.
// ChatGPT reasoning items carry ids like "rs_..." and encrypted content that
// starts with "gAAAA"; OpenCode Go (DeepSeek) uses UUIDs. Sending one
// provider's encrypted reasoning to the other fails verification, so each
// direction keeps only its own items.
func stripForeignReasoning(payload map[string]any, forGo bool) {
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
		if isChatGPTReasoning(item) != forGo {
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
	isGo := strings.HasPrefix(model, goPrefix)
	stripForeignReasoning(payload, isGo)

	origin, path := chatgptOrigin, "/backend-api/codex/responses"
	headers := http.Header{}
	if isGo {
		payload["model"] = strings.TrimPrefix(model, goPrefix)
		body, _ = json.Marshal(payload)
		origin, path = opencodeOrigin, "/zen/go/v1/responses"
		headers.Set("Authorization", "Bearer "+goKey)
		headers.Set("x-opencode-session", opencodeSessionID(r.Header))
		for _, name := range goRequestHeaders {
			if value := r.Header.Get(name); value != "" {
				headers.Set(name, value)
			}
		}
	} else {
		authorization := r.Header.Get("Authorization")
		if authorization == "" {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		body, _ = json.Marshal(payload)
		headers.Set("Authorization", authorization)
		for name, values := range r.Header {
			if hopByHopHeaders[strings.ToLower(name)] {
				continue
			}
			for _, value := range values {
				headers.Add(name, value)
			}
		}
	}
	headers.Set("Accept-Encoding", "identity")

	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, origin+path, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	request.Header = headers
	if os.Getenv("ROUTER_DEBUG_HEADERS") == "1" {
		if isGo {
			log.Printf("go request model=%s", model)
		} else {
			instructions, _ := payload["instructions"].(string)
			last := lastInputText(payload["input"])
			log.Printf("chatgpt request model=%s thread=%s instructions=%q last_input=%q",
				model, r.Header.Get("Thread-Id"), truncate(instructions, 200), truncate(last, 200))
		}
	}
	response, err := upstream.Do(request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	log.Printf("%s %s model=%s go=%t -> %d", r.Method, r.URL.Path, model, isGo, response.StatusCode)
	if response.StatusCode >= http.StatusBadRequest {
		errorBody, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		log.Printf("upstream error body: %s", strings.TrimSpace(string(errorBody)))
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(response.StatusCode)
		w.Write(errorBody)
		return
	}

	for name, values := range response.Header {
		if hopByHopHeaders[strings.ToLower(name)] {
			continue
		}
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)

	flusher, _ := w.(http.Flusher)
	if isGo && response.StatusCode < http.StatusBadRequest {
		relayGoStream(w, response.Body, flusher)
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

func relayGoStream(w io.Writer, body io.Reader, flusher http.Flusher) {
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
		rewriteReasoningEvents(payload)
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
	for _, entry := range content {
		if part, ok := entry.(map[string]any); ok {
			part["type"] = "summary_text"
		}
	}
	item["content"] = []any{}
	item["summary"] = content
}

func opencodeSessionID(incoming http.Header) string {
	for _, name := range sessionHeaderCandidates {
		if value := incoming.Get(name); value != "" {
			return value
		}
	}
	return randomUUID()
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

func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("router-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
