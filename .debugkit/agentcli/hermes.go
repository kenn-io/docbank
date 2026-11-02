package agentcli

// NewHermes returns an adapter with the configured executable and arguments.
func NewHermes(command Command) (Adapter, error) {
	return newAdapter(Hermes, command, "hermes", interactiveResumeCapabilities, buildInteractiveResume)
}

var interactiveResumeCapabilities = Capabilities{
	Modes: []Mode{Interactive}, Resume: true,
	OutputFormats: []OutputFormat{OutputText},
}

// buildInteractiveResume serves CLIs whose only shared contract is a
// top-level --resume ID that reopens their terminal UI.
func buildInteractiveResume(a *adapter, sessionID string, request Request) (Invocation, error) {
	if err := rejectInteractivePrompt(a.name, request); err != nil {
		return Invocation{}, err
	}
	args := a.base()
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	return Invocation{Argv: args}, nil
}
