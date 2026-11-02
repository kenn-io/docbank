package agentcli

import "fmt"

// NewGemini returns an adapter with the configured executable and arguments.
func NewGemini(command Command) (Adapter, error) {
	return newAdapter(Gemini, command, "gemini", geminiCapabilities, buildGemini)
}

var geminiCapabilities = Capabilities{
	Modes: []Mode{Interactive, NonInteractive}, Resume: true,
	OutputFormats: []OutputFormat{OutputText, OutputJSON, OutputJSONL}, Model: true,
	ApprovalModes: []ApprovalMode{ApprovalNever, ApprovalBypass},
}

func buildGemini(a *adapter, sessionID string, request Request) (Invocation, error) {
	if request.Mode == Interactive {
		if err := rejectInteractivePrompt(Gemini, request); err != nil {
			return Invocation{}, err
		}
	}
	args := a.base()
	if request.OutputFormat == OutputJSON {
		args = append(args, "--output-format", "json")
	}
	if request.OutputFormat == OutputJSONL {
		args = append(args, "--output-format", "stream-json")
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	switch request.Approval {
	case ApprovalNever:
		args = append(args, "--approval-mode", "plan")
	case ApprovalBypass:
		args = append(args, "--approval-mode", "yolo")
	default:
	}
	stdin, err := stdinPrompt(request.Prompt)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Gemini, err)
	}
	if stdin != nil {
		args = append(args, "--prompt", "")
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}
