// Package visiontest provides synthetic responses for vision integration fixtures.
package visiontest

import (
	"bytes"
	"encoding/json/v2"
)

// IsProbe recognizes the synthetic color-recognition setup request.
func IsProbe(body []byte) bool {
	return bytes.Contains(body, []byte("left and right"))
}

// Completion wraps synthetic content in the provider's completion envelope.
func Completion(content string) []byte {
	body, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}}})
	if err != nil {
		panic(err)
	}
	return body
}
