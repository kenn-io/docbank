package agenthook

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type selectiveHandler struct {
	NoopHandler
	postToolUse *PostToolUseInput
	postOutput  PostToolUseOutput
	stop        *StopInput
}

var _ Handler = (*selectiveHandler)(nil)

func (h *selectiveHandler) PostToolUse(
	_ context.Context,
	input PostToolUseInput,
) (PostToolUseOutput, error) {
	h.postToolUse = &input
	return h.postOutput, nil
}

func (h *selectiveHandler) Stop(
	_ context.Context,
	input StopInput,
) (StopOutput, error) {
	h.stop = &input
	return StopOutput{Decision: DecisionBlock, Reason: "work remains"}, nil
}

func TestHandleNormalizesAndDispatchesTypedPostToolUse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	handler := &selectiveHandler{}
	var output bytes.Buffer

	err := Handle(
		t.Context(),
		AgentHermes,
		strings.NewReader(`{
  "session_id":"h1",
  "hook_event_name":"post_tool_call",
  "tool_name":"terminal",
  "tool_input":{"command":"go test ./agenthook"},
  "extra":{"result":"ok","tool_call_id":"call-1","turn_id":"turn-1","status":"ok"}
}`),
		&output,
		handler,
	)

	require.NoError(err)
	require.NotNil(handler.postToolUse)
	input := handler.postToolUse
	assert.Equal("h1", input.SessionID)
	assert.Equal(EventPostToolUse, input.HookEventName)
	assert.Equal(ToolBash, input.ToolName)
	assert.Equal("call-1", input.ToolUseID)
	assert.Equal("turn-1", input.TurnID)
	assert.JSONEq(`"ok"`, string(input.ToolResponse))
	assert.JSONEq(`{"command":"go test ./agenthook"}`, string(input.ToolInput))
	assert.Contains(string(input.Raw), `"extra"`)
	assert.JSONEq(`{}`, output.String())
}

func TestHandleDispatchesMusePostToolUse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	handler := &selectiveHandler{postOutput: PostToolUseOutput{
		Decision: DecisionBlock, Reason: "output rejected",
	}}
	var output bytes.Buffer

	// Captured from Muse Code 1.4.7.
	err := Handle(
		t.Context(),
		AgentMuse,
		strings.NewReader(`{
  "cwd":"/work",
  "hook_event_name":"PostToolUse",
  "model":"muse-spark-1.2",
  "model_provider":"meta",
  "permission_mode":"bypassPermissions",
  "session_id":"01a13a7d-6d3f-7723-befd-b8d654f1e738",
  "tool_input":{"command":"echo hookprobe","description":"Echo hookprobe text","workdir":"/work"},
  "tool_name":"bash",
  "tool_response":"{\n  \"exit_code\": 0,\n  \"output\": \"hookprobe\\n\"\n}",
  "tool_use_id":"call_01a13a7d86c576e5803275c90658e469",
  "transcript_path":null,
  "turn_id":"9cf6732f-d150-43e5-b962-cb4ca4d72b3b"
}`),
		&output,
		handler,
	)

	require.NoError(err)
	require.NotNil(handler.postToolUse)
	input := handler.postToolUse
	assert.Equal(ToolBash, input.ToolName)
	assert.Equal(PermissionModeBypassPermissions, input.PermissionMode)
	assert.Equal("9cf6732f-d150-43e5-b962-cb4ca4d72b3b", input.TurnID)
	assert.Contains(string(input.Raw), `"model_provider":"meta"`)
	assert.JSONEq(`{"decision":"block","reason":"output rejected"}`, output.String())
}

func TestHandleDispatchesTypedStop(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	handler := &selectiveHandler{}
	var output bytes.Buffer

	err := Handle(
		t.Context(),
		AgentGemini,
		strings.NewReader(`{
  "session_id":"g1",
  "hook_event_name":"AfterAgent",
  "prompt_response":"finished",
  "stop_hook_active":true
}`),
		&output,
		handler,
	)

	require.NoError(err)
	require.NotNil(handler.stop)
	assert.Equal("g1", handler.stop.SessionID)
	assert.Equal(EventStop, handler.stop.HookEventName)
	assert.True(handler.stop.StopHookActive)
	assert.Equal("finished", handler.stop.LastAssistantMessage)
	assert.JSONEq(`{"decision":"block","reason":"work remains"}`, output.String())
}

