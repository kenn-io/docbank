package agenthook

import "path/filepath"

func copilotProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentCopilot, DisplayName: "GitHub Copilot CLI",
			ConfigEnvironment: "COPILOT_HOME",
			ConfigFilename:    filepath.Join("hooks", "agenthook.json"),
			SupportedEvents: []Event{
				EventSessionStart, EventUserPromptSubmit, EventPreToolUse,
				EventPostToolUse, EventPostToolUseFailure, EventPermissionRequest,
				EventNotification, EventStop, EventSessionEnd,
			},
		},
		formatDirectJSON,
		"Bash",
		func() (string, error) { return userDotDir(".copilot") },
	)
	spec.windowsCommandStyle = windowsCommandPowerShell
	spec.responseFormat = responseCopilot
	spec.timeoutField = "timeoutSec"
	spec.requireVersion = true
	// Copilot's SessionStart and SessionEnd schemas require source and reason:
	// https://docs.github.com/en/copilot/reference/hooks-reference#sessionstart--sessionstart
	spec.sessionSourceRequirement = inputRequired
	spec.sessionEndReasonRequirement = inputRequired
	return spec
}
