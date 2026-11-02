// Package agentcli builds command lines for
// supported coding-agent CLIs.
//
// The package does not start processes or own terminal, session-storage, or
// persistence concerns. Callers retain those responsibilities and may use the
// returned Stdin value with os/exec when the prompt is delivered over stdin.
// Configured arguments pass through unchanged. Request fields provide portable
// defaults and controls without parsing caller-supplied arguments.
package agentcli

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Name identifies a supported agent CLI family.
type Name string

const (
	Codex    Name = "codex"
	Claude   Name = "claude"
	Pi       Name = "pi"
	Gemini   Name = "gemini"
	Copilot  Name = "copilot"
	OpenCode Name = "opencode"
	Cursor   Name = "cursor"
	Kilo     Name = "kilo"
	Kiro     Name = "kiro"
	Droid    Name = "droid"
	Hermes   Name = "hermes"
	Muse     Name = "muse"
	Qwen     Name = "qwen"
)

var supportedNames = []Name{Codex, Claude, Gemini, Copilot, OpenCode, Cursor, Kiro, Kilo, Droid, Pi, Hermes, Qwen, Muse}

// Names returns the CLI families with concrete adapters.
func Names() []Name {
	return slices.Clone(supportedNames)
}

// New returns the concrete adapter for name.
func New(name Name, command Command) (Adapter, error) {
	switch name {
	case Codex:
		return NewCodex(command)
	case Claude:
		return NewClaude(command)
	case Gemini:
		return NewGemini(command)
	case Copilot:
		return NewCopilot(command)
	case OpenCode:
		return NewOpenCode(command)
	case Cursor:
		return NewCursor(command)
	case Kiro:
		return NewKiro(command)
	case Kilo:
		return NewKilo(command)
	case Droid:
		return NewDroid(command)
	case Pi:
		return NewPi(command)
	case Hermes:
		return NewHermes(command)
	case Qwen:
		return NewQwen(command)
	case Muse:
		return NewMuse(command)
	default:
		return nil, fmt.Errorf("unsupported agent CLI %q", name)
	}
}

// Mode selects an interactive terminal session or a one-shot invocation.
type Mode string

const (
	Interactive    Mode = "interactive"
	NonInteractive Mode = "noninteractive"
)

// OutputFormat selects the process output contract.
type OutputFormat string

const (
	OutputDefault OutputFormat = ""
	OutputText    OutputFormat = "text"
	OutputJSON    OutputFormat = "json"
	OutputJSONL   OutputFormat = "jsonl"
)

// Prompt is an optional initial or resumed-turn prompt. Its zero value means no
// prompt. Each adapter chooses its CLI's normal argument or stdin transport.
// Files are supported by agents with native file-reference syntax.
type Prompt struct {
	Text  string
	Files []string
}

// ReasoningLevel is the portable subset of agent reasoning controls.
type ReasoningLevel string

const (
	ReasoningDefault ReasoningLevel = ""
	ReasoningLow     ReasoningLevel = "low"
	ReasoningMedium  ReasoningLevel = "medium"
	ReasoningHigh    ReasoningLevel = "high"
	ReasoningXHigh   ReasoningLevel = "xhigh"
	ReasoningMaximum ReasoningLevel = "maximum"
)

// SandboxMode selects restrictions for model-generated commands.
type SandboxMode string

const (
	SandboxDefault          SandboxMode = ""
	SandboxReadOnly         SandboxMode = "read-only"
	SandboxWorkspaceWrite   SandboxMode = "workspace-write"
	SandboxDangerFullAccess SandboxMode = "danger-full-access"
)

// ApprovalMode selects how tool approval requests are handled.
type ApprovalMode string

const (
	ApprovalDefault   ApprovalMode = ""
	ApprovalOnRequest ApprovalMode = "on-request"
	ApprovalNever     ApprovalMode = "never"
	ApprovalBypass    ApprovalMode = "bypass"
)

// AutonomyLevel selects an agent's native tier of unattended actions.
type AutonomyLevel string

const (
	AutonomyDefault AutonomyLevel = ""
	AutonomyLow     AutonomyLevel = "low"
	AutonomyMedium  AutonomyLevel = "medium"
	AutonomyHigh    AutonomyLevel = "high"
)

