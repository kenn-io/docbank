package agenthook

func droidProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentDroid, DisplayName: "Factory Droid",
			ConfigFilename: "hooks.json",
			SupportedEvents: []Event{
				EventPreToolUse, EventPostToolUse, EventStop,
			},
		},
		formatNestedJSON,
		"Execute",
		func() (string, error) { return userDotDir(".factory") },
	)
	// Factory Droid documents Claude-compatible hookSpecificOutput control:
	// https://docs.factory.ai/docs/harness/hooks#pretooluse-control
	spec.responseFormat = responseClaude
	return spec
}
