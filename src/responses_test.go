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
			rt := newRouter([]provider.Provider{withReasoningTags(provider.Provider{ID: "mine", BaseURL: server.URL + "/v1", APIKey: "provider-key"})})
			send := func(input []any) *httptest.ResponseRecorder {
				t.Helper()
				body, _ := json.Marshal(map[string]any{"model": "mine-model-a", "input": input})
				r := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(string(body)))
				r.Header.Set("Authorization", "Bearer chatgpt-key")
				r.Header.Set("ChatGPT-Account-Id", "account")
				r.Header.Set("Cookie", "session=private")
				w := httptest.NewRecorder()
				rt.handleResponses(w, r)
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
			rt := newRouter([]provider.Provider{p})
			r := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"go-deepseek-v4.1-flash","tools":[{"type":"function","parameters":{"properties":{"query":{"type":"string"}}}}]}`))
			r.Header.Set("Session-Id", "conversation")
			r.Header.Set("Authorization", "Bearer chatgpt-key")
			w := httptest.NewRecorder()
			rt.handleResponses(w, r)
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
			rt := newRouter([]provider.Provider{p})
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
				rt.handleResponses(w, r)
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