// JSONSchema configures a CLI's native structured-output mechanism. Codex
// accepts Path, while Claude and Pi accept Inline. Pi additionally requires an
// Extension and OutputPath.
type JSONSchema struct {
	Inline     string
	Path       string
	OutputPath string
	Extension  string
	Fallback   string
}

// Request describes one agent turn. Its zero value requests an interactive
// invocation using the agent's configured defaults. Discovery controls are
// separate because extensions, skills, hooks, MCP servers, and context files
// are distinct concepts in the supported CLIs. Explicit configured command
// options remain the caller's responsibility.
type Request struct {
	Mode                   Mode
	Prompt                 Prompt
	Model                  string
	Provider               string
	Reasoning              ReasoningLevel
	OutputFormat           OutputFormat
	OutputPath             string
	Schema                 JSONSchema
	Sandbox                SandboxMode
	Approval               ApprovalMode
	Autonomy               AutonomyLevel
	AllowedTools           []string
	DeniedTools            []string
	DisableBuiltInTools    bool
	SkillPaths             []string
	DisableSkills          bool
	DisableHooks           bool
	DisableExtensions      bool
	DisablePromptTemplates bool
	DisableThemes          bool
	DisableContextFiles    bool
	DisableBuiltInMCPs     bool
	DisableUserConfig      bool
	DisableSessionStorage  bool
	ConfigOverrides        []string
}

// DisableScope describes what a CLI must turn off to disable hooks.
type DisableScope string

const (
	DisableUnsupported        DisableScope = ""
	DisableHooksOnly          DisableScope = "hooks-only"
	DisableAllCustomizations  DisableScope = "all-customizations"
	DisableExtensionDiscovery DisableScope = "extension-discovery"
)

// ToolCapabilities describes native tool-selection flags.
type ToolCapabilities struct {
	AllowList       bool
	DenyList        bool
	DisableBuiltIns bool
}

// Capabilities reports which Request fields an adapter can honor.
type Capabilities struct {
	Modes                  []Mode
	PromptFiles            bool
	Resume                 bool
	OutputFormats          []OutputFormat
	JSONSchemaInline       bool
	JSONSchemaPath         bool
	JSONSchemaOutputPath   bool
	JSONSchemaExtension    bool
	JSONSchemaFallback     bool
	Model                  bool
	Provider               bool
	ReasoningLevels        []ReasoningLevel
	SandboxModes           []SandboxMode
	ApprovalModes          []ApprovalMode
	AutonomyLevels         []AutonomyLevel
	Tools                  ToolCapabilities
	SkillPaths             bool
	DisableSkills          bool
	DisableHooks           DisableScope
	DisableExtensions      bool
	DisablePromptTemplates bool
	DisableThemes          bool
	DisableContextFiles    bool
	DisableBuiltInMCPs     bool
	DisableUserConfig      bool
	DisableSessionStorage  bool
	ConfigOverrides        bool
}

// Invocation is a complete argv plus optional stdin content. Argv includes the
// configured executable and its configured options.
type Invocation struct {
	Argv  []string
	Stdin *string
}

// Command identifies an agent executable and arguments included on every
// invocation. Options pass through unchanged, in order, before request-generated
// arguments. Callers own their meaning and any overlap with Request fields.
// A zero Command uses the adapter's default executable and no extra arguments.
type Command struct {
	Executable string
	Options    []string
}

// Adapter builds start and resume invocations for one CLI family.
type Adapter interface {
	Name() Name
	Capabilities() Capabilities
	Start(Request) (Invocation, error)
	Resume(sessionID string, request Request) (Invocation, error)
}

// UnsupportedOptionError reports a requested option that an adapter cannot
// represent without changing its meaning.
type UnsupportedOptionError struct {
	Agent  Name
	Option string
	Value  string
	Mode   Mode
	Hint   string
}

func (e *UnsupportedOptionError) Error() string {
	message := fmt.Sprintf("agent %q does not support %s", e.Agent, e.Option)
	if e.Value != "" {
		message += "=" + fmt.Sprintf("%q", e.Value)
	}
	if e.Mode != "" {
		message += " in " + string(e.Mode) + " mode"
	}
	if e.Hint != "" {
		message += "; " + e.Hint
	}
	return message
}

type adapter struct {
	name         Name
	executable   string
	options      []string
	capabilities Capabilities
	build        func(*adapter, string, Request) (Invocation, error)
}

