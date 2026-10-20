package main

import (
	"bytes"
	"codex-go-router/src/provider"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestInceptionKeyAndCatalog(t *testing.T) {
	t.Setenv("OPENCODE_GO_API_KEY", "")
	t.Setenv("ROUTER_CONFIG", "")
	for _, source := range []string{"missing", "file", "process"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("INCEPTION_API_KEY", "")
			env := map[string]string{}
			if source == "file" {
				env["INCEPTION_API_KEY"] = "file-key"
				t.Setenv("INCEPTION_API_KEY", "process-key")
			} else if source == "process" {
				t.Setenv("INCEPTION_API_KEY", "process-key")
			}
			loaded, err := loadProviders(filepath.Join(t.TempDir(), "router.json"), env)
			if err != nil {
				t.Fatal(err)
			}
			original := providers
			providers = loaded
			t.Cleanup(func() { providers = original })
			encoded, err := buildUnionCatalog(map[string]any{"models": []any{map[string]any{"slug": baseCatalogModel, "input_modalities": []any{"text", "image"}, "tool_mode": "code_mode_only"}}})
			if err != nil {
				t.Fatal(err)
			}
			var catalog map[string]any
			if err := json.Unmarshal(encoded, &catalog); err != nil {
				t.Fatal(err)
			}
			model := findModel(catalog["models"].([]any), "inception-labs/mercury-3")
			if source == "missing" {
				if len(loaded) != 0 || model != nil {
					t.Fatal("Inception enabled without an API key")
				}
				return
			}
			key := "process-key"
			if source == "file" {
				key = "file-key"
			}
			if len(loaded) != 1 || loaded[0].APIKey != key {
				t.Fatal("Inception key source or precedence is incorrect")
			}
			if model == nil || model["context_window"] != float64(1_000_000) || model["tool_mode"] != nil {
				t.Fatalf("unexpected Mercury catalog entry: %v", model)
			}
			modalities := model["input_modalities"].([]any)
			if len(modalities) != 1 || modalities[0] != "text" || len(model["supported_reasoning_levels"].([]any)) != 4 {
				t.Fatalf("unexpected Mercury capabilities: %v", model)
			}
		})
	}
}

