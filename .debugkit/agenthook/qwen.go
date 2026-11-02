package agenthook

import "time"

func qwenProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentQwen, DisplayName: "Qwen Code",
			ConfigEnvironment: "QWEN_HOME", ConfigFilename: "settings.json",
			SupportedEvents: []Event{
				EventSessionStart, EventUserPromptSubmit, EventPreToolUse,
				EventPostToolUse, EventPostToolUseFailure, EventPermissionRequest,
				EventNotification, EventStop, EventSessionEnd,
			},
		},
		formatNestedJSON,
		// Qwen hook matchers use the internal tool id run_shell_command, not
		// the Bash permission/display alias:
		// https://github.com/QwenLM/qwen-code/blob/main/docs/users/features/hooks.md#hook-configuration
		// https://github.com/QwenLM/qwen-code/blob/main/packages/core/src/tools/tool-names.ts#L20-L28
		"run_shell_command",
		func() (string, error) { return userDotDir(".qwen") },
	)
	// Qwen documents Claude-compatible JSON output for hook control:
	// https://github.com/QwenLM/qwen-code/blob/main/docs/users/features/hooks.md#json-output
	spec.responseFormat = responseClaude
	spec.timeoutUnit = time.Millisecond
	// Qwen SessionStart and SessionEnd require source and reason:
	// https://github.com/QwenLM/qwen-code/blob/main/docs/users/features/hooks.md#sessionstart
	spec.sessionSourceRequirement = inputRequired
	spec.sessionEndReasonRequirement = inputRequired
	return spec
}