func TestNoopHandlerImplementsEveryTypedEvent(t *testing.T) {
	var handler Handler = NoopHandler{}
	var output bytes.Buffer

	err := Handle(
		t.Context(),
		AgentClaude,
		strings.NewReader(`{"session_id":"c1","hook_event_name":"Notification","message":"ready","notification_type":"idle_prompt"}`),
		&output,
		handler,
	)

	require.NoError(t, err)
	assert.JSONEq(t, `{}`, output.String())
}

func TestHandleRejectsUnknownEventsBeforeCallingHandler(t *testing.T) {
	var output bytes.Buffer

	err := Handle(
		t.Context(),
		AgentClaude,
		strings.NewReader(`{"session_id":"c1","hook_event_name":"FutureEvent"}`),
		&output,
		NoopHandler{},
	)

	require.Error(t, err)
	require.
		ErrorContains(t, err, `unsupported Claude hook event "FutureEvent"`)
	assert.Empty(t, output.String())
}

type preToolHandler struct {
	NoopHandler
	output PreToolUseOutput
}

func (h preToolHandler) PreToolUse(
	context.Context,
	PreToolUseInput,
) (PreToolUseOutput, error) {
	return h.output, nil
}

func TestHandleEncodesTypedClaudePermissionDecision(t *testing.T) {
	var output bytes.Buffer
	handler := preToolHandler{output: PreToolUseOutput{
		PermissionDecision:       PermissionDecisionDeny,
		PermissionDecisionReason: "production command",
		UpdatedInput:             json.RawMessage(`{"command":"true"}`),
	}}

	err := Handle(
		t.Context(),
		AgentClaude,
		strings.NewReader(`{
  "session_id":"c1",
  "hook_event_name":"PreToolUse",
  "tool_name":"Bash",
  "tool_input":{"command":"false"}
}`),
		&output,
		handler,
	)

	require.NoError(t, err)
	assert.JSONEq(t, `{
  "hookSpecificOutput": {
    "hookEventName": "PreToolUse",
    "permissionDecision": "deny",
    "permissionDecisionReason": "production command",
    "updatedInput": {"command":"true"}
  }
}`, output.String())
}

func TestHandleEncodesTypedCopilotPermissionDecision(t *testing.T) {
	var output bytes.Buffer
	handler := preToolHandler{output: PreToolUseOutput{
		PermissionDecision:       PermissionDecisionDeny,
		PermissionDecisionReason: "production command",
		UpdatedInput:             json.RawMessage(`{"command":"true"}`),
	}}

	err := Handle(
		t.Context(),
		AgentCopilot,
		strings.NewReader(`{
  "session_id":"c1",
  "hook_event_name":"PreToolUse",
  "tool_name":"Bash",
  "tool_input":{"command":"false"}
}`),
		&output,
		handler,
	)

	require.NoError(t, err)
	assert.JSONEq(t, `{
  "permissionDecision": "deny",
  "permissionDecisionReason": "production command",
  "modifiedArgs": {"command":"true"}
	}`, output.String())
}

func TestHandleEncodesGeminiToolDenial(t *testing.T) {
	var output bytes.Buffer
	handler := preToolHandler{output: PreToolUseOutput{
		PermissionDecision:       PermissionDecisionDeny,
		PermissionDecisionReason: "production command",
	}}

	err := Handle(
		t.Context(),
		AgentGemini,
		strings.NewReader(`{
  "session_id":"g1",
  "hook_event_name":"BeforeTool",
  "tool_name":"run_shell_command",
  "tool_input":{"command":"false"}
}`),
		&output,
		handler,
	)

	require.NoError(t, err)
	assert.JSONEq(t, `{
  "decision": "deny",
  "reason": "production command"
}`, output.String())
}

func TestHandleEncodesGeminiToolRewrite(t *testing.T) {
	var output bytes.Buffer
	handler := preToolHandler{output: PreToolUseOutput{
		UpdatedInput: json.RawMessage(`{"command":"true"}`),
	}}

	err := Handle(
		t.Context(),
		AgentGemini,
		strings.NewReader(`{
  "session_id":"g1",
  "hook_event_name":"BeforeTool",
  "tool_name":"run_shell_command",
  "tool_input":{"command":"false"}
}`),
		&output,
		handler,
	)

	require.NoError(t, err)
	assert.JSONEq(t, `{
  "hookSpecificOutput": {
    "hookEventName": "BeforeTool",
    "tool_input": {"command":"true"}
  }
	}`, output.String())
}

