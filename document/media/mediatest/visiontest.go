package mediatest

import (
	"bytes"
	"encoding/json/v2"

	"go.kenn.io/docbank/document"
)

// IsVisionProbe recognizes the adapter's synthetic color-recognition readiness request.
func IsVisionProbe(body []byte) bool {
	return bytes.Contains(body, []byte(document.VisionProbePrompt))
}

// VisionCompletion wraps synthetic content in the provider's completion envelope.
func VisionCompletion(content string) []byte {
	body, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}}})
	if err != nil {
		panic(err)
	}
	return body
}
