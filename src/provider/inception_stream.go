package provider

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

type inceptionToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type inceptionDelta struct {
	Content   string              `json:"content"`
	ToolCalls []inceptionToolCall `json:"tool_calls"`
}

type inceptionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"cached_input_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
	PromptDetails    struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type inceptionCompletion struct {
	Choices []struct {
		Delta        inceptionDelta `json:"delta"`
		Message      inceptionDelta `json:"message"`
		FinishReason string         `json:"finish_reason"`
	} `json:"choices"`
	Usage            inceptionUsage `json:"usage"`
	ReasoningSummary struct {
		Content string `json:"content"`
	} `json:"reasoning_summary"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type inceptionOutput struct {
	item  map[string]any
	index int
	text  strings.Builder
}

// One state per HTTP response; it keeps item IDs and indices stable across deltas.
type inceptionResponse struct {
	writer      io.Writer
	response    map[string]any
	output      []*inceptionOutput
	message     *inceptionOutput
	toolCalls   map[int]*inceptionOutput
	customTools map[string]bool
	sequence    int
	err         error
}

func newInceptionResponse(writer io.Writer, model string, customTools map[string]bool) *inceptionResponse {
	s := &inceptionResponse{
		writer: writer, toolCalls: map[int]*inceptionOutput{}, customTools: customTools,
		response: map[string]any{"id": "resp_" + rand.Text(), "object": "response", "created_at": time.Now().Unix(), "model": model, "status": "in_progress", "output": []any{}, "error": nil, "incomplete_details": nil},
	}
	s.emit("response.created", map[string]any{"response": s.response})
	return s
}

func (s *inceptionResponse) emit(kind string, event map[string]any) {
	if s.writer == nil || s.err != nil {
		return
	}
	event["type"] = kind
	event["sequence_number"] = s.sequence
	s.sequence++
	encoded, err := json.Marshal(event)
	if err == nil {
		_, err = fmt.Fprintf(s.writer, "event: %s\ndata: %s\n\n", kind, encoded)
	}
	s.err = err
}

func (s *inceptionResponse) add(item map[string]any) *inceptionOutput {
	o := &inceptionOutput{item: item, index: len(s.output)}
	s.output = append(s.output, o)
	s.emit("response.output_item.added", map[string]any{"output_index": o.index, "item": item})
	return o
}

func (s *inceptionResponse) appendDelta(delta inceptionDelta) {
	if delta.Content != "" {
		if s.message == nil {
			s.message = s.add(map[string]any{"id": "msg_" + rand.Text(), "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}})
			s.emit("response.content_part.added", map[string]any{"item_id": s.message.item["id"], "output_index": s.message.index, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
		}
		s.message.text.WriteString(delta.Content)
		s.emit("response.output_text.delta", map[string]any{"item_id": s.message.item["id"], "output_index": s.message.index, "content_index": 0, "delta": delta.Content})
	}
	for _, call := range delta.ToolCalls {
		o := s.toolCalls[call.Index]
		if o == nil {
			item := map[string]any{"id": "fc_" + rand.Text(), "type": "function_call", "call_id": call.ID, "name": "", "arguments": "", "status": "in_progress"}
			if s.customTools[call.Function.Name] {
				item["type"] = "custom_tool_call"
				item["input"] = ""
				delete(item, "arguments")
			}
			item["name"] = call.Function.Name
			o = s.add(item)
			s.toolCalls[call.Index] = o
		} else {
			if call.ID != "" {
				o.item["call_id"] = call.ID
			}
			name, _ := o.item["name"].(string)
			o.item["name"] = name + call.Function.Name
		}
		o.text.WriteString(call.Function.Arguments)
		if o.item["type"] == "function_call" && call.Function.Arguments != "" {
			s.emit("response.function_call_arguments.delta", map[string]any{"item_id": o.item["id"], "output_index": o.index, "delta": call.Function.Arguments})
		}
	}
}

func (s *inceptionResponse) finish(reason string, usage inceptionUsage, summary string) error {
	if reason == "" {
		return fmt.Errorf("inception response ended without a finish reason")
	}
	if summary != "" {
		part := map[string]any{"type": "summary_text", "text": summary}
		o := s.add(map[string]any{"id": "reasoning_" + rand.Text(), "type": "reasoning", "summary": []any{part}})
		s.emit("response.reasoning_summary_part.added", map[string]any{"item_id": o.item["id"], "output_index": o.index, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}})
		s.emit("response.reasoning_summary_text.delta", map[string]any{"item_id": o.item["id"], "output_index": o.index, "summary_index": 0, "delta": summary})
		s.emit("response.reasoning_summary_text.done", map[string]any{"item_id": o.item["id"], "output_index": o.index, "summary_index": 0, "text": summary})
		s.emit("response.reasoning_summary_part.done", map[string]any{"item_id": o.item["id"], "output_index": o.index, "summary_index": 0, "part": part})
	}
	output := make([]any, 0, len(s.output))
	for _, o := range s.output {
		switch o.item["type"] {
		case "message":
			part := map[string]any{"type": "output_text", "text": o.text.String(), "annotations": []any{}}
			o.item["content"] = []any{part}
			s.emit("response.output_text.done", map[string]any{"item_id": o.item["id"], "output_index": o.index, "content_index": 0, "text": o.text.String()})
			s.emit("response.content_part.done", map[string]any{"item_id": o.item["id"], "output_index": o.index, "content_index": 0, "part": part})
		case "function_call":
			o.item["arguments"] = o.text.String()
			s.emit("response.function_call_arguments.done", map[string]any{"item_id": o.item["id"], "output_index": o.index, "arguments": o.text.String()})
		case "custom_tool_call":
			var arguments struct {
				Input *string `json:"input"`
			}
			if err := json.Unmarshal([]byte(o.text.String()), &arguments); err != nil || arguments.Input == nil {
				return fmt.Errorf("inception returned invalid input for custom tool %v", o.item["name"])
			}
			o.item["input"] = *arguments.Input
			s.emit("response.custom_tool_call_input.delta", map[string]any{"item_id": o.item["id"], "output_index": o.index, "delta": *arguments.Input})
			s.emit("response.custom_tool_call_input.done", map[string]any{"item_id": o.item["id"], "output_index": o.index, "input": *arguments.Input})
		}
		if o.item["type"] != "reasoning" {
			o.item["status"] = "completed"
			if reason == "length" || reason == "content_filter" {
				o.item["status"] = "incomplete"
			}
		}
		output = append(output, o.item)
		s.emit("response.output_item.done", map[string]any{"output_index": o.index, "item": o.item})
	}
	s.response["output"] = output
	s.response["status"] = "completed"
	if usage.PromptDetails.CachedTokens != 0 {
		usage.CachedTokens = usage.PromptDetails.CachedTokens
	}
	if usage.CompletionDetails.ReasoningTokens != 0 {
		usage.ReasoningTokens = usage.CompletionDetails.ReasoningTokens
	}
	s.response["usage"] = map[string]any{
		"input_tokens": usage.PromptTokens, "output_tokens": usage.CompletionTokens, "total_tokens": usage.TotalTokens,
		"input_tokens_details":  map[string]any{"cached_tokens": usage.CachedTokens},
		"output_tokens_details": map[string]any{"reasoning_tokens": usage.ReasoningTokens},
	}
	if reason == "length" || reason == "content_filter" {
		s.response["status"] = "incomplete"
		detail := "max_output_tokens"
		if reason == "content_filter" {
			detail = reason
		}
		s.response["incomplete_details"] = map[string]any{"reason": detail}
		s.emit("response.incomplete", map[string]any{"response": s.response})
	} else {
		s.emit("response.completed", map[string]any{"response": s.response})
	}
	return s.err
}

func inceptionStream(writer io.Writer, body io.Reader, model string, customTools map[string]bool) (err error) {
	s := newInceptionResponse(writer, model, customTools)
	defer func() {
		if err != nil {
			s.emit("error", map[string]any{"code": "inception_stream_error", "message": err.Error()})
		}
	}()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var usage inceptionUsage
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