func TestHandleRejectsNonObjectToolRewrite(t *testing.T) {
	tests := []struct {
		name    string
		agent   Agent
		payload string
	}{
		{
			name:  "Claude",
			agent: AgentClaude,
			payload: `{
  "session_id":"c1",
  "hook_event_name":"PreToolUse",
  "tool_name":"Bash",
  "tool_input":{"command":"false"}
}`,
		},
		{
			name:  "Copilot",
			agent: AgentCopilot,
			payload: `{
  "session_id":"c1",
  "hook_event_name":"PreToolUse",
  "tool_name":"Bash",
  "tool_input":{"command":"false"}
}`,
		},
		{
			name:  "Gemini",
			agent: AgentGemini,
			payload: `{
  "session_id":"g1",
  "hook_event_name":"BeforeTool",
  "tool_name":"run_shell_command",
  "tool_input":{"command":"false"}
}`,
		},
		{
			name:  "Qwen",
			agent: AgentQwen,
			payload: `{
  "session_id":"q1",
  "hook_event_name":"PreToolUse",
  "tool_name":"run_shell_command",
  "tool_input":{"command":"false"}
}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			handler := preToolHandler{output: PreToolUseOutput{
				UpdatedInput: json.RawMessage(`[]`),
			}}

			err := Handle(
				t.Context(), tt.agent, strings.NewReader(tt.payload),
				&output, handler,
			)

			require.Error(t, err)
			require.
				ErrorContains(t, err, "PreToolUse updatedInput must be a JSON object")
			assert.Empty(t, output.String())
		})
	}
}

type promptHandler struct {
	NoopHandler
	output UserPromptSubmitOutput
}

type permissionHandler struct {
	NoopHandler
	output PermissionRequestOutput
}

func (h permissionHandler) PermissionRequest(
	context.Context,
	PermissionRequestInput,
) (PermissionRequestOutput, error) {
	return h.output, nil
}

type stopHandler struct {
	NoopHandler
	output StopOutput
}

func (h stopHandler) Stop(context.Context, StopInput) (StopOutput, error) {
	return h.output, nil
}

func (h promptHandler) UserPromptSubmit(
	context.Context,
	UserPromptSubmitInput,
) (UserPromptSubmitOutput, error) {
	return h.output, nil
}

func TestHandleEncodesHermesPromptContext(t *testing.T) {
	var output bytes.Buffer
	handler := promptHandler{output: UserPromptSubmitOutput{
		AdditionalContext: "deployment is production",
	}}

	err := Handle(
		t.Context(),
		AgentHermes,
		strings.NewReader(`{
  "session_id":"h1",
  "hook_event_name":"pre_llm_call",
  "extra":{"user_message":"deploy"}
}`),
		&output,
		handler,
	)

	require.NoError(t, err)
	assert.JSONEq(t, `{"context":"deployment is production"}`, output.String())
}

func TestHandleRejectsHermesStopContextWithExplicitAllow(t *testing.T) {
	var output bytes.Buffer
	handler := stopHandler{output: StopOutput{
		Decision:          DecisionAllow,
		AdditionalContext: "continue checking the deployment",
	}}

	err := Handle(
		t.Context(),
		AgentHermes,
		strings.NewReader(`{
  "session_id":"h1",
  "hook_event_name":"pre_verify"
}`),
		&output,
		handler,
	)

	require.Error(t, err)
	require.
		ErrorContains(t, err, "Hermes Stop output does not support decision \"allow\"")
	assert.Empty(t, output.String())
}

func TestHandleEncodesCursorToolDenial(t *testing.T) {
	var output bytes.Buffer
	handler := preToolHandler{output: PreToolUseOutput{
		PermissionDecision:       PermissionDecisionDeny,
		PermissionDecisionReason: "production command",
	}}

	err := Handle(
		t.Context(),
		AgentCursor,
		strings.NewReader(`{
  "conversation_id":"c1",
  "hook_event_name":"preToolUse",
  "tool_name":"Shell",
  "tool_input":{"command":"false"}
}`),
		&output,
		handler,
	)

	require.NoError(t, err)
	assert.JSONEq(t, `{
  "permission":"deny",
  "user_message":"production command",
  "agent_message":"production command"
}`, output.String())
}

func TestHandleEncodesCursorToolRewrite(t *testing.T) {
	var output bytes.Buffer
	handler := preToolHandler{output: PreToolUseOutput{
		UpdatedInput: json.RawMessage(`{"command":"true"}`),
	}}

	err := Handle(
		t.Context(),
		AgentCursor,
		strings.NewReader(`{
  "conversation_id":"c1",
  "hook_event_name":"preToolUse",
  "tool_name":"Shell",
  "tool_input":{"command":"false"}
}`),
		&output,
		handler,
	)

	require.NoError(t, err)
	assert.JSONEq(t, `{"updated_input":{"command":"true"}}`, output.String())
}

func TestHandleEncodesCursorPromptBlock(t *testing.T) {
	var output bytes.Buffer
	handler := promptHandler{output: UserPromptSubmitOutput{
		Decision: DecisionBlock,
		Reason:   "prompt violates policy",
	}}

	err := Handle(
		t.Context(),
		AgentCursor,
		strings.NewReader(`{
  "conversation_id":"c1",
  "hook_event_name":"beforeSubmitPrompt",
  "prompt":"publish the credentials"
}`),
		&output,
		handler,
	)

	require.NoError(t, err)
	assert.JSONEq(t, `{
  "continue":false,
  "user_message":"prompt violates policy"
}`, output.String())
}

func TestHandleRejectsIncompleteToolInput(t *testing.T) {
	var output bytes.Buffer

	err := Handle(
		t.Context(),
		AgentClaude,
		strings.NewReader(`{
  "session_id":"c1",
  "hook_event_name":"PreToolUse",
  "tool_input":{"command":"false"}
}`),
		&output,
		NoopHandler{},
	)

	require.Error(t, err)
	require.
		ErrorContains(t, err, `PreToolUse input missing tool_name`)
	assert.Empty(t, output.String())
}

func TestHandleRejectsMissingSessionID(t *testing.T) {
	var output bytes.Buffer

	err := Handle(
		t.Context(),
		AgentClaude,
		strings.NewReader(`{
  "hook_event_name":"PreToolUse",
  "tool_name":"Bash",
  "tool_input":{"command":"false"}
}`),
		&output,
		NoopHandler{},
	)

	require.Error(t, err)
	require.
		ErrorContains(t, err, `PreToolUse input missing session_id`)
	assert.Empty(t, output.String())
}

func TestHandleRejectsMissingRequiredEventFields(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "session source",
			payload: `{"session_id":"c1","hook_event_name":"SessionStart"}`,
			want:    "SessionStart input missing source",
		},
		{
			name:    "user prompt",
			payload: `{"session_id":"c1","hook_event_name":"UserPromptSubmit"}`,
			want:    "UserPromptSubmit input missing prompt",
		},
		{
			name: "tool failure error",
			payload: `{
  "session_id":"c1",
  "hook_event_name":"PostToolUseFailure",
  "tool_name":"Bash",
  "tool_input":{"command":"false"}
}`,
			want: "PostToolUseFailure input missing error",
		},
		{
			name: "notification message",
			payload: `{
  "session_id":"c1",
  "hook_event_name":"Notification",
  "notification_type":"idle_prompt"
}`,
			want: "Notification input missing message",
		},
		{
			name:    "session end reason",
			payload: `{"session_id":"c1","hook_event_name":"SessionEnd"}`,
			want:    "SessionEnd input missing reason",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			err := Handle(
				t.Context(), AgentClaude, strings.NewReader(tt.payload),
				&output, NoopHandler{},
			)

			require.Error(t, err)
			require.
				ErrorContains(t, err, tt.want)
			assert.Empty(t, output.String())
		})
	}
}

type lifecycleHandler struct {
	NoopHandler
	sessionStart *SessionStartInput
	sessionEnd   *SessionEndInput
}

func (h *lifecycleHandler) SessionStart(
	_ context.Context,
	input SessionStartInput,
) (SessionStartOutput, error) {
	h.sessionStart = &input
	return SessionStartOutput{}, nil
}

func (h *lifecycleHandler) SessionEnd(
	_ context.Context,
	input SessionEndInput,
) (SessionEndOutput, error) {
	h.sessionEnd = &input
	return SessionEndOutput{}, nil
}

func TestHandleAllowsNativeLifecyclePayloadWithoutClaudeEquivalent(t *testing.T) {
	tests := []struct {
		name    string
		agent   Agent
		payload string
		check   func(*testing.T, *lifecycleHandler)
	}{
		{
			name:  "Cursor session start without source",
			agent: AgentCursor,
			payload: `{
  "session_id":"c1",
  "hook_event_name":"sessionStart",
  "is_background_agent":false,
  "composer_mode":"agent"
}`,
			check: func(t *testing.T, handler *lifecycleHandler) {
				t.Helper()
				require.NotNil(t, handler.sessionStart)
				assert.Empty(t, handler.sessionStart.Source)
			},
		},
		{
			name:    "Pi reload without Claude source",
			agent:   AgentPi,
			payload: `{"session_id":"p1","hook_event_name":"session_start","reason":"reload"}`,
			check: func(t *testing.T, handler *lifecycleHandler) {
				t.Helper()
				require.NotNil(t, handler.sessionStart)
				assert.Empty(t, handler.sessionStart.Source)
			},
		},
		{
			name:  "Hermes session end without Claude reason",
			agent: AgentHermes,
			payload: `{
  "session_id":"h1",
  "hook_event_name":"on_session_end",
  "turn_exit_reason":"text_response(stop)",
  "completed":true
}`,
			check: func(t *testing.T, handler *lifecycleHandler) {
				t.Helper()
				require.NotNil(t, handler.sessionEnd)
				assert.Empty(t, handler.sessionEnd.Reason)
			},
		},
		{
			name:    "OpenCode session start without source",
			agent:   AgentOpenCode,
			payload: `{"session_id":"ses_1","hook_event_name":"SessionStart","cwd":"/work"}`,
			check: func(t *testing.T, handler *lifecycleHandler) {
				t.Helper()
				require.NotNil(t, handler.sessionStart)
				assert.Empty(t, handler.sessionStart.Source)
			},
		},
		{
			name:    "OpenCode user prompt without prompt",
			agent:   AgentOpenCode,
			payload: `{"session_id":"ses_1","hook_event_name":"UserPromptSubmit"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			handler := &lifecycleHandler{}

			err := Handle(
				t.Context(), tt.agent, strings.NewReader(tt.payload),
				&output, handler,
			)

			require.NoError(t, err)
			if tt.check != nil {
				tt.check(t, handler)
			}
			assert.JSONEq(t, `{}`, output.String())
		})
	}
}

