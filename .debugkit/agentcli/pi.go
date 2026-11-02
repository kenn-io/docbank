package agentcli

import (
	"fmt"
	"strings"
)

// NewPi returns an adapter with the configured executable and arguments.
func NewPi(command Command) (Adapter, error) {
	return newAdapter(Pi, command, "pi", piCapabilities, buildPi)
}

var piCapabilities = Capabilities{
	Modes:                  []Mode{Interactive, NonInteractive},
	PromptFiles:            true,
	Resume:                 true,
	OutputFormats:          []OutputFormat{OutputText, OutputJSONL},
	JSONSchemaInline:       true,
	JSONSchemaOutputPath:   true,
	JSONSchemaExtension:    true,
	JSONSchemaFallback:     true,
	Model:                  true,
	Provider:               true,
	ReasoningLevels:        []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	Tools:                  ToolCapabilities{AllowList: true, DenyList: true, DisableBuiltIns: true},
	SkillPaths:             true,
	DisableSkills:          true,
	DisableHooks:           DisableExtensionDiscovery,
	DisableExtensions:      true,
	DisablePromptTemplates: true,
	DisableThemes:          true,
	DisableContextFiles:    true,
	DisableSessionStorage:  true,
}

func buildPi(a *adapter, sessionID string, request Request) (Invocation, error) {
	mode := request.Mode
	args := a.base()
	if err := validatePiRequest(mode, request); err != nil {
		return Invocation{}, err
	}
	if request.DisableSessionStorage {
		args = append(args, "--no-session")
	}
	if request.DisableHooks || request.DisableExtensions {
		args = append(args, "--no-extensions")
	}
	if request.DisableBuiltInTools {
		args = append(args, "--no-builtin-tools")
	}
	if request.DisableSkills {
		args = append(args, "--no-skills")
	}
	if request.DisablePromptTemplates {
		args = append(args, "--no-prompt-templates")
	}
	if request.DisableThemes {
		args = append(args, "--no-themes")
	}
	if request.DisableContextFiles {
		args = append(args, "--no-context-files")
	}
	for _, path := range request.SkillPaths {
		args = append(args, "--skill", path)
	}
	if request.Schema.Inline != "" {
		args = append(args,
			"--extension", request.Schema.Extension,
			"--json-schema", request.Schema.Inline,
			"--json-output", request.Schema.OutputPath,
		)
		fallback := request.Schema.Fallback
		if fallback == "" {
			fallback = "none"
		}
		args = append(args, "--json-fallback", fallback)
	}
	if mode == NonInteractive {
		args = append(args, "--print")
	}
	if request.OutputFormat == OutputJSONL {
		args = append(args, "--mode", "json")
	}
	if sessionID != "" {
		args = append(args, "--session", sessionID)
	}
	if request.Provider != "" {
		args = append(args, "--provider", request.Provider)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Reasoning != ReasoningDefault {
		args = append(args, "--thinking", reasoningValue(request.Reasoning))
	}
	if len(request.AllowedTools) != 0 {
		args = append(args, "--tools", strings.Join(request.AllowedTools, ","))
	}
	if len(request.DeniedTools) != 0 {
		args = append(args, "--exclude-tools", strings.Join(request.DeniedTools, ","))
	}
	if request.Prompt.Text != "" || len(request.Prompt.Files) != 0 {
		args = append(args, "--")
	}
	args, err := appendArgumentPrompt(args, request.Prompt, true)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Pi, err)
	}
	return Invocation{Argv: args}, nil
}

func validatePiRequest(mode Mode, request Request) error {
	if request.OutputPath != "" || request.Schema.Path != "" {
		return unsupported(Pi, mode, "output path", "", "Pi output files require its JSON-schema extension")
	}
	if request.Schema.Inline != "" {
		if mode != NonInteractive {
			return unsupported(Pi, mode, "JSON schema", "", "use noninteractive mode")
		}
		if strings.TrimSpace(request.Schema.Extension) == "" || strings.TrimSpace(request.Schema.OutputPath) == "" {
			return fmt.Errorf("agent %q JSON schema requires Schema.Extension and Schema.OutputPath", Pi)
		}
		if request.Schema.Fallback != "" && request.Schema.Fallback != "none" && request.Schema.Fallback != "force" && request.Schema.Fallback != "best-effort" {
			return unsupported(Pi, mode, "JSON fallback", request.Schema.Fallback, "request none, force, or best-effort")
		}
	} else if request.Schema.Extension != "" || request.Schema.OutputPath != "" || request.Schema.Fallback != "" {
		return fmt.Errorf("agent %q schema extension options require Schema.Inline", Pi)
	}
	return nil
}
