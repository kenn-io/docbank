// Package visiontest provides synthetic responses for vision integration fixtures.
package visiontest

import (
	"bytes"
	"encoding/json/v2"

	"go.kenn.io/docbank/document"
)

// IsProbe recognizes the adapter's synthetic color-recognition readiness request.
func IsProbe(body []byte) bool {
	return bytes.Contains(body, []byte(document.VisionProbePrompt))
}

// Completion wraps synthetic content in the provider's completion envelope.
func Completion(content string) []byte {
	body, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}}})
	if err != nil {
		panic(err)
	}
	return body
}