func TestInceptionToolRoundTrip(t *testing.T) {
	t.Setenv("OPENCODE_GO_API_KEY", "")
	t.Setenv("ROUTER_CONFIG", "")
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			turn := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				turn++
				if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer inception-key" {
					t.Error("wrong Inception endpoint or credentials")
				}
				for _, name := range []string{"ChatGPT-Account-Id", "Cookie", "X-Opencode-Session"} {
					if r.Header.Get(name) != "" {
						t.Errorf("leaked header %s", name)
					}
				}
				var chat map[string]any
				if err := json.NewDecoder(r.Body).Decode(&chat); err != nil {
					t.Error(err)
					return
				}
				if chat["model"] != "mercury-3" || chat["reasoning_effort"] != "high" || chat["max_completion_tokens"] != float64(1024) {
					t.Errorf("incorrect request translation: %v", chat)
				}
				if _, ok := chat["input"]; ok {
					t.Error("Responses payload leaked to Chat Completions")
				}
				messages := chat["messages"].([]any)
				if messages[0].(map[string]any)["content"] != "instructions" || messages[1].(map[string]any)["role"] != "system" {
					t.Errorf("instructions or developer message lost: %v", messages)
				}
				tools := chat["tools"].([]any)
				if len(tools) != 2 || tools[1].(map[string]any)["function"].(map[string]any)["name"] != "apply_patch" {
					t.Errorf("incorrect tool definitions: %v", tools)
				}
				var message map[string]any
				reason := "stop"
				if turn == 1 {
					message = map[string]any{"content": "Checking.", "tool_calls": []any{
						map[string]any{"index": 0, "id": "call_a", "type": "function", "function": map[string]any{"name": "search", "arguments": `{"query":"x"}`}},
						map[string]any{"index": 1, "id": "call_b", "type": "function", "function": map[string]any{"name": "apply_patch", "arguments": `{"input":"patch\n"}`}},
					}}
					reason = "tool_calls"
				} else {
					assistant := messages[3].(map[string]any)
					calls := assistant["tool_calls"].([]any)
					if len(calls) != 2 || calls[1].(map[string]any)["function"].(map[string]any)["arguments"] != `{"input":"patch\n"}` {
						t.Errorf("tool calls not restored: %v", assistant)
					}
					for i, id := range []string{"call_a", "call_b"} {
						result := messages[4+i].(map[string]any)
						if result["role"] != "tool" || result["tool_call_id"] != id || result["content"] != "done" {
							t.Errorf("tool output not restored: %v", result)
						}
					}
					message = map[string]any{"content": "OK"}
				}
				usage := map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "prompt_tokens_details": map[string]any{"cached_tokens": 3}, "completion_tokens_details": map[string]any{"reasoning_tokens": 2}}
				summary := map[string]any{"content": ""}
				if turn == 2 {
					summary["content"] = "Checked."
				}
				if !stream {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": reason}}, "usage": usage, "reasoning_summary": summary})
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(chunk map[string]any) {
					encoded, _ := json.Marshal(chunk)
					fmt.Fprintf(w, "data: %s\n\n", encoded)
				}
				// Arguments can arrive in separate chunks for parallel calls.
				if turn == 1 {
					calls := message["tool_calls"].([]any)
					calls[0].(map[string]any)["function"].(map[string]any)["arguments"] = `{"query":`
					send(map[string]any{"choices": []any{map[string]any{"delta": message}}})
					send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": `"x"}`}}}}, "finish_reason": reason}}})
				} else {
					send(map[string]any{"choices": []any{map[string]any{"delta": message, "finish_reason": reason}}})
				}
				send(map[string]any{"choices": []any{}, "usage": usage, "reasoning_summary": summary})
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer server.Close()
			loaded, err := loadProviders(filepath.Join(t.TempDir(), "router.json"), map[string]string{"INCEPTION_API_KEY": "inception-key"})
			if err != nil || len(loaded) != 1 {
				t.Fatalf("load Inception: count=%d, err=%v", len(loaded), err)
			}
			p := loaded[0]
			p.BaseURL = server.URL + "/v1"
			original := providers
			providers = []provider.Provider{p}
			t.Cleanup(func() { providers = original })
			input := []any{map[string]any{"role": "developer", "content": "developer instructions"}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "test"}}}}
			for i := 0; i < 2; i++ {
				payload := map[string]any{
					"model": "inception-labs/mercury-3", "instructions": "instructions", "input": input, "stream": stream,
					"reasoning": map[string]any{"effort": "xhigh"}, "max_output_tokens": 1024,
					"tools": []any{map[string]any{"type": "function", "name": "search", "parameters": map[string]any{"type": "object"}}, map[string]any{"type": "custom", "name": "apply_patch"}, map[string]any{"type": "web_search"}},
				}
				body, _ := json.Marshal(payload)
				r := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(body))
				r.Header.Set("Authorization", "Bearer chatgpt-key")
				r.Header.Set("Cookie", "private")
				r.Header.Set("ChatGPT-Account-Id", "private")
				w := httptest.NewRecorder()
				handleResponses(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
				var result map[string]any
				if stream {
					for _, line := range strings.Split(w.Body.String(), "\n") {
						if data, ok := strings.CutPrefix(line, "data: "); ok {
							var event map[string]any
							if err := json.Unmarshal([]byte(data), &event); err != nil {
								t.Fatal(err)
							}
							if event["type"] == "response.completed" {
								result = event["response"].(map[string]any)
							}
						}
					}
				} else if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result == nil || result["status"] != "completed" {
					t.Fatalf("missing completed response: %s", w.Body.String())
				}
				usage := result["usage"].(map[string]any)
				if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(5) || usage["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(3) {
					t.Errorf("incorrect usage: %v", usage)
				}
				output := result["output"].([]any)
				if i == 0 {
					if len(output) != 3 || output[1].(map[string]any)["arguments"] != `{"query":"x"}` || output[2].(map[string]any)["type"] != "custom_tool_call" || output[2].(map[string]any)["input"] != "patch\n" {
						t.Fatalf("incorrect tool output: %v", output)
					}
					input = append(input, output...)
					input = append(input, map[string]any{"type": "function_call_output", "call_id": "call_a", "output": "done"}, map[string]any{"type": "custom_tool_call_output", "call_id": "call_b", "output": "done"})
				} else if output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "OK" {
					t.Fatalf("incorrect final message: %v", output)
				} else {
					if len(output) != 2 || output[1].(map[string]any)["type"] != "reasoning" {
						t.Fatalf("missing reasoning summary: %v", output)
					}
					for _, destination := range []string{"go", ""} {
						payload := map[string]any{"input": output}
						stripForeignReasoning(payload, destination)
						if len(payload["input"].([]any)) != 1 {
							t.Fatalf("Inception reasoning leaked to %q", destination)
						}
					}
				}
			}
		})
	}
}
