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
	"time"
)

func TestClaudeAuthCommand(t *testing.T) {
	node := filepath.Join(t.TempDir(), "node")
	sdk := t.TempDir()
	// The bridge resolves the SDK from its working directory.
	script := "#!/bin/sh\n[ \"$5\" = '--auth' ] && [ \"$6\" = 'status' ] && [ \"$PWD\" = '" + sdk + "' ]\n"
	if err := os.WriteFile(node, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := ClaudeAuth(ClaudeBridge{Node: node, Dir: sdk}, "status"); err != nil {
		t.Fatalf("auth command: %v", err)
	}
}

func TestClaudeBridgeProcess(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			node := filepath.Join(t.TempDir(), "node")
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
			if err := os.WriteFile(node, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(fmt.Sprintf(`{"model":"sonnet","input":"hello","stream":%t}`, stream)))
			response, err := ClaudeAgent(ClaudeBridge{Node: node, Dir: t.TempDir()}, t.TempDir()).RoundTrip(nil, request)
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
		if _, _, err := readClaudeEvents(strings.NewReader(input), state); err == nil {
			t.Fatal("failed/truncated stream succeeded")
		}
		if strings.Contains(output.String(), "response.completed") {
			t.Fatal("failed stream emitted completion")
		}
	}
}

// The bridge keeps saving session state while the SDK shuts down; Codex must not wait for that.
func TestClaudeRespondsBeforeBridgeExits(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			node := filepath.Join(t.TempDir(), "node")
			script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"delta","delta":{"content":"OK"}}' '{"type":"complete","finish":"stop","usage":{}}'
sleep 3
`
			if err := os.WriteFile(node, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			request := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(fmt.Sprintf(`{"model":"sonnet","input":"hello","stream":%t}`, stream)))
			response, err := ClaudeAgent(ClaudeBridge{Node: node, Dir: t.TempDir()}, t.TempDir()).RoundTrip(nil, request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || !strings.Contains(string(body), `"status":"completed"`) {
				t.Fatalf("response = %s, err = %v", body, err)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("response waited %v for the bridge to exit", elapsed)
			}
		})
	}
}

func TestClaudeProcessFailureReportsCause(t *testing.T) {
	node := filepath.Join(t.TempDir(), "node")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"error","message":"authentication_failed: run codex-go-router claude login"}'
exit 1
`
	if err := os.WriteFile(node, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"sonnet","input":"hello","stream":true}`))
	response, err := ClaudeAgent(ClaudeBridge{Node: node, Dir: t.TempDir()}, t.TempDir()).RoundTrip(nil, request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(response.Body)
	if readErr == nil {
		t.Fatal("SDK failure should propagate to the transport")
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			Response struct {
				Status string `json:"status"`
				Error  struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"response"`
		}
		if err := json.Unmarshal(line[6:], &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "response.failed" && event.Response.Status == "failed" && strings.Contains(event.Response.Error.Message, "claude login") {
			return
		}
	}
	t.Fatalf("missing actionable response.failed: %s", body)
}

func TestClaudeSessionLock(t *testing.T) {
	root := t.TempDir()
	dir, unlock, err := lockClaudeSession(root, "thread", "claude-sonnet-5-5")
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range [][2]string{{"other", "claude-sonnet-5-5"}, {"thread", "claude-opus-5-5"}} {
		otherDir, otherUnlock, err := lockClaudeSession(root, other[0], other[1])
		if err != nil || otherDir == dir {
			t.Fatalf("session %v shares %s: %v", other, dir, err)
		}
		otherUnlock()
	}
	acquired := make(chan struct{})
	go func() {
		_, unlock, err := lockClaudeSession(root, "thread", "claude-sonnet-5-5")
		if err == nil {
			unlock()
		}
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("same session was not serialized")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	<-acquired
	if dir, _, _ := lockClaudeSession(root, "", "claude-sonnet-5-5"); dir != "" {
		t.Fatal("requests without a thread must not persist sessions")
	}
}