func TestHandleRejectsUnsupportedClaudeStyleDecision(t *testing.T) {
	for _, agent := range []Agent{AgentClaude, AgentCodex, AgentDroid} {
		t.Run(string(agent), func(t *testing.T) {
			var output bytes.Buffer
			handler := stopHandler{output: StopOutput{
				Decision: DecisionDeny,
				Reason:   "continue checking",
			}}

			err := Handle(
				t.Context(), agent,
				strings.NewReader(`{"session_id":"s1","hook_event_name":"Stop"}`),
				&output, handler,
			)

			require.Error(t, err)
			require.
				ErrorContains(t, err, `Stop output does not support decision "deny"`)
			assert.Empty(t, output.String())
		})
	}
}

func TestHandleRejectsNonObjectPermissionRequestRewrite(t *testing.T) {
	var output bytes.Buffer
	handler := permissionHandler{output: PermissionRequestOutput{
		Decision: &PermissionRequestDecision{
			Behavior:     PermissionBehaviorAllow,
			UpdatedInput: json.RawMessage(`null`),
		},
	}}

	err := Handle(
		t.Context(), AgentClaude,
		strings.NewReader(`{
  "session_id":"c1",
  "hook_event_name":"PermissionRequest",
  "tool_name":"Bash",
  "tool_input":{"command":"false"}
}`),
		&output, handler,
	)

	require.Error(t, err)
	require.
		ErrorContains(t, err, "PermissionRequest updatedInput must be a JSON object")
	assert.Empty(t, output.String())
}

