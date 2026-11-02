package agenthook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maxHookPayloadBytes = 16 << 20

func normalize(agent Agent, input io.Reader) ([]byte, error) {
	spec, ok := profiles[agent]
	if !ok {
		return nil, fmt.Errorf("unsupported agent hook integration %q", agent)
	}

	data, err := io.ReadAll(io.LimitReader(input, maxHookPayloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s hook payload: %w", spec.profile.DisplayName, err)
	}
	if len(data) > maxHookPayloadBytes {
		return nil, fmt.Errorf(
			"%s hook payload exceeds %d bytes", spec.profile.DisplayName, maxHookPayloadBytes,
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var payload map[string]json.RawMessage
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode %s hook payload: %w", spec.profile.DisplayName, err)
	}
	if payload == nil {
		return nil, fmt.Errorf("decode %s hook payload: expected a JSON object", spec.profile.DisplayName)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return nil, fmt.Errorf("decode %s hook payload: %w", spec.profile.DisplayName, err)
	}

	promoteAliases(payload)
	if agent == AgentHermes {
		if err := promoteHermesExtra(payload); err != nil {
			return nil, fmt.Errorf("normalize Hermes Agent hook payload: %w", err)
		}
	}
	if agent == AgentPi {
		if err := promotePiReason(payload); err != nil {
			return nil, fmt.Errorf("normalize Pi hook payload: %w", err)
		}
	}
	if err := normalizePayloadString(payload, "hook_event_name", func(value string) string {
		return canonicalEventName(spec, value)
	}); err != nil {
		return nil, fmt.Errorf("normalize %s hook payload: %w", spec.profile.DisplayName, err)
	}
	if err := normalizePayloadString(payload, "tool_name", func(value string) string {
		if value == spec.shellToolName {
			return ToolBash
		}
		return value
	}); err != nil {
		return nil, fmt.Errorf("normalize %s hook payload: %w", spec.profile.DisplayName, err)
	}

	data, err = json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode normalized %s hook payload: %w", spec.profile.DisplayName, err)
	}
	return append(data, '\n'), nil
}

func promoteAliases(payload map[string]json.RawMessage) {
	aliases := []struct {
		canonical string
		native    []string
	}{
		{canonical: "session_id", native: []string{"conversation_id", "sessionId", "sessionID"}},
		{canonical: "transcript_path", native: []string{"transcriptPath"}},
		{canonical: "hook_event_name", native: []string{"hookEventName"}},
		{canonical: "turn_id", native: []string{"turnId", "turnID"}},
		{canonical: "stop_hook_active", native: []string{"stopHookActive"}},
		{canonical: "last_assistant_message", native: []string{"prompt_response", "lastAssistantMessage"}},
		{canonical: "tool_name", native: []string{"toolName"}},
		{canonical: "tool_use_id", native: []string{"tool_call_id", "toolUseId", "toolUseID", "toolCallId", "toolCallID"}},
		{canonical: "tool_input", native: []string{"toolInput"}},
		{canonical: "tool_response", native: []string{"tool_result", "tool_output", "toolResponse", "toolOutput"}},
		{canonical: "notification_type", native: []string{"notificationType"}},
	}
	for _, alias := range aliases {
		promoteFirst(payload, alias.canonical, payload, alias.native...)
	}
}

func promoteHermesExtra(payload map[string]json.RawMessage) error {
	raw, ok := payload["extra"]
	if !ok {
		return nil
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(raw, &extra); err != nil {
		return fmt.Errorf("field %q must be an object: %w", "extra", err)
	}
	promoteFirst(payload, "prompt", extra, "user_message")
	promoteFirst(payload, "turn_id", extra, "turn_id")
	promoteFirst(payload, "tool_use_id", extra, "tool_call_id")
	promoteFirst(payload, "tool_response", extra, "result")
	return nil
}

func promoteFirst(
	payload map[string]json.RawMessage,
	canonical string,
	source map[string]json.RawMessage,
	aliases ...string,
) {
	if _, exists := payload[canonical]; exists {
		return
	}
	for _, alias := range aliases {
		if value, exists := source[alias]; exists {
			payload[canonical] = value
			return
		}
	}
}

func normalizePayloadString(
	payload map[string]json.RawMessage,
	field string,
	normalize func(string) string,
) error {
	raw, ok := payload[field]
	if !ok {
		return nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("field %q must be a string: %w", field, err)
	}
	value = normalize(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	payload[field] = encoded
	return nil
}

func canonicalEventName(spec profileSpec, native string) string {
	for _, event := range spec.profile.SupportedEvents {
		if native == string(event) || native == nativeEventName(spec, event) {
			return string(event)
		}
	}
	return native
}
