package provider

import (
	"bytes"
	"strings"
	"testing"
)

func TestInceptionStreamDoesNotCompleteTruncatedResponse(t *testing.T) {
	var output bytes.Buffer
	err := inceptionStream(&output, strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: [DONE]\n\n"), "mercury-3", nil)
	if err == nil || strings.Contains(output.String(), "response.completed") || !strings.Contains(output.String(), "inception_stream_error") {
		t.Fatalf("truncated response reported success: err=%v, output=%s", err, output.String())
	}
}

func TestInceptionRejectsImageInput(t *testing.T) {
	_, _, err := inceptionRequest(map[string]any{"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,example"}}}}})
	if err == nil || !strings.Contains(err.Error(), "text only") {
		t.Fatalf("unsupported image input was discarded: %v", err)
	}
}
