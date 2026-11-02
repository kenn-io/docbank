package agentcli

import (
	"fmt"
)

// NewCodex returns an adapter with the configured executable and arguments.
func NewCodex(command Command) (Adapter, error) {
	return newAdapter(Codex, command, "codex", codexCapabilities, buildCodex)
}

var codexCapabilities = Capabilities{
	Modes:                 []Mode{Interactive, NonInteractive},
	Resume:                true,
	OutputFormats:         []OutputFormat{OutputText, OutputJSONL},
	JSONSchemaPath:        true,
	JSONSchemaOutputPath:  true,
	Model:                 true,
	ReasoningLevels:       []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	SandboxModes:          []SandboxMode{SandboxReadOnly, SandboxWorkspaceWrite, SandboxDangerFullAccess},
	ApprovalModes:         []ApprovalMode{ApprovalOnRequest, ApprovalNever, ApprovalBypass},
	DisableSkills:         true,
	DisableHooks:          DisableHooksOnly,
	DisableUserConfig:     true,
	DisableSessionStorage: true,
	ConfigOverrides:       true,
}

func buildCodex(a *adapter, sessionID string, request Request) (Invocation, error) {
	mode := request.Mode
	args := a.base()
	if err := validateCodexRequest(mode, request); err != nil {
		return Invocation{}, err
	}
	if mode == NonInteractive {
		args = append(args, "exec")
	}
	if sessionID != "" {
		args = append(args, "resume")
	}

	for _, override := range request.ConfigOverrides {
		args = append(args, "-c", override)
	}
	if request.DisableUserConfig {
		args = append(args, "--ignore-user-config")
	}
	if request.DisableSkills {
		args = append(args, "-c", "skills.include_instructions=false")
	}
	if request.DisableHooks {
		args = append(args, "--disable", "hooks")
	}
	if request.DisableSessionStorage {
		args = append(args, "--ephemeral")
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Reasoning != ReasoningDefault {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", reasoningValue(request.Reasoning)))
	}
	if request.Approval == ApprovalBypass {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	} else {
		if request.Sandbox != SandboxDefault {
			if mode == NonInteractive && sessionID != "" {
				args = append(args, "-c", fmt.Sprintf("sandbox_mode=%q", request.Sandbox))
			} else {
				args = append(args, "--sandbox", string(request.Sandbox))
			}
		}
		if request.Approval != ApprovalDefault {
			if mode == NonInteractive && sessionID != "" {
				args = append(args, "-c", fmt.Sprintf("approval_policy=%q", request.Approval))
			} else {
				args = append(args, "--ask-for-approval", string(request.Approval))
			}
		}
	}
	if request.OutputFormat == OutputJSONL {
		args = append(args, "--json")
	}
	if request.Schema.Path != "" {
		args = append(args, "--output-schema", request.Schema.Path)
	}
	outputPath := request.OutputPath
	if request.Schema.OutputPath != "" {
		outputPath = request.Schema.OutputPath
	}
	if outputPath != "" {
		args = append(args, "--output-last-message", outputPath)
	}
	if sessionID != "" {
		args = append(args, sessionID)
	}
	var stdin *string
	var err error
	if mode == Interactive {
		args, err = appendArgumentPrompt(args, request.Prompt, false)
	} else {
		stdin, err = stdinPrompt(request.Prompt)
		if stdin != nil {
			args = append(args, "-")
		}
	}
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Codex, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}

func validateCodexRequest(mode Mode, request Request) error {
	if request.OutputPath != "" && request.Schema.OutputPath != "" && request.OutputPath != request.Schema.OutputPath {
		return fmt.Errorf("agent %q received conflicting output paths", Codex)
	}
	if mode == Interactive {
		if request.Schema.Path != "" || request.OutputPath != "" || request.Schema.OutputPath != "" {
			return unsupported(Codex, mode, "structured output", "", "use noninteractive mode")
		}
		if request.DisableUserConfig || request.DisableSessionStorage {
			return unsupported(Codex, mode, "automation-only config controls", "", "use noninteractive mode")
		}
	}
	if request.Approval == ApprovalBypass && request.Sandbox != SandboxDefault {
		return fmt.Errorf("agent %q cannot combine approval bypass with sandbox %q", Codex, request.Sandbox)
	}
	return nil
}
