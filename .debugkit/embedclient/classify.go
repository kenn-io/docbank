package embedclient

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"net/http"
	"strings"
)

// classifyFailure derives the Reason for a non-2xx response. The status
// decides most classes. For 400, 413, and 422 the provider's error code or
// message tells an input the endpoint refuses from a request it never
// accepts, and a 403 naming a missing model is a configuration problem.
func classifyFailure(status int, body []byte) Reason {
	code, message := errorDetail(body)
	code, message = strings.ToLower(code), strings.ToLower(message)
	switch {
	case status == http.StatusForbidden && code == "model_not_found":
		return ReasonInvalidRequest
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ReasonCredentials
	case status == http.StatusTooManyRequests:
		return ReasonRateLimited
	case status == http.StatusRequestTimeout || status >= 500:
		return ReasonUnavailable
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		return ReasonInvalidRequest
	case status == http.StatusBadRequest ||
		status == http.StatusRequestEntityTooLarge ||
		status == http.StatusUnprocessableEntity:
	default:
		return ReasonUnknown
	}
	if reason, ok := errorCodeReasons[code]; ok {
		return reason
	}
	if reason := messageReason(message); reason != ReasonUnknown {
		return reason
	}
	if status == http.StatusRequestEntityTooLarge {
		return ReasonInputTooLong
	}
	return ReasonUnknown
}

// errorCodeReasons maps the string error codes providers send to a Reason.
// Many servers send no code, a null code, or a numeric one, so the message
// is the usual signal.
var errorCodeReasons = map[string]Reason{
	"context_length_exceeded":  ReasonInputTooLong,
	"content_policy_violation": ReasonContentPolicy,
	"content_filter":           ReasonContentPolicy,
	"model_not_found":          ReasonInvalidRequest,
}

// messageReason classifies a lowercased error message by its wording. A bare
// keyword is not enough: "invalid token" is a credential failure and
// "unsupported content type" is a media-type failure, so a size word must
// pair with an input word, and "content" must pair with "policy". A message
// about the model or the requested dimensions is a request the endpoint
// never accepts. A limit on the whole batch or request is not about one
// input, so it stays unknown.
func messageReason(message string) Reason {
	switch {
	case message == "":
		return ReasonUnknown
	case strings.Contains(message, "dimension") || modelMissing(message):
		return ReasonInvalidRequest
	case strings.Contains(message, "content") && strings.Contains(message, "policy"):
		return ReasonContentPolicy
	case containsAny(message, "batch", "per request", "number of inputs", "too many inputs"):
		return ReasonUnknown
	case containsAny(message, "too long", "too large", "too many", "length", "maximum", "exceed",
		"overflow", "less than") &&
		containsAny(message, "token", "context", "input", "text", "character"):
		return ReasonInputTooLong
	}
	return ReasonUnknown
}

func modelMissing(message string) bool {
	return strings.Contains(message, "model") &&
		containsAny(message, "not found", "does not exist", "no such", "unknown")
}

func containsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// errorDetail extracts an error code and message from the error shapes
// OpenAI-compatible servers send: {"error":{"code","message"}} (OpenAI,
// LiteLLM, vLLM), {"error":"message"} (Ollama's native route), and a flat
// {"code","message"} (TEI, older vLLM). A body that is not JSON, such as a
// framework's plain-text rejection, is used whole as the message.
func errorDetail(body []byte) (code, message string) {
	var envelope struct {
		Error   jsontext.Value `json:"error"`
		Code    any            `json:"code"`
		Message string         `json:"message"`
	}
	if err := jsonv2.Unmarshal(body, &envelope); err != nil {
		return "", string(body)
	}
	code, message = stringCode(envelope.Code), envelope.Message
	var nested struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	var text string
	switch {
	case jsonv2.Unmarshal(envelope.Error, &nested) == nil:
		if c := stringCode(nested.Code); c != "" {
			code = c
		}
		if nested.Message != "" {
			message = nested.Message
		}
	case jsonv2.Unmarshal(envelope.Error, &text) == nil && text != "":
		message = text
	}
	return code, message
}

// stringCode returns a string error code. Some servers send a numeric code,
// which carries no class beyond the status.
func stringCode(code any) string {
	s, _ := code.(string)
	return s
}
