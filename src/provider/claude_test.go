package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeAuthCommand(t *testing.T) {
	python := filepath.Join(t.TempDir(), "python")
	script := "#!/bin/sh\n[ \"$4\" = '--auth' ] && [ \"$5\" = 'status' ]\n"
	if err := os.WriteFile(python, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := ClaudeAuth(python, "status"); err != nil {
		t.Fatalf("auth command: %v", err)
	}
}

func TestClaudeBridgeProcess(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			python := filepath.Join(t.TempDir(), "python")
			// Exercise the real subprocess transport without requiring an account.
			script := `#!/bin/sh
cat >/dev/null
cat <<'EVENTS'
{"type":"delta","delta":{"content":"Checking."}}
{"type":"delta","delta":{"tool_calls":[{"index":0,"id":"call_1","namespace":"functions","function":{"name":"exec","arguments":"{\"cmd\":\"pwd\"}"}}]}}
{"type":"delta","custom":true,"delta":{"tool_calls":[{"index":1,"id":"call_2","function":{"name":"apply_patch","arguments":"{\"input\":\"patch text\"}"}}]}}
{"type":"complete","finish":"stop","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}
EVENTS
`
			if err := os.WriteFile(python, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(fmt.Sprintf(`{"model":"sonnet","input":"hello","stream":%t}`, stream)))
			response, err := ClaudeAgent(python).RoundTrip(nil, request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			var final map[string]any
			if stream {
				for _, line := range bytes.Split(body, []byte("\n")) {
					if !bytes.HasPrefix(line, []byte("data: ")) {
						continue
					}
					var event map[string]any
					if err := json.Unmarshal(line[6:], &event); err != nil {
						t.Fatal(err)
					}
					if event["type"] == "response.completed" {
						final = event["response"].(map[string]any)
					}
				}
			} else if err := json.Unmarshal(body, &final); err != nil {
				t.Fatal(err)
			}
			if final == nil || final["status"] != "completed" {
				t.Fatalf("missing completion: %s", body)
			}
			output := final["output"].([]any)
			if len(output) != 3 {
				t.Fatalf("output = %v", output)
			}
			function := output[1].(map[string]any)
			custom := output[2].(map[string]any)
			if function["call_id"] != "call_1" || function["namespace"] != "functions" || function["arguments"] != `{"cmd":"pwd"}` {
				t.Fatalf("function = %v", function)
			}
			if custom["type"] != "custom_tool_call" || custom["input"] != "patch text" {
				t.Fatalf("custom = %v", custom)
			}
		})
	}
}

func TestClaudeStreamFailure(t *testing.T) {
	for _, input := range []string{
		`{"type":"error","message":"authentication_failed"}`,
		`{"type":"delta","delta":{"content":"partial"}}`,
	} {
		var output bytes.Buffer
		state := newResponseStream(&output, "sonnet", nil)
		if err := readClaudeEvents(strings.NewReader(input), state); err == nil {
			t.Fatal("failed/truncated stream succeeded")
		}
		if strings.Contains(output.String(), "response.completed") {
			t.Fatal("failed stream emitted completion")
		}
	}
}
