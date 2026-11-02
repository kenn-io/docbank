package agentcli

// NewQwen returns an adapter with the configured executable and arguments.
func NewQwen(command Command) (Adapter, error) {
	return newAdapter(Qwen, command, "qwen", interactiveResumeCapabilities, buildInteractiveResume)
}
