package provider

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"
)

//go:embed claude_agent.py
var claudeAgentBridge string

func ClaudeAuth(python, action string) error {
	cmd := exec.Command(python, "-u", "-c", claudeAgentBridge, "--auth", action)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// sessionRoot stores SDK session IDs per Codex thread so later turns can resume.
func ClaudeAgent(python, sessionRoot string) Provider {
	p := Provider{ID: "claude", Name: "Claude Agent SDK", Prefix: "claude/", BaseURL: "http://claude-agent.local"}
	p.RoundTrip = func(_ *http.Client, request *http.Request) (*http.Response, error) {
		return claudeRoundTrip(python, sessionRoot, request)
	}
	return p
}

// Resolve aliases once at startup so the catalog and requests use exact IDs.
func DiscoverClaudeModels(python string) ([]Model, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-u", "-c", claudeAgentBridge, "--models")
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("discover Claude models (check SDK installation): %w", err)
	}
	var models []Model
	if err := json.Unmarshal(data, &models); err != nil {
		return nil, fmt.Errorf("decode Claude models: %w", err)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("Claude SDK returned no models")
	}
	return models, nil
}

type claudeBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *claudeBody) Close() error {
	b.cancel()
	return b.ReadCloser.Close()
}

func claudeRoundTrip(python, sessionRoot string, request *http.Request) (*http.Response, error) {
	defer request.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		return nil, err
	}
	// Session identity comes from Codex headers, never a client-supplied path.
	payload["_thread_id"] = request.Header.Get("Thread-Id")
	if payload["_thread_id"] == "" {
		payload["_thread_id"] = request.Header.Get("Session-Id")
	}
	payload["_session_root"] = sessionRoot
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(request.Context())
	cmd := exec.CommandContext(ctx, python, "-u", "-c", claudeAgentBridge)
	cmd.Stdin = bytes.NewReader(encoded)
	// Give the SDK time to close its own subprocess on disconnect.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start Claude Agent SDK: %w", err)
	}
	model, _ := payload["model"].(string)
	stream, _ := payload["stream"].(bool)
	run := func(writer io.Writer) ([]byte, error) {
		defer cancel()
		state := newResponseStream(writer, model, nil)
		err := readClaudeEvents(stdout, state)
		if err != nil {
			cancel()
		}
		waitErr := cmd.Wait()
		if err == nil && waitErr != nil {
			err = fmt.Errorf("Claude Agent SDK process failed; check SDK installation and Claude login: %w", waitErr)
		}
		if err != nil {
			state.response["status"] = "failed"
			state.response["error"] = map[string]any{"code": "claude_agent_error", "message": err.Error()}
			state.emit("response.failed", map[string]any{"response": state.response})
			return nil, err
		}
		return json.Marshal(state.response)
	}
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, ContentLength: -1}
	if stream {
		reader, writer := io.Pipe()
		response.Header.Set("Content-Type", "text/event-stream")
		response.Body = &claudeBody{ReadCloser: reader, cancel: cancel}
		go func() { _, err := run(writer); writer.CloseWithError(err) }()
	} else {
		body, err := run(nil)
		if err != nil {
			return nil, err
		}
		response.Header.Set("Content-Type", "application/json")
		response.Body = io.NopCloser(bytes.NewReader(body))
	}
	return response, nil
}

func readClaudeEvents(reader io.Reader, state *responseStream) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxEventLineBytes)
	var finish string
	var usage responseUsage
	for scanner.Scan() {
		var event struct {
			Type    string        `json:"type"`
			Delta   responseDelta `json:"delta"`
			Custom  bool          `json:"custom"`
			Finish  string        `json:"finish"`
			Usage   responseUsage `json:"usage"`
			Message string        `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return fmt.Errorf("invalid Claude Agent SDK event: %w", err)
		}
		switch event.Type {
		case "delta":
			if event.Custom {
				if state.customTools == nil {
					state.customTools = map[string]bool{}
				}
				for _, call := range event.Delta.ToolCalls {
					state.customTools[call.Function.Name] = true
				}
			}
			state.appendDelta(event.Delta)
		case "complete":
			finish, usage = event.Finish, event.Usage
		case "error":
			return fmt.Errorf("Claude Agent SDK: %s", event.Message)
		default:
			return fmt.Errorf("unknown Claude Agent SDK event %q", event.Type)
		}
		if state.err != nil {
			return state.err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return state.finish(finish, usage, "")
}