func TestEncodeResponseRejectsUnspecifiedProfileFormat(t *testing.T) {
	spec := newProfileSpec(
		Profile{Agent: "test", DisplayName: "Test Agent"},
		formatNestedJSON,
		ToolBash,
		func() (string, error) { return "", nil },
	)

	response, err := encodeResponse(spec, EventStop, handledOutput{value: StopOutput{}})

	require.Error(t, err)
	require.
		ErrorContains(t, err, "unsupported agent hook response format")
	assert.Empty(t, response)
}

func TestDecodeTypedInputRejectsUnspecifiedLifecycleRequirement(t *testing.T) {
	spec := newProfileSpec(
		Profile{
			Agent:           "test",
			DisplayName:     "Test Agent",
			SupportedEvents: []Event{EventSessionStart},
		},
		formatNestedJSON,
		ToolBash,
		func() (string, error) { return "", nil },
	)
	var input SessionStartInput

	err := decodeTypedInput(
		spec,
		EventSessionStart,
		[]byte(`{"session_id":"s1","hook_event_name":"SessionStart"}`),
		&input.CommonInput,
		&input,
	)

	require.Error(t, err)
	assert.ErrorContains(t, err, "Test Agent has no SessionStart source requirement")
}

func TestHandleRejectsDenyWithoutReason(t *testing.T) {
	var output bytes.Buffer
	handler := preToolHandler{output: PreToolUseOutput{
		PermissionDecision: PermissionDecisionDeny,
	}}

	err := Handle(
		t.Context(),
		AgentClaude,
		strings.NewReader(`{
  "session_id":"c1",
  "hook_event_name":"PreToolUse",
  "tool_name":"Bash",
  "tool_input":{"command":"false"}
}`),
		&output,
		handler,
	)

	require.Error(t, err)
	require.
		ErrorContains(t, err, "deny output requires a reason")
	assert.Empty(t, output.String())
}

