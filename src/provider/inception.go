package provider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Mercury 3 exposes Chat Completions, so the adapter translates at the HTTP boundary.
// https://docs.inceptionlabs.ai/get-started/models
func Inception() Provider {
	return Provider{
		ID: "inception-labs", Name: "Inception Labs",
		Prefix: "inception-labs/", BaseURL: "https://api.inceptionlabs.ai/v1",
		APIKeyEnv: "INCEPTION_API_KEY", RoundTrip: inceptionRoundTrip,
		Models: []Model{{
			ID: "mercury-3", Name: "Inception Labs/Mercury 3", ClearToolMode: true,
			ContextWindow: 1_000_000, InputModalities: []string{"text"},
			ReasoningLevels: []string{"minimal", "low", "medium", "high"},
		}},
	}
}

func inceptionRoundTrip(client *http.Client, request *http.Request) (*http.Response, error) {
	var payload map[string]any
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		request.Body.Close()
		return nil, err
	}
	request.Body.Close()
	chat, customTools, err := inceptionRequest(payload)
	if err != nil {
		return nil, fmt.Errorf("inception request: %w", err)
	}
	encoded, err := json.Marshal(chat)
	if err != nil {
		return nil, err
	}
	endpoint := *request.URL
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/responses") + "/chat/completions"
	out, err := http.NewRequestWithContext(request.Context(), request.Method, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	out.Header = request.Header.Clone()
	out.Header.Set("Content-Type", "application/json")
	response, err := client.Do(out)
	if err != nil || response.StatusCode >= http.StatusBadRequest {
		return response, err
	}
	model, _ := chat["model"].(string)
	if stream, _ := chat["stream"].(bool); stream {
		body := response.Body
		reader, writer := io.Pipe()
		response.Body = reader
		response.Header.Set("Content-Type", "text/event-stream")
		go func() {
			defer body.Close()
			err := inceptionStream(writer, body, model, customTools)
			writer.CloseWithError(err)
		}()
	} else {
		defer response.Body.Close()
		var completion inceptionCompletion
		if err := json.NewDecoder(response.Body).Decode(&completion); err != nil {
			return nil, fmt.Errorf("inception response: %w", err)
		}
		state := newResponseStream(nil, model, customTools)
		if len(completion.Choices) == 0 {
			return nil, fmt.Errorf("inception response has no choices")
		}
		choice := completion.Choices[0]
		for i := range choice.Message.ToolCalls {
			choice.Message.ToolCalls[i].Index = i
		}
		state.appendDelta(choice.Message)
		if err := state.finish(choice.FinishReason, completion.Usage, completion.ReasoningSummary.Content); err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(state.response)
		if err != nil {
			return nil, err
		}
		response.Body = io.NopCloser(bytes.NewReader(encoded))
		response.Header.Set("Content-Type", "application/json")
	}
	response.Header.Del("Content-Length")
	response.ContentLength = -1
	return response, nil
}

