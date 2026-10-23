package provider

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

type responseToolCall struct {
	Index     int    `json:"index"`
	ID        string `json:"id"`
	Namespace string `json:"namespace,omitempty"`
	Function  struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type responseDelta struct {
	Content   string             `json:"content"`
	ToolCalls []responseToolCall `json:"tool_calls"`
}

type responseUsage struct {
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

type responseOutput struct {
	item  map[string]any
	index int
	text  strings.Builder
}

// One state per HTTP response; it keeps item IDs and indices stable across deltas.
type responseStream struct {
	writer      io.Writer
	response    map[string]any
	output      []*responseOutput
	message     *responseOutput
	toolCalls   map[int]*responseOutput
	customTools map[string]bool
	sequence    int
	err         error
}

func newResponseStream(writer io.Writer, model string, customTools map[string]bool) *responseStream {
	s := &responseStream{
		writer: writer, toolCalls: map[int]*responseOutput{}, customTools: customTools,
		response: map[string]any{"id": "resp_" + rand.Text(), "object": "response", "created_at": time.Now().Unix(), "model": model, "status": "in_progress", "output": []any{}, "error": nil, "incomplete_details": nil},
	}
	s.emit("response.created", map[string]any{"response": s.response})
	return s
}

func (s *responseStream) emit(kind string, event map[string]any) {
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

func (s *responseStream) add(item map[string]any) *responseOutput {
	o := &responseOutput{item: item, index: len(s.output)}
	s.output = append(s.output, o)
	s.emit("response.output_item.added", map[string]any{"output_index": o.index, "item": item})
	return o
}

func (s *responseStream) appendDelta(delta responseDelta) {
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
			if call.Namespace != "" {
				item["namespace"] = call.Namespace
			}
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

func (s *responseStream) finish(reason string, usage responseUsage, summary string) error {
	if reason == "" {
		return fmt.Errorf("provider response ended without a finish reason")
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
				return fmt.Errorf("provider returned invalid input for custom tool %v", o.item["name"])
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