func TestHandleRejectsInvalidTypedOutput(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		handler Handler
		want    string
	}{
		{
			name: "permission decision",
			payload: `{
  "session_id":"c1",
  "hook_event_name":"PreToolUse",
  "tool_name":"Bash",
  "tool_input":{"command":"false"}
}`,
			handler: preToolHandler{output: PreToolUseOutput{
				PermissionDecision: PermissionDecision("sometimes"),
			}},
			want: `PreToolUse output has invalid permission decision "sometimes"`,
		},
		{
			name: "block without reason",
			payload: `{
  "session_id":"c1",
  "hook_event_name":"Stop"
}`,
			handler: stopHandler{output: StopOutput{Decision: DecisionBlock}},
			want:    "Stop block output requires a reason",
		},
		{
			name: "permission behavior",
			payload: `{
  "session_id":"c1",
  "hook_event_name":"PermissionRequest",
  "tool_name":"Bash",
  "tool_input":{"command":"false"}
}`,
			handler: permissionHandler{output: PermissionRequestOutput{
				Decision: &PermissionRequestDecision{Behavior: PermissionBehavior("sometimes")},
			}},
			want: `PermissionRequest output has invalid behavior "sometimes"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			err := Handle(
				t.Context(), AgentClaude, strings.NewReader(tt.payload),
				&output, tt.handler,
			)

			require.Error(t, err)
			require.
				ErrorContains(t, err, tt.want)
			assert.Empty(t, output.String())
		})
	}
}

func TestHandleRejectsOversizedPayload(t *testing.T) {
	payload := `{"hook_event_name":"Notification","message":"` +
		strings.Repeat("x", maxHookPayloadBytes) + `"}`

	err := Handle(
		t.Context(), AgentClaude, strings.NewReader(payload),
		&bytes.Buffer{}, NoopHandler{},
	)

	require.Error(t, err)
	assert.ErrorContains(t, err, "hook payload exceeds")
}

func TestHandleDispatchesPiSessionEnd(t *testing.T) {
	var output bytes.Buffer
	handler := &lifecycleHandler{}

	err := Handle(
		t.Context(), AgentPi,
		strings.NewReader(`{"session_id":"pi-1","hook_event_name":"session_shutdown","reason":"new"}`),
		&output, handler,
	)

	require.NoError(t, err)
	require.NotNil(t, handler.sessionEnd)
	assert.Equal(t, "pi-1", handler.sessionEnd.SessionID)
	assert.Equal(t, SessionEndClear, handler.sessionEnd.Reason)
	assert.JSONEq(t, `{}`, output.String())
}

func TestHandleRejectsPiControlOutput(t *testing.T) {
	var output bytes.Buffer
	handler := stopHandler{output: StopOutput{Decision: DecisionBlock, Reason: "work remains"}}

	err := Handle(
		t.Context(), AgentPi,
		strings.NewReader(`{"session_id":"pi-1","hook_event_name":"agent_settled"}`),
		&output, handler,
	)

	require.ErrorContains(t, err, "does not support Stop control output")
	assert.Empty(t, output.String())
}
