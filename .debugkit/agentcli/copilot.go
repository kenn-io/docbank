package agentcli

import (
	"fmt"
)

// NewCopilot returns an adapter with the configured executable and arguments.
func NewCopilot(command Command) (Adapter, error) {
	return newAdapter(Copilot, command, "copilot", copilotCapabilities, buildCopilot)
}

var copilotCapabilities = Capabilities{
	Modes: []Mode{Interactive, NonInteractive}, Resume: true,
	OutputFormats:      []OutputFormat{OutputText, OutputJSONL},
	Model:              true,
	ReasoningLevels:    []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	ApprovalModes:      []ApprovalMode{ApprovalBypass},
	Tools:              ToolCapabilities{AllowList: true, DenyList: true},
	DisableBuiltInMCPs: true, DisableContextFiles: true,
}

func buildCopilot(a *adapter, sessionID string, request Request) (Invocation, error) {
	args := a.base()
	if request.Mode == Interactive {
		if err := rejectInteractivePrompt(Copilot, request); err != nil {
			return Invocation{}, err
		}
	} else {
		// Copilot requires --allow-all-tools in noninteractive prompt mode. Callers
		// can still restrict automatic tool use with --deny-tool rules.
		args = append(args, "--silent", "--allow-all-tools")
	}
	if request.OutputFormat == OutputJSONL {
		args = append(args, "--stream", "off", "--output-format", "json")
	}
	if sessionID != "" {
		args = append(args, "--resume="+sessionID)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Reasoning != ReasoningDefault {
		args = append(args, "--reasoning-effort", reasoningValue(request.Reasoning))
	}
	if request.Approval == ApprovalBypass {
		args = append(args, "--allow-all")
	}
	for _, tool := range request.AllowedTools {
		args = append(args, "--allow-tool", tool)
	}
	for _, tool := range request.DeniedTools {
		args = append(args, "--deny-tool", tool)
	}
	if request.DisableBuiltInMCPs {
		args = append(args, "--disable-builtin-mcps")
	}
	if request.DisableContextFiles {
		args = append(args, "--no-custom-instructions")
	}
	if len(request.Prompt.Files) != 0 {
		return Invocation{}, fmt.Errorf("build %s invocation: agent does not support prompt file arguments", Copilot)
	}
	if request.Prompt.Text != "" {
		args = append(args, "--prompt", request.Prompt.Text)
	}
	return Invocation{Argv: args}, nil
}
