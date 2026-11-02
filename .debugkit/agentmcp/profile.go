package agentmcp

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Agent identifies a supported MCP client.
type Agent string

const (
	AgentClaude  Agent = "claude"
	AgentCodex   Agent = "codex"
	AgentCopilot Agent = "copilot"
	AgentCursor  Agent = "cursor"
	AgentDroid   Agent = "droid"
	AgentGemini  Agent = "gemini"
	AgentHermes  Agent = "hermes"
	AgentQwen    Agent = "qwen"
)

// Profile describes an agent's MCP configuration format and user config path.
type Profile struct {
	Agent             Agent
	DisplayName       string
	ConfigEnvironment string
	ConfigFilename    string
}

// Profiles returns supported clients in stable display order.
func Profiles() []Profile {
	return []Profile{
		{AgentClaude, "Claude Code", "CLAUDE_CONFIG_DIR", ".claude.json"},
		{AgentCodex, "Codex", "CODEX_HOME", "config.toml"},
		{AgentCopilot, "GitHub Copilot CLI", "COPILOT_HOME", "mcp-config.json"},
		{AgentCursor, "Cursor", "", "mcp.json"},
		{AgentDroid, "Factory Droid", "", "mcp.json"},
		{AgentGemini, "Gemini CLI", "GEMINI_CLI_HOME", "settings.json"},
		{AgentHermes, "Hermes Agent", "HERMES_HOME", "config.yaml"},
		{AgentQwen, "Qwen Code", "QWEN_HOME", "settings.json"},
	}
}

// LookupProfile returns the profile for agent.
func LookupProfile(agent Agent) (Profile, bool) {
	for _, profile := range Profiles() {
		if profile.Agent == agent {
			return profile, true
		}
	}
	return Profile{}, false
}

// ParseAgent resolves a case-insensitive agent name.
func ParseAgent(name string) (Agent, error) {
	agent := Agent(strings.ToLower(strings.TrimSpace(name)))
	if _, ok := LookupProfile(agent); !ok {
		return "", fmt.Errorf("unsupported MCP agent %q", name)
	}
	return agent, nil
}

// ConfigPath returns the user config path, honoring the agent's home override.
// Pass InstallOptions.ConfigPath to target a project config instead.
func ConfigPath(agent Agent) (string, error) {
	profile, ok := LookupProfile(agent)
	if !ok {
		return "", fmt.Errorf("unsupported MCP agent %q", agent)
	}
	dir := os.Getenv(profile.ConfigEnvironment)
	if dir != "" {
		if agent == AgentGemini {
			dir = filepath.Join(dir, ".gemini")
		}
		return filepath.Join(dir, profile.ConfigFilename), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve MCP config home: %w", err)
	}
	if agent == AgentClaude {
		return filepath.Join(home, ".claude.json"), nil
	}
	dir = filepath.Join(home, "."+string(agent))
	if agent == AgentDroid {
		dir = filepath.Join(home, ".factory")
	}
	if agent == AgentHermes && runtime.GOOS == "windows" {
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			local = filepath.Join(home, "AppData", "Local")
		}
		dir = filepath.Join(local, "hermes")
	}
	return filepath.Join(dir, profile.ConfigFilename), nil
}
