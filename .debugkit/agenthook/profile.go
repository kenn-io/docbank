package agenthook

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Agent identifies an agent harness with a supported command-hook profile.
type Agent string

const (
	AgentClaude   Agent = "claude"
	AgentCodex    Agent = "codex"
	AgentCopilot  Agent = "copilot"
	AgentCursor   Agent = "cursor"
	AgentDroid    Agent = "droid"
	AgentGemini   Agent = "gemini"
	AgentHermes   Agent = "hermes"
	AgentMuse     Agent = "muse"
	AgentOpenCode Agent = "opencode"
	AgentPi       Agent = "pi"
	AgentQwen     Agent = "qwen"
)

// Event is a Claude Code lifecycle event name. Profiles translate events to
// native names when a harness uses a different vocabulary.
type Event string

const (
	EventSessionStart       Event = "SessionStart"
	EventUserPromptSubmit   Event = "UserPromptSubmit"
	EventPreToolUse         Event = "PreToolUse"
	EventPostToolUse        Event = "PostToolUse"
	EventPostToolUseFailure Event = "PostToolUseFailure"
	EventPermissionRequest  Event = "PermissionRequest"
	EventNotification       Event = "Notification"
	EventStop               Event = "Stop"
	EventSessionEnd         Event = "SessionEnd"
)

// ToolBash is the Claude Code tool name for shell execution. Profiles map this
// exact matcher to the corresponding native tool name, such as terminal in
// Hermes, Execute in Factory Droid, bash in Muse Code, and run_shell_command
// in Gemini and Qwen.
const ToolBash = "Bash"

// Profile describes the stable, user-visible properties of an agent hook
// integration. SupportedEvents uses the Claude Code event vocabulary.
type Profile struct {
	Agent             Agent
	DisplayName       string
	ConfigEnvironment string
	ConfigFilename    string
	SupportedEvents   []Event
}

type configFormat uint8

const (
	formatNestedJSON configFormat = iota
	formatDirectJSON
	formatHermesYAML
	// formatScript writes a kit-owned JavaScript module the harness loads.
	formatScript
)

type windowsCommandStyle uint8

const (
	windowsCommandNone windowsCommandStyle = iota
	windowsCommandNested
	windowsCommandPowerShell
	// windowsCommandExec writes an executable and its arguments as separate
	// fields on Windows, so no shell parses them.
	windowsCommandExec
)

type responseFormat uint8

const (
	responseUnknown responseFormat = iota
	responseClaude
	responseCopilot
	responseCursor
	responseGemini
	responseHermes
	responseObservational
)

type inputRequirement uint8

const (
	inputUnspecified inputRequirement = iota
	inputOptional
	inputRequired
)

type profileSpec struct {
	profile                     Profile
	format                      configFormat
	shellTool                   string
	shellToolName               string
	defaultDir                  func() (string, error)
	configEnvSubdir             string
	configEnvDir                func(string) (string, error)
	newConfigFields             map[string]any
	eventName                   func(Event) string
	timeoutUnit                 time.Duration
	timeoutField                string
	windowsCommandStyle         windowsCommandStyle
	responseFormat              responseFormat
	failClosedEvents            []Event
	requireVersion              bool
	sessionSourceRequirement    inputRequirement
	userPromptRequirement       inputRequirement
	sessionEndReasonRequirement inputRequirement
	script                      string
}

var profileOrder = []Agent{
	AgentClaude,
	AgentCodex,
	AgentCopilot,
	AgentCursor,
	AgentDroid,
	AgentGemini,
	AgentHermes,
	AgentMuse,
	AgentOpenCode,
	AgentPi,
	AgentQwen,
}

var profiles = map[Agent]profileSpec{
	AgentClaude:   claudeProfile(),
	AgentCodex:    codexProfile(),
	AgentCopilot:  copilotProfile(),
	AgentCursor:   cursorProfile(),
	AgentDroid:    droidProfile(),
	AgentGemini:   geminiProfile(),
	AgentHermes:   hermesProfile(),
	AgentMuse:     museProfile(),
	AgentOpenCode: openCodeProfile(),
	AgentPi:       piProfile(),
	AgentQwen:     qwenProfile(),
}

func newProfileSpec(
	profile Profile,
	format configFormat,
	shellTool string,
	defaultDir func() (string, error),
) profileSpec {
	return profileSpec{
		profile: profile, format: format, shellTool: shellTool,
		shellToolName:         shellTool,
		userPromptRequirement: inputRequired,
		defaultDir:            defaultDir, timeoutUnit: time.Second, timeoutField: "timeout",
	}
}

// ParseAgent resolves a case-insensitive agent name.
func ParseAgent(name string) (Agent, error) {
	agent := Agent(strings.ToLower(strings.TrimSpace(name)))
	if _, ok := profiles[agent]; !ok {
		return "", fmt.Errorf("unsupported agent hook integration %q", name)
	}
	return agent, nil
}

// Profiles returns every supported agent profile in stable display order.
func Profiles() []Profile {
	result := make([]Profile, 0, len(profileOrder))
	for _, agent := range profileOrder {
		profile, _ := LookupProfile(agent)
		result = append(result, profile)
	}
	return result
}

// LookupProfile returns a copy of the profile for agent.
func LookupProfile(agent Agent) (Profile, bool) {
	spec, ok := profiles[agent]
	if !ok {
		return Profile{}, false
	}
	profile := spec.profile
	profile.SupportedEvents = slices.Clone(profile.SupportedEvents)
	return profile, true
}

// ConfigPath returns the current user's standard config path for agent,
// honoring the harness-specific home environment variable when one exists.
func ConfigPath(agent Agent) (string, error) {
	spec, ok := profiles[agent]
	if !ok {
		return "", fmt.Errorf("unsupported agent hook integration %q", agent)
	}
	dir := ""
	if spec.profile.ConfigEnvironment != "" {
		dir = strings.TrimSpace(os.Getenv(spec.profile.ConfigEnvironment))
		if dir != "" && spec.configEnvSubdir != "" {
			dir = filepath.Join(dir, spec.configEnvSubdir)
		}
		if dir != "" && spec.configEnvDir != nil {
			var err error
			if dir, err = spec.configEnvDir(dir); err != nil {
				return "", fmt.Errorf("resolve %s config directory: %w", spec.profile.DisplayName, err)
			}
		}
	}
	if dir == "" {
		var err error
		dir, err = spec.defaultDir()
		if err != nil {
			return "", fmt.Errorf("resolve %s config directory: %w", spec.profile.DisplayName, err)
		}
	}
	return filepath.Join(dir, spec.profile.ConfigFilename), nil
}

func nativeMatcher(spec profileSpec, matcher string) string {
	if matcher == ToolBash {
		return spec.shellTool
	}
	return matcher
}

func nativeEventName(spec profileSpec, event Event) string {
	if spec.eventName != nil {
		return spec.eventName(event)
	}
	return string(event)
}

func userDotDir(name string) (string, error) {
	if name == ".factory" {
		if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
			return filepath.Join(home, name), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, name), nil
}

func hermesDefaultDir() (string, error) {
	if runtime.GOOS == "windows" {
		if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
			return filepath.Join(local, "hermes"), nil
		}
	}
	return userDotDir(".hermes")
}