func newAdapter(name Name, command Command, defaultExecutable string, capabilities Capabilities, build func(*adapter, string, Request) (Invocation, error)) (Adapter, error) {
	executable := command.Executable
	if executable == "" {
		executable = defaultExecutable
	}
	if strings.TrimSpace(executable) == "" {
		return nil, fmt.Errorf("agent %q requires a configured executable", name)
	}
	return &adapter{
		name:         name,
		executable:   executable,
		options:      slices.Clone(command.Options),
		capabilities: capabilities,
		build:        build,
	}, nil
}

func (a *adapter) Name() Name                                { return a.name }
func (a *adapter) Capabilities() Capabilities                { return cloneCapabilities(a.capabilities) }
func (a *adapter) Start(request Request) (Invocation, error) { return a.invoke("", request) }
func (a *adapter) Resume(sessionID string, request Request) (Invocation, error) {
	sessionID, err := validateSessionID(sessionID)
	if err != nil {
		return Invocation{}, err
	}
	return a.invoke(sessionID, request)
}

func (a *adapter) invoke(sessionID string, request Request) (Invocation, error) {
	mode, err := invocationMode(request.Mode)
	if err != nil {
		return Invocation{}, err
	}
	if !slices.Contains(a.capabilities.Modes, mode) {
		return Invocation{}, unsupported(a.name, mode, "mode", string(mode), "choose a mode listed by Capabilities")
	}
	request.Mode = mode
	if err := validateSupportedRequest(a.name, mode, request, a.capabilities); err != nil {
		return Invocation{}, err
	}
	if mode == Interactive && request.OutputFormat != OutputDefault && request.OutputFormat != OutputText {
		return Invocation{}, unsupported(a.name, mode, "output format", string(request.OutputFormat), "use noninteractive mode for machine-readable output")
	}
	for _, values := range []struct {
		name   string
		values []string
	}{
		{"allowed tools", request.AllowedTools},
		{"denied tools", request.DeniedTools},
		{"skill paths", request.SkillPaths},
		{"config overrides", request.ConfigOverrides},
	} {
		if err := validateValues(values.name, values.values); err != nil {
			return Invocation{}, err
		}
	}
	return a.build(a, sessionID, request)
}

func (a *adapter) base() []string {
	return append([]string{a.executable}, a.options...)
}

func invocationMode(mode Mode) (Mode, error) {
	if mode == "" {
		return Interactive, nil
	}
	if mode != Interactive && mode != NonInteractive {
		return "", fmt.Errorf("unknown agent invocation mode %q", mode)
	}
	return mode, nil
}

func validateSessionID(sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.HasPrefix(sessionID, "-") {
		return "", errors.New("agent resume requires a session ID that does not begin with '-'")
	}
	return sessionID, nil
}

func validateValues(option string, values []string) error {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("agent %s contains an empty value", option)
		}
	}
	return nil
}

func joinComma(values []string) string {
	return strings.Join(values, ",")
}

func reasoningValue(level ReasoningLevel) string {
	if level == ReasoningMaximum {
		return "max"
	}
	return string(level)
}

func unsupported(name Name, mode Mode, option, value, hint string) error {
	return &UnsupportedOptionError{Agent: name, Option: option, Value: value, Mode: mode, Hint: hint}
}

// rejectInteractivePrompt keeps a prompt out of adapters that build no
// interactive prompt transport, where it would select noninteractive mode.
func rejectInteractivePrompt(name Name, request Request) error {
	if request.Prompt.Text != "" {
		return unsupported(name, Interactive, "prompt", "", "omit the prompt, or use noninteractive mode where Capabilities lists it")
	}
	return nil
}

