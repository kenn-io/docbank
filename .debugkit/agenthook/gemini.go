package agenthook

import "time"

func geminiProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentGemini, DisplayName: "Gemini CLI",
			ConfigEnvironment: "GEMINI_CLI_HOME", ConfigFilename: "settings.json",
			SupportedEvents: []Event{
				EventSessionStart, EventUserPromptSubmit, EventPreToolUse,
				EventPostToolUse, EventNotification, EventStop, EventSessionEnd,
			},
		},
		formatNestedJSON,
		"run_shell_command",
		func() (string, error) { return userDotDir(".gemini") },
	)
	spec.configEnvSubdir = ".gemini"
	spec.eventName = geminiEventName
	// Gemini BeforeTool uses top-level decision/reason and rewrites through
	// hookSpecificOutput.tool_input rather than Claude's PreToolUse fields:
	// https://github.com/google-gemini/gemini-cli/blob/main/docs/hooks/reference.md#beforetool
	// https://github.com/google-gemini/gemini-cli/blob/main/packages/core/src/hooks/types.ts#L327-L347
	spec.responseFormat = responseGemini
	spec.timeoutUnit = time.Millisecond
	// Gemini SessionStart and SessionEnd require source and reason:
	// https://github.com/google-gemini/gemini-cli/blob/main/docs/hooks/reference.md#sessionstart
	spec.sessionSourceRequirement = inputRequired
	spec.sessionEndReasonRequirement = inputRequired
	return spec
}

func geminiEventName(event Event) string {
	switch event {
	case EventUserPromptSubmit:
		return "BeforeAgent"
	case EventPreToolUse:
		return "BeforeTool"
	case EventPostToolUse:
		return "AfterTool"
	case EventStop:
		return "AfterAgent"
	default:
		return string(event)
	}
}
