package agentcli

import "fmt"

// NewKilo returns an adapter with the configured executable and arguments.
func NewKilo(command Command) (Adapter, error) {
	return newAdapter(Kilo, command, "kilo", kiloCapabilities, buildKilo)
}

var kiloCapabilities = Capabilities{
	Modes:           []Mode{NonInteractive},
	Resume:          true,
	OutputFormats:   []OutputFormat{OutputText, OutputJSONL},
	Model:           true,
	ReasoningLevels: []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	ApprovalModes:   []ApprovalMode{ApprovalBypass},
}

func buildKilo(a *adapter, sessionID string, request Request) (Invocation, error) {
	args := append(a.base(), "run")
	if request.OutputFormat == OutputJSONL {
		args = append(args, "--format", "json")
	}
	if sessionID != "" {
		args = append(args, "--session", sessionID)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Approval == ApprovalBypass {
		args = append(args, "--auto")
	}
	if variant := reasoningValue(request.Reasoning); variant != "" {
		args = append(args, "--variant", variant)
	}
	stdin, err := stdinPrompt(request.Prompt)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Kilo, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}
