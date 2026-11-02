package agentcli

import "fmt"

// NewKiro returns an adapter with the configured executable and arguments.
func NewKiro(command Command) (Adapter, error) {
	return newAdapter(Kiro, command, "kiro-cli", kiroCapabilities, buildKiro)
}

var kiroCapabilities = Capabilities{
	Modes:           []Mode{NonInteractive},
	Resume:          true,
	OutputFormats:   []OutputFormat{OutputText},
	ReasoningLevels: []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	ApprovalModes:   []ApprovalMode{ApprovalBypass},
	Tools:           ToolCapabilities{AllowList: true},
}

func buildKiro(a *adapter, sessionID string, request Request) (Invocation, error) {
	args := []string{a.executable, "chat"}
	args = append(args, a.options...)
	args = append(args, "--no-interactive")
	if sessionID != "" {
		args = append(args, "--resume-id", sessionID)
	}
	if request.Reasoning != ReasoningDefault {
		args = append(args, "--effort", reasoningValue(request.Reasoning))
	}
	if request.Approval == ApprovalBypass {
		args = append(args, "--trust-all-tools")
	}
	if len(request.AllowedTools) != 0 {
		args = append(args, "--trust-tools", joinComma(request.AllowedTools))
	}
	if request.Prompt.Text != "" || len(request.Prompt.Files) != 0 {
		args = append(args, "--")
		promptArgs, err := appendArgumentPrompt(args, request.Prompt, false)
		if err != nil {
			return Invocation{}, fmt.Errorf("build %s invocation: %w", Kiro, err)
		}
		return Invocation{Argv: promptArgs}, nil
	}
	return Invocation{Argv: args}, nil
}
