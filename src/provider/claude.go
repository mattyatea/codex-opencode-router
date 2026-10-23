package provider

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync/atomic"
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

// errClaudeIncomplete means the bridge exited before reporting completion.
var errClaudeIncomplete = errors.New("Claude Agent SDK ended without completing the response")

type claudeBody struct {
	io.ReadCloser
	abort func()
}

func (b *claudeBody) Close() error {
	b.abort()
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
	// A disconnect interrupts the bridge only until it reports completion; after
	// that it keeps running so it can save the session state while the SDK exits.
	ctx, cancel := context.WithCancel(context.Background())
	stopOnDisconnect := context.AfterFunc(request.Context(), cancel)
	var detached atomic.Bool
	abort := func() {
		if !detached.Load() {
			cancel()
		}
	}
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
		state := newResponseStream(writer, model, nil)
		finish, usage, err := readClaudeEvents(stdout, state)
		if err == nil {
			detached.Store(true)
			stopOnDisconnect()
			go func() {
				defer cancel()
				if err := cmd.Wait(); err != nil {
					log.Printf("Claude Agent SDK exited with an error after completing a response: %v", err)
				}
			}()
			err = state.finish(finish, usage, "")
		} else {
			cancel()
			if waitErr := cmd.Wait(); waitErr != nil && errors.Is(err, errClaudeIncomplete) {
				err = fmt.Errorf("Claude Agent SDK process failed; check SDK installation and Claude login: %w", waitErr)
			}
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
		response.Body = &claudeBody{ReadCloser: reader, abort: abort}
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

// readClaudeEvents streams bridge deltas into state and returns at the "complete" event.
func readClaudeEvents(reader io.Reader, state *responseStream) (string, responseUsage, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxEventLineBytes)
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
			return "", responseUsage{}, fmt.Errorf("invalid Claude Agent SDK event: %w", err)
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
			return event.Finish, event.Usage, state.err
		case "error":
			return "", responseUsage{}, fmt.Errorf("Claude Agent SDK: %s", event.Message)
		default:
			return "", responseUsage{}, fmt.Errorf("unknown Claude Agent SDK event %q", event.Type)
		}
		if state.err != nil {
			return "", responseUsage{}, state.err
		}
	}
	if err := scanner.Err(); err != nil {
		return "", responseUsage{}, err
	}
	return "", responseUsage{}, errClaudeIncomplete
}
