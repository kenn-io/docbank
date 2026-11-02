package agentcli

// NewMuse returns an adapter with the configured executable and arguments.
func NewMuse(command Command) (Adapter, error) {
	return newAdapter(Muse, command, "muse", museCapabilities, buildMuse)
}

var museCapabilities = Capabilities{
	Modes:                 []Mode{Interactive, NonInteractive},
	Resume:                true,
	OutputFormats:         []OutputFormat{OutputText, OutputJSONL},
	JSONSchemaPath:        true,
	Model:                 true,
	Provider:              true,
	ReasoningLevels:       []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	SandboxModes:          []SandboxMode{SandboxDangerFullAccess},
	ApprovalModes:         []ApprovalMode{ApprovalOnRequest, ApprovalNever, ApprovalBypass},
	DisableSessionStorage: true,
	ConfigOverrides:       true,
}

// buildMuse places configured options after exec in noninteractive mode
// because muse rejects root options before the exec subcommand.
func buildMuse(a *adapter, sessionID string, request Request) (Invocation, error) {
	mode := request.Mode
	var args []string
	if mode == Interactive {
		if request.Schema.Path != "" {
			return Invocation{}, unsupported(Muse, mode, "JSON schema path", "", "use noninteractive mode")
		}
		if sessionID != "" && request.Prompt.Text != "" {
			return Invocation{}, unsupported(Muse, mode, "prompt", "", "omit the prompt when resuming, or use noninteractive mode")
		}
		args = a.base()
	} else {
		args = append([]string{a.executable, "exec"}, a.options...)
		if sessionID != "" {
			args = append(args, "--session-id", sessionID)
		}
	}
	for _, override := range request.ConfigOverrides {
		args = append(args, "-c", override)
	}
	if request.Provider != "" {
		args = append(args, "--provider", request.Provider)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Reasoning != ReasoningDefault {
		args = append(args, "--reasoning-effort", reasoningValue(request.Reasoning))
	}
	if request.Approval == ApprovalBypass {
		// --yolo also disables the sandbox, which covers SandboxDangerFullAccess.
		args = append(args, "--yolo")
	} else {
		if request.Approval != ApprovalDefault {
			args = append(args, "--approval-mode", string(request.Approval))
		}
		if request.Sandbox == SandboxDangerFullAccess {
			args = append(args, "--disable-sandbox")
		}
	}
	if request.DisableSessionStorage {
		args = append(args, "--no-session-log")
	}
	if request.OutputFormat == OutputJSONL {
		args = append(args, "--json")
	}
	if request.Schema.Path != "" {
		args = append(args, "--output-schema", request.Schema.Path)
	}
	if mode == Interactive && sessionID != "" {
		return Invocation{Argv: append(args, "resume", sessionID)}, nil
	}
	// muse exec reads its prompt only from an argument or a named file.
	if request.Prompt.Text != "" {
		args = append(args, "--", request.Prompt.Text)
	}
	return Invocation{Argv: args}, nil
}
