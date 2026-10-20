package main

import (
	"codex-go-router/src/provider"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadProvidersAndCatalog(t *testing.T) {
	t.Setenv("ROUTER_CONFIG", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")
	t.Setenv("TEST_PROVIDER_KEY", "from-process")
	path := filepath.Join(t.TempDir(), "router.json")
	data := `{"providers":[{"id":"mine","base_url":"https://example.com/v1/","api_key_env":"TEST_PROVIDER_KEY","models":[{"id":"model-a","context_window":128000,"clear_tool_mode":true}]}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadProviders(path, map[string]string{"TEST_PROVIDER_KEY": "from-file"})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].APIKey != "from-file" || loaded[0].BaseURL != "https://example.com/v1" {
		t.Fatalf("unexpected provider config (count=%d)", len(loaded))
	}
	original := providers
	providers = loaded
	t.Cleanup(func() { providers = original })
	encoded, err := buildUnionCatalog(map[string]any{"models": []any{map[string]any{"slug": baseCatalogModel, "tool_mode": "code_mode_only"}}})
	if err != nil {
		t.Fatal(err)
	}
	var catalog map[string]any
	if err := json.Unmarshal(encoded, &catalog); err != nil {
		t.Fatal(err)
	}
	model := findModel(catalog["models"].([]any), "mine-model-a")
	if model == nil || model["context_window"] != float64(128000) || model["display_name"] != "mine/model-a" || model["tool_mode"] != nil {
		t.Fatalf("custom catalog entry = %v", model)
	}
	loaded, err = loadProviders(path, map[string]string{"OPENCODE_GO_API_KEY": "go-key"})
	if err != nil || len(loaded) != 2 || loaded[0].ID != "go" || loaded[1].APIKey != "from-process" {
		t.Fatalf("built-in plus custom load: count=%d, err=%v", len(loaded), err)
	}
}

func TestLoadProvidersRejectsInvalidConfig(t *testing.T) {
	t.Setenv("ROUTER_CONFIG", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")
	t.Setenv("MISSING_PROVIDER_KEY", "")
	for name, data := range map[string]string{
		"reserved id": `{"providers":[{"id":"go"}]}`,
		"bad URL":     `{"providers":[{"id":"mine","base_url":"file:///tmp/test"}]}`,
		"missing key": `{"providers":[{"id":"mine","base_url":"https://example.com","api_key_env":"MISSING_PROVIDER_KEY","models":[{"id":"test"}]}]}`,
		"bad JSON":    `{`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "router.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadProviders(path, nil); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestCustomProviderRoutingAndReasoningRoundTrip(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var received map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer provider-key" {
					t.Errorf("wrong endpoint or credentials")
				}
				for _, name := range []string{"ChatGPT-Account-Id", "Cookie", "X-Opencode-Session"} {
					if r.Header.Get(name) != "" {
						t.Errorf("unexpected forwarded header %s", name)
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Error(err)
				}
				if received["model"] != "model-a" {
					t.Errorf("model = %v", received["model"])
				}
				if !stream {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"output":[{"type":"reasoning","id":"rs_custom","summary":[],"encrypted_content":"original-state"}]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_custom\",\"summary\":[],\"encrypted_content\":\"original-state\"}}\n\n")
			}))
			defer server.Close()
			original := providers
			providers = []provider.Provider{withReasoningTags(provider.Provider{ID: "mine", BaseURL: server.URL + "/v1", APIKey: "provider-key"})}
			t.Cleanup(func() { providers = original })
			send := func(input []any) *httptest.ResponseRecorder {
				t.Helper()
				body, _ := json.Marshal(map[string]any{"model": "mine-model-a", "input": input})
				r := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(string(body)))
				r.Header.Set("Authorization", "Bearer chatgpt-key")
				r.Header.Set("ChatGPT-Account-Id", "account")
				r.Header.Set("Cookie", "session=private")
				w := httptest.NewRecorder()
				handleResponses(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d: %s", w.Code, w.Body.String())
				}
				return w
			}
			w := send([]any{})
			var item map[string]any
			if stream {
				var event map[string]any
				data := strings.Split(strings.Split(w.Body.String(), "data: ")[1], "\n")[0]
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					t.Fatal(err)
				}
				item = event["item"].(map[string]any)
			} else {
				var response map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				item = response["output"].([]any)[0].(map[string]any)
			}
			if !strings.HasPrefix(item["encrypted_content"].(string), "codex-router:") {
				t.Fatal("reasoning provider was not tagged")
			}
			for _, destination := range []string{"other", "go", ""} {
				payload := map[string]any{"input": []any{item}}
				stripForeignReasoning(payload, destination)
				if len(payload["input"].([]any)) != 0 {
					t.Fatalf("reasoning leaked to %q", destination)
				}
			}
			send([]any{item})
			input := received["input"].([]any)
			if len(input) != 1 || input[0].(map[string]any)["encrypted_content"] != "original-state" {
				t.Fatalf("reasoning not restored: %v", input)
			}
		})
	}
}

func TestOpenCodeRouting(t *testing.T) {
	const jsonResponse = "{\n  \"output\": [{\"type\": \"reasoning\", \"encrypted_content\": \"go-state\"}]\n}"
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/zen/go/v1/responses" || r.Header.Get("Authorization") != "Bearer go-key" {
					t.Error("wrong OpenCode endpoint or credentials")
				}
				if r.Header.Get("X-Opencode-Session") != "conversation" {
					t.Errorf("session = %q", r.Header.Get("X-Opencode-Session"))
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					return
				}
				if payload["model"] != "deepseek-v4.1-flash" {
					t.Errorf("model = %v", payload["model"])
				}
				tool := payload["tools"].([]any)[0].(map[string]any)
				parameters := tool["parameters"].(map[string]any)
				required, _ := parameters["required"].([]any)
				if len(required) != 1 || required[0] != "query" {
					t.Errorf("tool schema was not normalized: %v", parameters)
				}
				if !stream {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, jsonResponse)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: response.reasoning_text.delta\ndata: {\"type\":\"response.reasoning_text.delta\",\"content_index\":0,\"delta\":\"step\"}\n\n")
			}))
			defer server.Close()
			p := provider.OpenCode()
			p.BaseURL = server.URL + "/zen/go/v1"
			p.APIKey = "go-key"
			original := providers
			providers = []provider.Provider{p}
			t.Cleanup(func() { providers = original })
			r := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"go-deepseek-v4.1-flash","tools":[{"type":"function","parameters":{"properties":{"query":{"type":"string"}}}}]}`))
			r.Header.Set("Session-Id", "conversation")
			r.Header.Set("Authorization", "Bearer chatgpt-key")
			w := httptest.NewRecorder()
			handleResponses(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			if !stream {
				if w.Body.String() != jsonResponse {
					t.Fatalf("OpenCode JSON response was changed: %s", w.Body.String())
				}
				return
			}
			var event map[string]any
			data := strings.Split(strings.Split(w.Body.String(), "data: ")[1], "\n")[0]
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				t.Fatal(err)
			}
			if event["type"] != "response.reasoning_summary_text.delta" || event["summary_index"] != float64(0) || event["delta"] != "step" {
				t.Fatalf("reasoning event was not converted: %v", event)
			}
			if _, ok := event["content_index"]; ok {
				t.Fatal("content_index was not replaced")
			}
		})
	}
}
