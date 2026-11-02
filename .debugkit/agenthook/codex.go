package agenthook

func codexProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentCodex, DisplayName: "Codex",
			ConfigEnvironment: "CODEX_HOME", ConfigFilename: "hooks.json",
			SupportedEvents: []Event{
				EventSessionStart, EventUserPromptSubmit, EventPreToolUse,
				EventPostToolUse, EventPermissionRequest, EventStop, EventSessionEnd,
			},
		},
		formatNestedJSON,
		"^Bash$",
		func() (string, error) { return userDotDir(".codex") },
	)
	spec.windowsCommandStyle = windowsCommandNested
	spec.shellToolName = ToolBash
	// Codex hooks use the Claude-compatible JSON response and lifecycle input
	// contract, including SessionStart source and SessionEnd reason:
	// https://developers.openai.com/codex/config-advanced#hooks
	spec.responseFormat = responseClaude
	spec.sessionSourceRequirement = inputRequired
	spec.sessionEndReasonRequirement = inputRequired
	return spec
}
