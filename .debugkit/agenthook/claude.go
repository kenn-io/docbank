package agenthook

func claudeProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentClaude, DisplayName: "Claude Code",
			ConfigEnvironment: "CLAUDE_CONFIG_DIR", ConfigFilename: "settings.json",
			SupportedEvents: []Event{
				EventSessionStart, EventUserPromptSubmit, EventPreToolUse,
				EventPostToolUse, EventPostToolUseFailure, EventPermissionRequest,
				EventNotification, EventStop, EventSessionEnd,
			},
		},
		formatNestedJSON,
		"Bash",
		func() (string, error) { return userDotDir(".claude") },
	)
	// Claude defines the package's public response vocabulary:
	// https://code.claude.com/docs/en/hooks#json-output
	spec.responseFormat = responseClaude
	// Claude SessionStart and SessionEnd require source and reason:
	// https://code.claude.com/docs/en/hooks#hook-inputs
	spec.sessionSourceRequirement = inputRequired
	spec.sessionEndReasonRequirement = inputRequired
	// Windows Claude runs a command string through Git Bash, which strips
	// backslashes, or PowerShell; the exec form ("command" plus "args") runs the
	// executable directly: https://code.claude.com/docs/en/hooks#command-hook-fields
	spec.windowsCommandStyle = windowsCommandExec
	return spec
}
