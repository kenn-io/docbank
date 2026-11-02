package agentcli

import "fmt"

// NewDroid returns an adapter with the configured executable and arguments.
func NewDroid(command Command) (Adapter, error) {
	return newAdapter(Droid, command, "droid", droidCapabilities, buildDroid)
}

var droidCapabilities = Capabilities{
	Modes: []Mode{Interactive, NonInteractive}, Resume: true,
	OutputFormats: []OutputFormat{OutputText, OutputJSON, OutputJSONL}, Model: true,
	ReasoningLevels: []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	AutonomyLevels:  []AutonomyLevel{AutonomyLow, AutonomyMedium, AutonomyHigh},
	ApprovalModes:   []ApprovalMode{ApprovalBypass}, Tools: ToolCapabilities{AllowList: true, DenyList: true},
	DisableSkills: true,
}

func buildDroid(a *adapter, sessionID string, request Request) (Invocation, error) {
	var args []string
	if request.Mode == Interactive {
		if err := validateDroidInteractive(request); err != nil {
			return Invocation{}, err
		}
		args = a.base()
		if sessionID != "" {
			args = append(args, "--resume", sessionID)
		}
	} else {
		args = append([]string{a.executable, "exec"}, a.options...)
		if sessionID != "" {
			args = append(args, "--session-id", sessionID)
		}
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Reasoning != ReasoningDefault {
		args = append(args, "--reasoning-effort", reasoningValue(request.Reasoning))
	}
	if request.Autonomy != AutonomyDefault {
		args = append(args, "--auto", string(request.Autonomy))
	}
	if request.Approval == ApprovalBypass {
		args = append(args, "--skip-permissions-unsafe")
	}
	if len(request.AllowedTools) != 0 {
		args = append(args, "--restrict-tools", joinComma(request.AllowedTools))
	}
	if len(request.DeniedTools) != 0 {
		args = append(args, "--disabled-tools", joinComma(request.DeniedTools))
	}
	if request.DisableSkills {
		args = append(args, "--disable-builtin-skills")
	}
	switch request.OutputFormat {
	case OutputJSON:
		args = append(args, "--output-format", "json")
	case OutputJSONL:
		args = append(args, "--output-format", "stream-json")
	default:
	}
	stdin, err := stdinPrompt(request.Prompt)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Droid, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}

// validateDroidInteractive allows what the Droid REPL's help lists (--resume,
// --auto and --disable-builtin-skills); the other controls are exec flags.
func validateDroidInteractive(request Request) error {
	if err := rejectInteractivePrompt(Droid, request); err != nil {
		return err
	}
	for _, control := range []struct {
		requested bool
		option    string
	}{
		{request.Model != "", "model"},
		{request.Reasoning != ReasoningDefault, "reasoning"},
		{request.Approval != ApprovalDefault, "approval mode"},
		{len(request.AllowedTools) != 0 || len(request.DeniedTools) != 0, "tools"},
	} {
		if control.requested {
			return unsupported(Droid, Interactive, control.option, "", "use noninteractive mode")
		}
	}
	return nil
}
