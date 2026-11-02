package agentcli

import (
	"fmt"
	"strings"
)

// NewClaude returns an adapter with the configured executable and arguments.
func NewClaude(command Command) (Adapter, error) {
	return newAdapter(Claude, command, "claude", claudeCapabilities, buildClaude)
}

var claudeCapabilities = Capabilities{
	Modes:                 []Mode{Interactive, NonInteractive},
	Resume:                true,
	OutputFormats:         []OutputFormat{OutputText, OutputJSON, OutputJSONL},
	JSONSchemaInline:      true,
	Model:                 true,
	ReasoningLevels:       []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	ApprovalModes:         []ApprovalMode{ApprovalOnRequest, ApprovalNever, ApprovalBypass},
	Tools:                 ToolCapabilities{AllowList: true, DenyList: true, DisableBuiltIns: true},
	DisableSkills:         true,
	DisableHooks:          DisableAllCustomizations,
	DisableSessionStorage: true,
}

func buildClaude(a *adapter, sessionID string, request Request) (Invocation, error) {
	mode := request.Mode
	args := a.base()
	if err := validateClaudeRequest(mode, request); err != nil {
		return Invocation{}, err
	}
	if mode == NonInteractive {
		args = append(args, "--print")
	}
	if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText {
		format := string(request.OutputFormat)
		if request.OutputFormat == OutputJSONL {
			format = "stream-json"
			args = append(args, "--verbose")
		}
		args = append(args, "--output-format", format)
	}
	if request.Schema.Inline != "" {
		args = append(args, "--json-schema", request.Schema.Inline)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Reasoning != ReasoningDefault {
		args = append(args, "--effort", reasoningValue(request.Reasoning))
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if request.DisableHooks {
		args = append(args, "--safe-mode")
	} else if request.DisableSkills {
		args = append(args, "--disable-slash-commands")
	}
	if request.DisableSessionStorage {
		args = append(args, "--no-session-persistence")
	}
	if request.Approval == ApprovalBypass {
		args = append(args, "--dangerously-skip-permissions")
	} else if request.Approval != ApprovalDefault {
		permissionMode := "manual"
		if request.Approval == ApprovalNever {
			permissionMode = "dontAsk"
		}
		args = append(args, "--permission-mode", permissionMode)
	}
	if request.DisableBuiltInTools {
		args = append(args, "--tools", "")
	} else if len(request.AllowedTools) != 0 {
		args = append(args, "--allowedTools", strings.Join(request.AllowedTools, ","))
	}
	if len(request.DeniedTools) != 0 {
		args = append(args, "--disallowedTools", strings.Join(request.DeniedTools, ","))
	}
	var stdin *string
	var err error
	if mode == Interactive {
		args, err = appendArgumentPrompt(args, request.Prompt, false)
	} else {
		stdin, err = stdinPrompt(request.Prompt)
	}
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Claude, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}

func validateClaudeRequest(mode Mode, request Request) error {
	if request.DisableBuiltInTools && len(request.AllowedTools) != 0 {
		return fmt.Errorf("agent %q cannot disable built-in tools and set an allowed tool list", Claude)
	}
	if mode == Interactive {
		if request.Schema.Inline != "" || request.DisableSessionStorage {
			return unsupported(Claude, mode, "automation-only output controls", "", "use noninteractive mode")
		}
	}
	return nil
}
