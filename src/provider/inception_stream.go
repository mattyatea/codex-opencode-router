package provider

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

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
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
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