func validateSupportedRequest(name Name, mode Mode, request Request, capabilities Capabilities) error {
	checks := []struct {
		requested bool
		supported bool
		option    string
		value     string
	}{
		{len(request.Prompt.Files) != 0, capabilities.PromptFiles, "prompt files", ""},
		{request.OutputFormat != OutputDefault, slices.Contains(capabilities.OutputFormats, request.OutputFormat), "output format", string(request.OutputFormat)},
		{request.Schema.Inline != "", capabilities.JSONSchemaInline, "inline JSON schema", ""},
		{request.Schema.Path != "", capabilities.JSONSchemaPath, "JSON schema path", ""},
		{request.OutputPath != "" || request.Schema.OutputPath != "", capabilities.JSONSchemaOutputPath, "output path", ""},
		{request.Schema.Extension != "", capabilities.JSONSchemaExtension, "JSON schema extension", ""},
		{request.Schema.Fallback != "", capabilities.JSONSchemaFallback, "JSON schema fallback", request.Schema.Fallback},
		{request.Model != "", capabilities.Model, "model", request.Model},
		{request.Provider != "", capabilities.Provider, "provider", request.Provider},
		{request.Reasoning != ReasoningDefault, slices.Contains(capabilities.ReasoningLevels, request.Reasoning), "reasoning", string(request.Reasoning)},
		{request.Sandbox != SandboxDefault, slices.Contains(capabilities.SandboxModes, request.Sandbox), "sandbox", string(request.Sandbox)},
		{request.Approval != ApprovalDefault, slices.Contains(capabilities.ApprovalModes, request.Approval), "approval mode", string(request.Approval)},
		{request.Autonomy != AutonomyDefault, slices.Contains(capabilities.AutonomyLevels, request.Autonomy), "autonomy", string(request.Autonomy)},
		{len(request.AllowedTools) != 0, capabilities.Tools.AllowList, "allowed tools", ""},
		{len(request.DeniedTools) != 0, capabilities.Tools.DenyList, "denied tools", ""},
		{request.DisableBuiltInTools, capabilities.Tools.DisableBuiltIns, "disable built-in tools", ""},
		{len(request.SkillPaths) != 0, capabilities.SkillPaths, "skill paths", ""},
		{request.DisableSkills, capabilities.DisableSkills, "disable skills", ""},
		{request.DisableHooks, capabilities.DisableHooks != DisableUnsupported, "disable hooks", ""},
		{request.DisableExtensions, capabilities.DisableExtensions, "disable extensions", ""},
		{request.DisablePromptTemplates, capabilities.DisablePromptTemplates, "disable prompt templates", ""},
		{request.DisableThemes, capabilities.DisableThemes, "disable themes", ""},
		{request.DisableContextFiles, capabilities.DisableContextFiles, "disable context files", ""},
		{request.DisableBuiltInMCPs, capabilities.DisableBuiltInMCPs, "disable built-in MCP servers", ""},
		{request.DisableUserConfig, capabilities.DisableUserConfig, "disable user config", ""},
		{request.DisableSessionStorage, capabilities.DisableSessionStorage, "disable session storage", ""},
		{len(request.ConfigOverrides) != 0, capabilities.ConfigOverrides, "config overrides", ""},
	}
	for _, check := range checks {
		if check.requested && !check.supported {
			return unsupported(name, mode, check.option, check.value, "remove this request option or choose an adapter that lists the capability")
		}
	}
	return nil
}

func appendArgumentPrompt(args []string, prompt Prompt, supportsFiles bool) ([]string, error) {
	if len(prompt.Files) != 0 && !supportsFiles {
		return nil, errors.New("agent does not support prompt file arguments")
	}
	for _, file := range prompt.Files {
		if strings.TrimSpace(file) == "" {
			return nil, errors.New("agent prompt file path is empty")
		}
		args = append(args, "@"+file)
	}
	if prompt.Text != "" {
		args = append(args, prompt.Text)
	}
	return args, nil
}

func stdinPrompt(prompt Prompt) (*string, error) {
	if len(prompt.Files) != 0 {
		return nil, errors.New("agent does not support prompt file arguments")
	}
	if prompt.Text == "" {
		return nil, nil
	}
	return new(prompt.Text), nil
}

func cloneCapabilities(capabilities Capabilities) Capabilities {
	capabilities.Modes = slices.Clone(capabilities.Modes)
	capabilities.OutputFormats = slices.Clone(capabilities.OutputFormats)
	capabilities.ReasoningLevels = slices.Clone(capabilities.ReasoningLevels)
	capabilities.SandboxModes = slices.Clone(capabilities.SandboxModes)
	capabilities.ApprovalModes = slices.Clone(capabilities.ApprovalModes)
	capabilities.AutonomyLevels = slices.Clone(capabilities.AutonomyLevels)
	return capabilities
}