func inceptionRequest(payload map[string]any) (map[string]any, map[string]bool, error) {
	messages := []any{}
	if instructions, _ := payload["instructions"].(string); instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	input, _ := payload["input"].([]any)
	if text, ok := payload["input"].(string); ok {
		input = []any{map[string]any{"role": "user", "content": text}}
	}
	for _, entry := range input {
		item, ok := entry.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("unsupported input item")
		}
		switch item["type"] {
		case "reasoning":
			continue
		case "function_call", "custom_tool_call":
			arguments := item["arguments"]
			if item["type"] == "custom_tool_call" {
				encoded, _ := json.Marshal(map[string]any{"input": item["input"]})
				arguments = string(encoded)
			}
			call := map[string]any{"id": item["call_id"], "type": "function", "function": map[string]any{"name": item["name"], "arguments": arguments}}
			var assistant map[string]any
			if len(messages) > 0 {
				last := messages[len(messages)-1].(map[string]any)
				if last["role"] == "assistant" {
					assistant = last
				}
			}
			if assistant == nil {
				assistant = map[string]any{"role": "assistant", "content": nil}
				messages = append(messages, assistant)
			}
			calls, _ := assistant["tool_calls"].([]any)
			assistant["tool_calls"] = append(calls, call)
		case "function_call_output", "custom_tool_call_output":
			content, err := inceptionText(item["output"])
			if err != nil {
				return nil, nil, err
			}
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": item["call_id"], "content": content})
		case nil, "message":
			content, err := inceptionText(item["content"])
			if err != nil {
				return nil, nil, err
			}
			role, _ := item["role"].(string)
			if role == "developer" {
				role = "system"
			}
			messages = append(messages, map[string]any{"role": role, "content": content})
		default:
			return nil, nil, fmt.Errorf("unsupported input type %v", item["type"])
		}
	}
	chat := map[string]any{"model": payload["model"], "messages": messages}
	for _, key := range []string{"stream", "temperature", "tool_choice"} {
		if value, ok := payload[key]; ok {
			chat[key] = value
		}
	}
	if value, ok := payload["max_output_tokens"]; ok {
		chat["max_completion_tokens"] = value
	}
	if reasoning, ok := payload["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			switch effort {
			case "none", "minimal":
				effort = "instant"
			case "xhigh", "max":
				effort = "high"
			}
			chat["reasoning_effort"] = effort
		}
		if summary, _ := reasoning["summary"].(string); summary != "" && summary != "none" {
			chat["reasoning_summary"] = true
		}
	}
	if stream, _ := chat["stream"].(bool); stream {
		chat["stream_options"] = map[string]any{"include_usage": true}
	}
	customTools := map[string]bool{}
	tools := []any{}
	definitions, _ := payload["tools"].([]any)
	for _, entry := range definitions {
		tool, _ := entry.(map[string]any)
		if tool["type"] != "function" && tool["type"] != "custom" {
			continue
		}
		function := map[string]any{"name": tool["name"]}
		for _, key := range []string{"description", "parameters", "strict"} {
			if value, ok := tool[key]; ok {
				function[key] = value
			}
		}
		if tool["type"] == "custom" {
			name, _ := tool["name"].(string)
			customTools[name] = true
			function["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []string{"input"}, "additionalProperties": false}
		}
		tools = append(tools, map[string]any{"type": "function", "function": function})
	}
	if len(tools) > 0 {
		chat["tools"] = tools
	}
	if choice, ok := chat["tool_choice"].(map[string]any); ok {
		chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": choice["name"]}}
	}
	if text, ok := payload["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			if format["type"] == "json_schema" {
				schema := map[string]any{}
				for key, value := range format {
					if key != "type" {
						schema[key] = value
					}
				}
				chat["response_format"] = map[string]any{"type": "json_schema", "json_schema": schema}
			} else {
				chat["response_format"] = format
			}
		}
	}
	return chat, customTools, nil
}

func inceptionText(value any) (string, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	parts, ok := value.([]any)
	if !ok {
		return "", fmt.Errorf("Mercury 3 requires text content")
	}
	var text strings.Builder
	for _, entry := range parts {
		part, _ := entry.(map[string]any)
		if part["type"] != "input_text" && part["type"] != "output_text" && part["type"] != "text" {
			return "", fmt.Errorf("Mercury 3 supports text only; unsupported content %v", part["type"])
		}
		value, _ := part["text"].(string)
		text.WriteString(value)
	}
	return text.String(), nil
}

type inceptionCompletion struct {
	Choices []struct {
		Delta        responseDelta `json:"delta"`
		Message      responseDelta `json:"message"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
	Usage            responseUsage `json:"usage"`
	ReasoningSummary struct {
		Content string `json:"content"`
	} `json:"reasoning_summary"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func inceptionStream(writer io.Writer, body io.Reader, model string, customTools map[string]bool) (err error) {
	s := newResponseStream(writer, model, customTools)
	defer func() {
		if err != nil {
			s.emit("error", map[string]any{"code": "inception_stream_error", "message": err.Error()})
		}
	}()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), maxEventLineBytes)
	var usage responseUsage
	var reason, summary string
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var chunk inceptionCompletion
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("invalid inception stream: %w", err)
		}
		if chunk.Error != nil {
			return fmt.Errorf("inception stream: %s", chunk.Error.Message)
		}
		if chunk.Usage.TotalTokens != 0 {
			usage = chunk.Usage
		}
		if chunk.ReasoningSummary.Content != "" {
			summary = chunk.ReasoningSummary.Content
		}
		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
			s.appendDelta(choice.Delta)
			if choice.FinishReason != "" {
				reason = choice.FinishReason
			}
		}
		if s.err != nil {
			return s.err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return s.finish(reason, usage, summary)
}
