package agenthook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/atomicfile"
	"gopkg.in/yaml.v3"
)

const testMarker = "--source shared-agent-hook-test"

func TestProfilesExposeClaudeStyleEvents(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	profiles := Profiles()
	agents := make([]Agent, 0, len(profiles))
	for _, profile := range profiles {
		agents = append(agents, profile.Agent)
	}
	assert.Equal([]Agent{
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
	}, agents)
	assert.Contains(profiles[6].SupportedEvents, EventPreToolUse)
	assert.NotContains(profiles[6].SupportedEvents, EventNotification)
	for _, profile := range profiles[8:10] {
		assert.Equal(
			[]Event{EventSessionStart, EventUserPromptSubmit, EventStop, EventSessionEnd},
			profile.SupportedEvents,
		)
	}
	assert.Contains(profiles[10].SupportedEvents, EventPermissionRequest)
}

func TestPlanInstallDefaultsToEveryProfileEvent(t *testing.T) {
	assert := assert.New(t)
	result, err := PlanInstall(AgentDroid, InstallOptions{
		ConfigPath: filepath.Join(t.TempDir(), "hooks.json"),
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
	})

	require.NoError(t, err)
	assert.True(result.Changed)
	assert.Contains(string(result.Data), `"PreToolUse"`)
	assert.Contains(string(result.Data), `"PostToolUse"`)
	assert.Contains(string(result.Data), `"Stop"`)
}

func TestBuildCommandQuotesNativeAndWindowsArguments(t *testing.T) {
	assert := assert.New(t)
	commands, err := BuildCommand(
		"/opt/Example Agent/bin/hook",
		"agent-hook", "run", "--config", "/tmp/example config.toml",
	)

	require.NoError(t, err)
	if runtime.GOOS == "windows" {
		assert.Equal(commands.Windows, commands.Native)
	} else {
		assert.Equal(
			`'/opt/Example Agent/bin/hook' agent-hook run --config '/tmp/example config.toml'`,
			commands.Native,
		)
	}
	assert.Equal(
		`"/opt/Example Agent/bin/hook" agent-hook run --config "/tmp/example config.toml"`,
		commands.Windows,
	)
	assert.Equal(
		`& '/opt/Example Agent/bin/hook' 'agent-hook' 'run' '--config' '/tmp/example config.toml'`,
		commands.PowerShell,
	)
}

func TestBuildCommandQuotesPowerShellArguments(t *testing.T) {
	commands, err := BuildCommand(
		`C:\Program Files\hook.exe`,
		`a'b`, `$value;&`, "",
	)

	require.NoError(t, err)
	assert.Equal(t,
		`& 'C:\Program Files\hook.exe' 'a''b' '$value;&' ''`,
		commands.PowerShell,
	)
}

func TestPlanInstallBuildsCommandFromExecutable(t *testing.T) {
	result, err := PlanInstall(AgentClaude, InstallOptions{
		ConfigPath: filepath.Join(t.TempDir(), "settings.json"),
		Executable: "/opt/Example Agent/hook",
		Arguments:  []string{"agent-hook", "run", "--source", "shared-agent-hook-test"},
		Marker:     testMarker,
		Hooks:      []Hook{{Event: EventSessionStart}},
	})

	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(result.Data, &root))
	handler := root["hooks"].(map[string]any)["SessionStart"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if runtime.GOOS == "windows" {
		// A command string would reach Git Bash or PowerShell; exec form reaches neither.
		assert.Equal(t, "/opt/Example Agent/hook", handler["command"])
		assert.Equal(t, []any{"agent-hook", "run", "--source", "shared-agent-hook-test"}, handler["args"])
		// Ownership checks the joined argv, so a marker spanning quoted characters matches.
		opts := InstallOptions{
			ConfigPath: filepath.Join(t.TempDir(), "settings.json"),
			Executable: "/opt/Example Agent/hook",
			Arguments:  []string{"agent-hook", "--source", "owner's app"},
			Marker:     "--source owner's app",
			Hooks:      []Hook{{Event: EventStop}},
		}
		_, err := Install(AgentClaude, opts)
		require.NoError(t, err)
		again, err := Install(AgentClaude, opts)
		require.NoError(t, err)
		assert.False(t, again.Changed)
		return
	}
	assert.Equal(t, "'/opt/Example Agent/hook' agent-hook run --source shared-agent-hook-test", handler["command"])
	assert.NotContains(t, handler, "args")
}

func TestPlanInstallRejectsWindowsShim(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("shims need a shell only on Windows")
	}
	for _, agent := range []Agent{AgentClaude, AgentPi} {
		for _, executable := range []string{`C:\tools\hook.cmd`, `C:\tools\hook.BAT`} {
			t.Run(string(agent)+" "+executable, func(t *testing.T) {
				_, err := PlanInstall(agent, InstallOptions{
					ConfigPath: filepath.Join(t.TempDir(), "hook-config"),
					Executable: executable,
					Arguments:  []string{"agent-hook", "run", "--source", "shared-agent-hook-test"},
					Marker:     testMarker,
				})

				require.ErrorContains(t, err, "pass the executable it launches")
			})
		}
	}
}

func TestUninstallMatchesMarkerAcrossExecFormArguments(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(os.WriteFile(path, []byte(`{"hooks": {"Stop": [{"hooks": [
  {"type": "command", "command": "C:\\hook.exe", "args": ["agent-hook", "--source", "shared-agent-hook-test"]},
  {"type": "command", "command": "keep-me"}
]}]}}`), 0o600))

	result, err := Uninstall(AgentClaude, path, testMarker)

	require.NoError(err)
	assert.NotContains(string(result.Data), "agent-hook")
	assert.Contains(string(result.Data), "keep-me")
}

func TestConfigPathHonorsAgentHomes(t *testing.T) {
	tests := []struct {
		agent Agent
		env   string
		path  string
	}{
		{agent: AgentClaude, env: "CLAUDE_CONFIG_DIR", path: "settings.json"},
		{agent: AgentCodex, env: "CODEX_HOME", path: "hooks.json"},
		{agent: AgentCopilot, env: "COPILOT_HOME", path: filepath.Join("hooks", "agenthook.json")},
		{agent: AgentGemini, env: "GEMINI_CLI_HOME", path: filepath.Join(".gemini", "settings.json")},
		{agent: AgentHermes, env: "HERMES_HOME", path: "config.yaml"},
		{agent: AgentMuse, env: "XDG_CONFIG_HOME", path: filepath.Join("muse", "settings.json")},
		{agent: AgentOpenCode, env: "OPENCODE_CONFIG_DIR", path: filepath.Join("plugins", "agenthook", "tui.js")},
		{agent: AgentOpenCode, env: "XDG_CONFIG_HOME", path: filepath.Join("opencode", "plugins", "agenthook", "tui.js")},
		{agent: AgentQwen, env: "QWEN_HOME", path: "settings.json"},
	}
	for _, tt := range tests {
		t.Run(string(tt.agent)+" "+tt.env, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(profiles[tt.agent].profile.ConfigEnvironment, "")
			t.Setenv(tt.env, dir)

			path, err := ConfigPath(tt.agent)

			require.NoError(t, err)
			assert.Equal(t, filepath.Join(dir, tt.path), path)
		})
	}
}

func TestInstallJSONPreservesOtherHooksAndReplacesOwnedHooks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	oldCommand := "/old/middleman agent-hook run " + testMarker
	require.NoError(os.WriteFile(path, []byte(`{
  "sequence": 9007199254740993,
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "keep-me"}]}],
    "SessionStart": [{"hooks": [{"type": "command", "command": "`+oldCommand+`"}]}]
  }
}`), 0o640))
	// Establish the mode being preserved regardless of the process umask.
	require.NoError(os.Chmod(path, 0o640))
	command := "/opt/middleman agent-hook run " + testMarker
	opts := InstallOptions{
		ConfigPath: path,
		Command:    command,
		CommandWindows: `C:\Program Files\middleman.exe agent-hook run ` +
			testMarker,
		Marker: testMarker,
		Hooks: []Hook{
			{Event: EventPreToolUse, Matcher: ToolBash, Timeout: 2 * time.Second},
			{Event: EventStop, Timeout: 2 * time.Second},
		},
	}

	result, err := Install(AgentCodex, opts)
	require.NoError(err)
	assert.True(result.Changed)
	info, err := os.Stat(path)
	require.NoError(err)
	if runtime.GOOS != "windows" {
		assert.Equal(os.FileMode(0o640), info.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	require.NoError(err)
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var root map[string]any
	require.NoError(decoder.Decode(&root))
	assert.Equal(json.Number("9007199254740993"), root["sequence"])
	hooks := root["hooks"].(map[string]any)
	assert.NotContains(hooks, "SessionStart")
	assert.Contains(string(data), "keep-me")
	assert.Contains(string(data), `"matcher": "^Bash$"`)
	assert.Contains(string(data), `"commandWindows"`)
	assert.Equal(2, strings.Count(string(data), `"command": "`+command+`"`))

	result, err = Install(AgentCodex, opts)
	require.NoError(err)
	assert.False(result.Changed)

	result, err = Uninstall(AgentCodex, path, testMarker)
	require.NoError(err)
	assert.True(result.Changed)
	data, err = os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(data), "keep-me")
	assert.NotContains(string(data), testMarker)
}

func TestInstallReturnsResultWhenConfigWasPublishedButNotDurable(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	original := writeAtomicFile
	writeAtomicFile = func(path string, data []byte, opts ...atomicfile.Option) error {
		if err := original(path, data, opts...); err != nil {
			return err
		}
		return fmt.Errorf("%w: injected directory sync failure", atomicfile.ErrNotDurable)
	}
	t.Cleanup(func() { writeAtomicFile = original })

	result, err := Install(AgentCodex, InstallOptions{
		ConfigPath: path,
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
		Hooks:      []Hook{{Event: EventStop}},
	})

	require.ErrorIs(err, atomicfile.ErrNotDurable)
	assert.True(t, result.Changed)
	assert.Equal(t, path, result.ConfigPath)
	data, readErr := os.ReadFile(path)
	require.NoError(readErr)
	assert.Equal(t, result.Data, data)
}

func TestPlanInstallQwenUsesClaudeEventsAndMillisecondTimeouts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	result, err := PlanInstall(AgentQwen, InstallOptions{
		ConfigPath: filepath.Join(t.TempDir(), "settings.json"),
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
		Hooks: []Hook{{
			Event: EventPreToolUse, Matcher: ToolBash, Timeout: 2 * time.Second,
		}},
	})

	require.NoError(err)
	var root map[string]any
	require.NoError(json.Unmarshal(result.Data, &root))
	hooks := root["hooks"].(map[string]any)
	entries := hooks["PreToolUse"].([]any)
	entry := entries[0].(map[string]any)
	assert.Equal("run_shell_command", entry["matcher"])
	handlers := entry["hooks"].([]any)
	handler := handlers[0].(map[string]any)
	assert.InDelta(float64(2000), handler["timeout"], 0)
}

func TestInstallMuseCreatesStartableSettings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	opts := InstallOptions{
		ConfigPath:     path,
		Command:        "/opt/hook " + testMarker,
		CommandWindows: `C:\hook.exe ` + testMarker,
		Marker:         testMarker,
		Hooks: []Hook{{
			Event: EventPreToolUse, Matcher: ToolBash, Timeout: 2 * time.Second,
		}},
	}

	_, err := Install(AgentMuse, opts)

	require.NoError(err)
	data, err := os.ReadFile(path)
	require.NoError(err)
	assert.JSONEq(`{
  "schema_version": 1,
  "hooks": {
    "PreToolUse": [{
      "matcher": "bash",
      "hooks": [{
        "type": "command",
        "command": "/opt/hook `+testMarker+`",
        "commandWindows": "C:\\hook.exe `+testMarker+`",
        "timeout": 2
      }]
    }]
  }
}`, string(data))

	_, err = Uninstall(AgentMuse, path, testMarker)
	require.NoError(err)
	data, err = os.ReadFile(path)
	require.NoError(err)
	assert.JSONEq(`{"schema_version": 1, "hooks": {}}`, string(data))
}

func TestInstallMuseKeepsExistingSettingsFields(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(os.WriteFile(path, []byte(`{"schema_version": 1, "model": "keep"}`), 0o600))

	result, err := PlanInstall(AgentMuse, InstallOptions{
		ConfigPath: path,
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
		Hooks:      []Hook{{Event: EventStop}},
	})

	require.NoError(err)
	var root map[string]any
	require.NoError(json.Unmarshal(result.Data, &root))
	assert.Equal(t, "keep", root["model"])
	assert.InDelta(t, float64(1), root["schema_version"], 0)
}

func TestPlanInstallAcceptsWholeMillisecondTimeouts(t *testing.T) {
	for _, agent := range []Agent{AgentGemini, AgentQwen} {
		t.Run(string(agent), func(t *testing.T) {
			result, err := PlanInstall(agent, InstallOptions{
				ConfigPath: filepath.Join(t.TempDir(), "settings.json"),
				Command:    "/opt/hook " + testMarker,
				Marker:     testMarker,
				Hooks: []Hook{{
					Event: EventPreToolUse, Timeout: 1500 * time.Millisecond,
				}},
			})

			require.NoError(t, err)
			assert.Contains(t, string(result.Data), `"timeout": 1500`)
		})
	}
}

func TestPlanInstallGeminiTranslatesClaudeEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	result, err := PlanInstall(AgentGemini, InstallOptions{
		ConfigPath: filepath.Join(t.TempDir(), "settings.json"),
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
		Hooks: []Hook{
			{Event: EventUserPromptSubmit},
			{Event: EventPreToolUse, Matcher: ToolBash, Timeout: 2 * time.Second},
			{Event: EventPostToolUse},
			{Event: EventStop},
		},
	})

	require.NoError(err)
	var root map[string]any
	require.NoError(json.Unmarshal(result.Data, &root))
	hooks := root["hooks"].(map[string]any)
	assert.Contains(hooks, "BeforeAgent")
	assert.Contains(hooks, "BeforeTool")
	assert.Contains(hooks, "AfterTool")
	assert.Contains(hooks, "AfterAgent")
	assert.NotContains(hooks, "UserPromptSubmit")
	beforeTool := hooks["BeforeTool"].([]any)[0].(map[string]any)
	assert.Equal("run_shell_command", beforeTool["matcher"])
	handler := beforeTool["hooks"].([]any)[0].(map[string]any)
	assert.InDelta(float64(2000), handler["timeout"], 0)
}

func TestInstallCopilotUsesDirectEntriesAndPreservesOtherHooks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "agenthook.json")
	oldCommand := "/old/hook " + testMarker
	require.NoError(os.WriteFile(path, []byte(`{
  "version": 1,
  "future": "keep-top-level",
  "hooks": {
    "Stop": [{"type": "command", "command": "keep-me"}],
    "SessionStart": [{"type": "command", "bash": "`+oldCommand+`"}]
  }
}`), 0o600))
	command := "/opt/hook " + testMarker
	commandPowerShell := `& 'C:\Program Files\hook.exe' '--source' 'shared-agent-hook-test'`
	opts := InstallOptions{
		ConfigPath:        path,
		Command:           command,
		CommandPowerShell: commandPowerShell,
		Marker:            testMarker,
		Hooks: []Hook{
			{Event: EventPreToolUse, Matcher: ToolBash, Timeout: 2 * time.Second},
			{Event: EventStop},
		},
	}

	result, err := Install(AgentCopilot, opts)
	require.NoError(err)
	assert.True(result.Changed)
	var root map[string]any
	require.NoError(json.Unmarshal(result.Data, &root))
	assert.InDelta(float64(1), root["version"], 0)
	assert.Equal("keep-top-level", root["future"])
	hooks := root["hooks"].(map[string]any)
	assert.NotContains(hooks, "SessionStart")
	assert.Len(hooks["Stop"], 2)
	preTool := hooks["PreToolUse"].([]any)[0].(map[string]any)
	assert.Equal("command", preTool["type"])
	assert.Equal("Bash", preTool["matcher"])
	assert.Equal(command, preTool["bash"])
	assert.Equal(commandPowerShell, preTool["powershell"])
	assert.InDelta(float64(2), preTool["timeoutSec"], 0)

	result, err = Install(AgentCopilot, opts)
	require.NoError(err)
	assert.False(result.Changed)

	result, err = Uninstall(AgentCopilot, path, testMarker)
	require.NoError(err)
	assert.True(result.Changed)
	assert.Contains(string(result.Data), "keep-me")
	assert.NotContains(string(result.Data), testMarker)
}

func TestPlanInstallCopilotBuildsBashAndPowerShellCommands(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	result, err := PlanInstall(AgentCopilot, InstallOptions{
		ConfigPath: filepath.Join(t.TempDir(), "agenthook.json"),
		Executable: `C:\Program Files\hook.exe`,
		Arguments:  []string{"agent-hook", "run", "--source", "shared-agent-hook-test"},
		Marker:     testMarker,
		Hooks:      []Hook{{Event: EventSessionStart}},
	})

	require.NoError(err)
	var root map[string]any
	require.NoError(json.Unmarshal(result.Data, &root))
	entry := root["hooks"].(map[string]any)["SessionStart"].([]any)[0].(map[string]any)
	assert.Equal(
		`'C:\Program Files\hook.exe' agent-hook run --source shared-agent-hook-test`,
		entry["bash"],
	)
	assert.Equal(
		`& 'C:\Program Files\hook.exe' 'agent-hook' 'run' '--source' 'shared-agent-hook-test'`,
		entry["powershell"],
	)
}

func TestCopilotCommandSelectionKeepsBashPOSIXOnWindows(t *testing.T) {
	native, windows := profileCommands(copilotProfile(), Commands{
		Native:     `"C:\Program Files\hook.exe" run`,
		POSIX:      `'C:\Program Files\hook.exe' run`,
		Windows:    `"C:\Program Files\hook.exe" run`,
		PowerShell: `& 'C:\Program Files\hook.exe' 'run'`,
	})

	assert.Equal(t, `'C:\Program Files\hook.exe' run`, native)
	assert.Equal(t, `& 'C:\Program Files\hook.exe' 'run'`, windows)
}

func TestPlanInstallCopilotDistinguishesWin32AndPowerShellOverrides(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "agenthook.json")

	result, err := PlanInstall(AgentCopilot, InstallOptions{
		ConfigPath:     path,
		Command:        "/opt/hook " + testMarker,
		CommandWindows: `"C:\Program Files\hook.exe" --source shared-agent-hook-test`,
		Marker:         testMarker,
		Hooks:          []Hook{{Event: EventSessionStart}},
	})

	require.NoError(err)
	var root map[string]any
	require.NoError(json.Unmarshal(result.Data, &root))
	entry := root["hooks"].(map[string]any)["SessionStart"].([]any)[0].(map[string]any)
	assert.Equal("/opt/hook "+testMarker, entry["command"])
	assert.NotContains(entry, "powershell")

	result, err = PlanInstall(AgentCopilot, InstallOptions{
		ConfigPath:        path,
		Command:           "/opt/hook " + testMarker,
		CommandPowerShell: `& 'C:\Program Files\hook.exe' '--source' 'shared-agent-hook-test'`,
		Marker:            testMarker,
		Hooks:             []Hook{{Event: EventSessionStart}},
	})

	require.NoError(err)
	require.NoError(json.Unmarshal(result.Data, &root))
	entry = root["hooks"].(map[string]any)["SessionStart"].([]any)[0].(map[string]any)
	assert.Equal("/opt/hook "+testMarker, entry["bash"])
	assert.Equal(
		`& 'C:\Program Files\hook.exe' '--source' 'shared-agent-hook-test'`,
		entry["powershell"],
	)
}

func TestPlanDirectJSONRejectsUnsupportedSchemaVersions(t *testing.T) {
	for _, agent := range []Agent{AgentCopilot, AgentCursor} {
		for _, version := range []string{`2`, `"1"`, `true`} {
			t.Run(fmt.Sprintf("%s/%s", agent, version), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "hooks.json")
				require.NoError(t, os.WriteFile(path, fmt.Appendf(nil,
					`{"version":%s,"hooks":{}}`, version), 0o600))

				_, err := PlanInstall(agent, InstallOptions{
					ConfigPath: path,
					Command:    "/opt/hook " + testMarker,
					Marker:     testMarker,
					Hooks:      []Hook{{Event: EventSessionStart}},
				})

				require.Error(t, err)
				assert.ErrorContains(t, err, "version must be 1")
			})
		}
	}
}

func TestPlanDirectJSONAcceptsNumericSchemaVersionOne(t *testing.T) {
	for _, version := range []string{`1`, `1.0`, `1e0`} {
		t.Run(version, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hooks.json")
			require.NoError(t, os.WriteFile(path, fmt.Appendf(nil,
				`{"version":%s,"hooks":{}}`, version), 0o600))

			_, err := PlanInstall(AgentCursor, InstallOptions{
				ConfigPath: path,
				Command:    "/opt/hook " + testMarker,
				Marker:     testMarker,
				Hooks:      []Hook{{Event: EventSessionStart}},
			})

			require.NoError(t, err)
		})
	}
}

func TestPlanInstallCursorUsesNativeDirectEntries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	result, err := PlanInstall(AgentCursor, InstallOptions{
		ConfigPath: filepath.Join(t.TempDir(), "hooks.json"),
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
		Hooks: []Hook{
			{Event: EventUserPromptSubmit},
			{Event: EventPreToolUse, Matcher: ToolBash, Timeout: 2 * time.Second},
			{Event: EventPostToolUseFailure},
			{Event: EventStop},
		},
	})

	require.NoError(err)
	var root map[string]any
	require.NoError(json.Unmarshal(result.Data, &root))
	assert.InDelta(float64(1), root["version"], 0)
	hooks := root["hooks"].(map[string]any)
	assert.Contains(hooks, "beforeSubmitPrompt")
	assert.Contains(hooks, "postToolUseFailure")
	assert.Contains(hooks, "stop")
	beforePrompt := hooks["beforeSubmitPrompt"].([]any)[0].(map[string]any)
	assert.Equal(true, beforePrompt["failClosed"])
	preTool := hooks["preToolUse"].([]any)[0].(map[string]any)
	assert.Equal("command", preTool["type"])
	assert.Equal("Shell", preTool["matcher"])
	assert.Equal("/opt/hook "+testMarker, preTool["command"])
	assert.InDelta(float64(2), preTool["timeout"], 0)
	assert.Equal(true, preTool["failClosed"])
	stop := hooks["stop"].([]any)[0].(map[string]any)
	assert.NotContains(stop, "failClosed")
}

func TestInstallHermesTranslatesEventsAndPreservesYAML(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	oldCommand := "/old/middleman agent-hook run " + testMarker
	require.NoError(os.WriteFile(path, []byte(`# keep this operator note
model: test-model
hooks_auto_accept: false
hooks:
  pre_tool_call:
    - matcher: web_search
      command: keep-me
  transform_tool_result:
    - command: keep-transform
      future_field: keep-extra
  on_session_start:
    - command: "`+oldCommand+`"
`), 0o600))
	command := "/opt/middleman agent-hook run " + testMarker
	opts := InstallOptions{
		ConfigPath: path,
		Command:    command,
		Marker:     testMarker,
		Hooks: []Hook{
			{Event: EventPreToolUse, Matcher: ToolBash, Timeout: 2 * time.Second},
			{Event: EventStop, Timeout: 2 * time.Second},
		},
	}

	result, err := Install(AgentHermes, opts)
	require.NoError(err)
	assert.True(result.Changed)
	data, err := os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(data), "# keep this operator note")
	assert.Contains(string(data), "keep-me")
	assert.NotContains(string(data), oldCommand)

	var root map[string]any
	require.NoError(yaml.Unmarshal(data, &root))
	hooks := root["hooks"].(map[string]any)
	assert.NotContains(hooks, "on_session_start")
	transform := hooks["transform_tool_result"].([]any)[0].(map[string]any)
	assert.Equal("keep-extra", transform["future_field"])
	preTool := hooks["pre_tool_call"].([]any)
	assert.Len(preTool, 2)
	installed := preTool[1].(map[string]any)
	assert.Equal("terminal", installed["matcher"])
	assert.Equal(command, installed["command"])
	assert.Equal(2, installed["timeout"])
	stop := hooks["pre_verify"].([]any)[0].(map[string]any)
	assert.Equal(command, stop["command"])

	result, err = Install(AgentHermes, opts)
	require.NoError(err)
	assert.False(result.Changed)

	result, err = Uninstall(AgentHermes, path, testMarker)
	require.NoError(err)
	assert.True(result.Changed)
	data, err = os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(data), "keep-me")
	assert.NotContains(string(data), testMarker)
}

func TestPlanUninstallHermesPreservesUnrelatedHookNodes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(os.WriteFile(path, []byte(`hooks:
  # keep event note
  pre_tool_call:
    # keep entry note
    - matcher: terminal # keep matcher note
      command: keep-me
  post_tool_call:
    - command: keep-after
`), 0o600))

	result, err := PlanUninstall(AgentHermes, path, testMarker)

	require.NoError(err)
	assert.False(result.Changed)
	data := string(result.Data)
	assert.Contains(data, "# keep event note")
	assert.Contains(data, "# keep entry note")
	assert.Contains(data, "# keep matcher note")
	assert.Less(strings.Index(data, "pre_tool_call"), strings.Index(data, "post_tool_call"))
}

func TestPlanUninstallHermesResolvesAliasedHooks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(os.WriteFile(path, []byte(`shared_hooks: &shared_hooks
  - &owned
    command: "/opt/hook --source shared-agent-hook-test"
hooks:
  pre_tool_call: *shared_hooks
  post_tool_call:
    - *owned
`), 0o600))

	result, err := PlanUninstall(AgentHermes, path, testMarker)

	require.NoError(err)
	assert.True(result.Changed)
	var root map[string]any
	require.NoError(yaml.Unmarshal(result.Data, &root))
	hooks := root["hooks"].(map[string]any)
	assert.NotContains(hooks, "pre_tool_call")
	assert.NotContains(hooks, "post_tool_call")
}

func TestPlanInstallHermesRetainsOwnedEventNodeMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(os.WriteFile(path, []byte(`hooks:
  # keep event note
  pre_tool_call: &pre_tool_hooks
    - command: "/opt/hook --source shared-agent-hook-test" # old hook
  post_tool_call:
    - command: keep-after
`), 0o600))

	result, err := PlanInstall(AgentHermes, InstallOptions{
		ConfigPath: path,
		Command:    "/new/hook " + testMarker,
		Marker:     testMarker,
		Hooks:      []Hook{{Event: EventPreToolUse, Matcher: ToolBash}},
	})

	require.NoError(err)
	data := string(result.Data)
	assert.Contains(data, "# keep event note")
	assert.Contains(data, "&pre_tool_hooks")
	assert.Less(strings.Index(data, "pre_tool_call"), strings.Index(data, "post_tool_call"))
}

func TestNormalizeConvertsNativePayloadToClaudeShape(t *testing.T) {
	tests := []struct {
		name  string
		agent Agent
		input string
		want  map[string]any
	}{
		{
			name:  "claude payload stays canonical",
			agent: AgentClaude,
			input: `{"session_id":"s1","hook_event_name":"PreToolUse","tool_name":"Bash","future":9007199254740993}`,
			want: map[string]any{
				"session_id": "s1", "hook_event_name": "PreToolUse", "tool_name": "Bash",
				"future": json.Number("9007199254740993"),
			},
		},
		{
			name:  "cursor aliases",
			agent: AgentCursor,
			input: `{"conversation_id":"c1","generation_id":"g1","hook_event_name":"postToolUse","tool_name":"Shell","tool_input":{"command":"go test ./..."},"tool_output":{"exitCode":0}}`,
			want: map[string]any{
				"session_id": "c1", "hook_event_name": "PostToolUse", "tool_name": "Bash",
				"tool_response":   map[string]any{"exitCode": json.Number("0")},
				"conversation_id": "c1", "generation_id": "g1",
			},
		},
		{
			name:  "gemini event and response aliases",
			agent: AgentGemini,
			input: `{"session_id":"g1","hook_event_name":"AfterAgent","prompt_response":"finished","stop_hook_active":true}`,
			want: map[string]any{
				"session_id": "g1", "hook_event_name": "Stop",
				"last_assistant_message": "finished", "stop_hook_active": true,
			},
		},
		{
			name:  "hermes promotes extra tool fields",
			agent: AgentHermes,
			input: `{"session_id":"h1","hook_event_name":"post_tool_call","tool_name":"terminal","tool_input":{"command":"pwd"},"extra":{"result":"/tmp","tool_call_id":"call-1","turn_id":"turn-1","status":"ok"}}`,
			want: map[string]any{
				"session_id": "h1", "hook_event_name": "PostToolUse", "tool_name": "Bash",
				"tool_use_id": "call-1", "turn_id": "turn-1", "tool_response": "/tmp",
				"extra": map[string]any{
					"result": "/tmp", "tool_call_id": "call-1", "turn_id": "turn-1", "status": "ok",
				},
			},
		},
		{
			name:  "pi session start reason",
			agent: AgentPi,
			input: `{"session_id":"p1","hook_event_name":"session_start","reason":"startup"}`,
			want:  map[string]any{"session_id": "p1", "hook_event_name": "SessionStart", "source": "startup"},
		},
		{
			name:  "pi new session clears",
			agent: AgentPi,
			input: `{"session_id":"p1","hook_event_name":"session_start","reason":"new"}`,
			want:  map[string]any{"hook_event_name": "SessionStart", "source": "clear"},
		},
		{
			name:  "pi session replaced by resume",
			agent: AgentPi,
			input: `{"session_id":"p1","hook_event_name":"session_shutdown","reason":"resume"}`,
			want:  map[string]any{"hook_event_name": "SessionEnd", "reason": "resume"},
		},
		{
			name:  "pi session replaced by fork",
			agent: AgentPi,
			input: `{"session_id":"p1","hook_event_name":"session_shutdown","reason":"fork"}`,
			want:  map[string]any{"hook_event_name": "SessionEnd", "reason": "other"},
		},
		{
			name:  "pi prompt",
			agent: AgentPi,
			input: `{"session_id":"p1","hook_event_name":"before_agent_start","prompt":"fix it"}`,
			want:  map[string]any{"session_id": "p1", "hook_event_name": "UserPromptSubmit", "prompt": "fix it"},
		},
		{
			name:  "muse shell tool",
			agent: AgentMuse,
			input: `{"session_id":"m1","hook_event_name":"PreToolUse","tool_name":"bash"}`,
			want: map[string]any{
				"session_id": "m1", "hook_event_name": "PreToolUse", "tool_name": "Bash",
			},
		},
		{
			name:  "qwen shell tool",
			agent: AgentQwen,
			input: `{"session_id":"q1","hook_event_name":"PreToolUse","tool_name":"run_shell_command"}`,
			want: map[string]any{
				"session_id": "q1", "hook_event_name": "PreToolUse", "tool_name": "Bash",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := normalize(tt.agent, strings.NewReader(tt.input))

			require.NoError(t, err)
			decoder := json.NewDecoder(strings.NewReader(string(data)))
			decoder.UseNumber()
			var got map[string]any
			require.NoError(t, decoder.Decode(&got))
			for key, want := range tt.want {
				assert.Equal(t, want, got[key], key)
			}
		})
	}
}

func TestNormalizeKeepsCanonicalFieldsOverAliases(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	data, err := normalize(AgentCursor, strings.NewReader(`{
  "session_id":"canonical",
  "conversation_id":"native",
  "hook_event_name":"preToolUse",
  "tool_response":{"canonical":true},
  "tool_output":{"native":true}
}`))

	require.NoError(err)
	var got map[string]any
	require.NoError(json.Unmarshal(data, &got))
	assert.Equal("canonical", got["session_id"])
	assert.Equal(map[string]any{"canonical": true}, got["tool_response"])
}

func TestNormalizeRejectsInvalidInput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, err := normalize(Agent("unknown"), strings.NewReader(`{}`))
	require.Error(err)
	require.ErrorContains(err, "unsupported agent hook integration")

	_, err = normalize(AgentClaude, strings.NewReader(`{"session_id":"s1"} {}`))
	require.Error(err)
	require.ErrorContains(err, "multiple JSON values")

	_, err = normalize(AgentHermes, strings.NewReader(`{
  "session_id":"h1",
  "hook_event_name":"post_tool_call",
  "extra":[]
}`))
	require.Error(err)
	assert.ErrorContains(err, `field "extra" must be an object`)
}

func TestHermesRejectsUnsupportedClaudeStyleHooks(t *testing.T) {
	tests := []struct {
		name string
		hook Hook
		want string
	}{
		{
			name: "event",
			hook: Hook{Event: EventNotification},
			want: "does not support Notification",
		},
		{
			name: "matcher",
			hook: Hook{Event: EventStop, Matcher: ToolBash},
			want: "only supports matchers",
		},
		{
			name: "timeout",
			hook: Hook{Event: EventStop, Timeout: 301 * time.Second},
			want: "must not exceed 300 seconds",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := PlanInstall(AgentHermes, InstallOptions{
				ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
				Command:    "/opt/hook " + testMarker,
				Marker:     testMarker,
				Hooks:      []Hook{tt.hook},
			})

			require.Error(t, err)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

func TestInstallPreservesConfigSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated privileges on Windows")
	}
	assert := assert.New(t)
	require := require.New(t)
	target := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(os.WriteFile(target, []byte(`{"hooks":{}}`), 0o600))
	link := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(os.Symlink(target, link))

	_, err := Install(AgentClaude, InstallOptions{
		ConfigPath: link,
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
		Hooks:      []Hook{{Event: EventSessionStart}},
	})

	require.NoError(err)
	info, err := os.Lstat(link)
	require.NoError(err)
	assert.NotZero(info.Mode() & os.ModeSymlink)
	data, err := os.ReadFile(target)
	require.NoError(err)
	assert.Contains(string(data), testMarker)
}

func TestWriteConfigRefusesLinkSwappedInForRegularConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink may need a privilege Windows CI lacks")
	}
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	other := filepath.Join(dir, "other.json")
	require.NoError(os.WriteFile(path, []byte("{}"), 0o600))
	require.NoError(os.WriteFile(other, []byte("other"), 0o600))
	original := writeAtomicFile
	writeAtomicFile = func(path string, data []byte, opts ...atomicfile.Option) error {
		// Swap the regular config for a link after writeConfig inspected it.
		require.NoError(os.Remove(path))
		require.NoError(os.Symlink(other, path))
		return original(path, data, opts...)
	}
	t.Cleanup(func() { writeAtomicFile = original })

	err := writeConfig(path, []byte("new"))

	require.Error(err)
	data, err := os.ReadFile(other)
	require.NoError(err)
	assert.Equal(t, "other", string(data))
}

func TestConfigPathNormalizesPiAgentDirAsPiDoes(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	windows := runtime.GOOS == "windows"
	pick := func(onWindows, elsewhere string) string {
		if windows {
			return onWindows
		}
		return elsewhere
	}
	tests := []struct {
		env   string
		want  string
		fails bool
	}{
		{env: "~", want: home},
		{env: "~/pi-agent", want: filepath.Join(home, "pi-agent")},
		{env: `~\pi-agent`, want: pick(filepath.Join(home, "pi-agent"), `~\pi-agent`)},
		{env: "/c/Users/me/pi", want: pick(`C:\Users\me\pi`, "/c/Users/me/pi")},
		{env: "/mnt/d/pi", want: pick(`D:\pi`, "/mnt/d/pi")},
		{env: "/cygdrive/e", want: pick(`E:\`, "/cygdrive/e")},
		{env: "//server/share", want: "//server/share"},
		// File URLs resolve as Node's fileURLToPath does, and fail where it throws.
		{
			env:  "file:///C:/Users/me/pi%20agent",
			want: pick(`C:\Users\me\pi agent`, "/C:/Users/me/pi agent"),
		},
		{env: "file://localhost/srv/pi", want: "/srv/pi", fails: windows},
		{env: "file://LOCALHOST/srv/pi", want: "/srv/pi", fails: windows},
		{env: "file://C:/Users/me/pi", want: pick(`C:\Users\me\pi`, "/C:/Users/me/pi")},
		{env: "file:///c|/Users/me/pi", want: pick(`c:\Users\me\pi`, "/c:/Users/me/pi")},
		{env: "file://server/share/pi", want: `\\server\share\pi`, fails: !windows},
		{env: "file://SERVER/share/pi", want: `\\server\share\pi`, fails: !windows},
		{env: "file:///srv/pi%2Fagent", fails: true},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			t.Setenv("PI_CODING_AGENT_DIR", tt.env)

			path, err := ConfigPath(AgentPi)

			if tt.fails {
				require.ErrorContains(t, err, "file URL")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(tt.want, "extensions", "agenthook.js"), path)
		})
	}
}

// scriptHooks parses the registration block of a generated script module.
func scriptHooks(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	block, err := scriptBlock(data, path)
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(block, &root))
	hooks, _ := root["hooks"].(map[string]any)
	return hooks
}

func scriptCommands(hooks map[string]any, event string) []string {
	var commands []string
	entries, _ := hooks[event].([]any)
	for _, entry := range entries {
		handlers, _ := entry.(map[string]any)["hooks"].([]any)
		for _, handler := range handlers {
			fields, _ := handler.(map[string]any)
			command, _ := fields["command"].(string)
			argv := []string{command}
			args, _ := fields["args"].([]any)
			for _, arg := range args {
				argv = append(argv, arg.(string))
			}
			commands = append(commands, strings.Join(argv, " "))
		}
	}
	return commands
}

func TestInstallPiKeepsOtherApplicationsCommands(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "extensions", "agenthook.js")
	install := func(executable, source string, extra ...string) Result {
		result, err := Install(AgentPi, InstallOptions{
			ConfigPath: path,
			Executable: executable,
			Arguments:  append(append([]string{"agent-hook"}, extra...), "--source", source),
			Marker:     "--source " + source,
		})
		require.NoError(err)
		return result
	}
	// B's argument is a block delimiter, which must not end the registration block.
	const bCommand = "/opt/b agent-hook " + scriptBlockEnd + " --source b-hook"

	install("/opt/a", "a-hook")
	install("/opt/b", "b-hook", scriptBlockEnd)
	assert.Equal(
		[]string{"/opt/a agent-hook --source a-hook", bCommand},
		scriptCommands(scriptHooks(t, path), "session_start"),
	)
	assert.False(install("/opt/b", "b-hook", scriptBlockEnd).Changed)

	install("/moved/a", "a-hook")
	hooks := scriptHooks(t, path)
	assert.Equal(
		[]string{bCommand, "/moved/a agent-hook --source a-hook"},
		scriptCommands(hooks, "agent_settled"),
	)
	assert.Len(scriptCommands(hooks, "before_agent_start"), 2)

	result, err := Uninstall(AgentPi, path, "--source a-hook")
	require.NoError(err)
	assert.True(result.Changed)
	assert.Equal([]string{bCommand}, scriptCommands(scriptHooks(t, path), "session_start"))

	// Removing the last hook deletes the module so Pi stops loading it; a plan
	// reports that without touching the file.
	result, err = PlanUninstall(AgentPi, path, "--source b-hook")
	require.NoError(err)
	assert.True(result.Changed)
	assert.Nil(result.Data)
	assert.FileExists(path)

	result, err = Uninstall(AgentPi, path, "--source b-hook")
	require.NoError(err)
	assert.True(result.Changed)
	assert.NoFileExists(path)

	result, err = Uninstall(AgentPi, filepath.Join(t.TempDir(), "missing.js"), "--source b-hook")
	require.NoError(err)
	assert.False(result.Changed)
}

func TestPlanInstallPiRefusesForeignFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agenthook.js")
	original := []byte("export default function (pi) {}\n")
	require.NoError(t, os.WriteFile(path, original, 0o600))

	_, err := Install(AgentPi, InstallOptions{
		ConfigPath: path,
		Executable: "/opt/hook",
		Arguments:  []string{"--source", "shared-agent-hook-test"},
		Marker:     testMarker,
	})

	require.ErrorContains(t, err, "not written by agenthook")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, data)
}

func TestPlanInstallPiRequiresExecutableWithoutMatchers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agenthook.js")

	_, err := PlanInstall(AgentPi, InstallOptions{
		ConfigPath: path,
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
	})
	require.ErrorContains(t, err, "need Executable and Arguments")

	_, err = PlanInstall(AgentPi, InstallOptions{
		ConfigPath: path,
		Executable: "/opt/hook",
		Arguments:  []string{"--source", "shared-agent-hook-test"},
		Marker:     testMarker,
		Hooks:      []Hook{{Event: EventStop, Matcher: ToolBash}},
	})
	require.ErrorContains(t, err, "do not support matchers")
}

func TestScriptHookHelper(t *testing.T) {
	if os.Getenv("KIT_AGENTHOOK_PI_HELPER_LINGER") != "" {
		// A background process the hook started, holding the hook's stderr open.
		<-time.After(4 * time.Minute)
		return
	}
	out := os.Getenv("KIT_AGENTHOOK_HELPER_OUT")
	if out == "" {
		return
	}
	payload, err := io.ReadAll(os.Stdin)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(payload, &fields))
	fields["helper_cwd"], err = os.Getwd()
	require.NoError(t, err)
	payload, err = json.Marshal(fields)
	require.NoError(t, err)
	file, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = file.Write(append(payload, '\n'))
	require.NoError(t, err)
	require.NoError(t, file.Close())
	switch fields["hook_event_name"] {
	case "before_agent_start":
		// Exit at once, leaving a process that inherits stderr. It must outlive
		// this test, so it gets no cancellation; the test kills it by its PID.
		// It runs in the test binary's directory, outside the session's, so
		// Windows can remove the session's directory.
		linger := exec.CommandContext(
			context.WithoutCancel(t.Context()), os.Args[0], "-test.run=^TestScriptHookHelper$",
		)
		linger.Env = append(os.Environ(), "KIT_AGENTHOOK_PI_HELPER_LINGER=1")
		linger.Dir = filepath.Dir(os.Args[0])
		linger.Stderr = os.Stderr
		require.NoError(t, linger.Start())
		pid := []byte(strconv.Itoa(linger.Process.Pid))
		require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(out), "linger.pid"), pid, 0o600))
		require.NoError(t, linger.Process.Release())
	case "agent_settled":
		// Ignore SIGTERM and outlive the 1s hook timeout, so only a forced kill
		// with an unconditional deadline keeps the extension from waiting.
		fmt.Fprintln(os.Stderr, "helper still settling")
		signal.Ignore(syscall.SIGTERM)
		<-time.After(4 * time.Minute)
	}
}

const piExtensionDriver = `
import { existsSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
const extension = await import(pathToFileURL(process.argv[2]).href);
const fileA = join(process.argv[3], "a.jsonl");
const fileB = join(process.argv[3], "b.jsonl");
const runtime = () => {
	const handlers = {};
	extension.default({ on: (name, handler) => { handlers[name] = handler; } });
	return handlers;
};
const ctx = (id, file, mode = "tui") => ({
	mode,
	cwd: process.argv[3],
	sessionManager: { getSessionId: () => id, getSessionFile: () => file },
});
const fire = async (pi, name, event, context) => {
	try {
		await pi[name]({ type: name, ...event }, context);
	} catch (error) {
		console.log(name + ": " + error.message);
	}
};
const nothingSent = (when) => {
	if (existsSync(process.env.KIT_AGENTHOOK_HELPER_OUT)) console.log("sent " + when);
};

// --no-session: Pi never writes a session file, so nothing is reported.
let pi = runtime();
await fire(pi, "session_start", { reason: "startup" }, ctx("memory", undefined));
await fire(pi, "before_agent_start", { prompt: "zero" }, ctx("memory", undefined));
await fire(pi, "context", { messages: [] }, ctx("memory", undefined));
await fire(pi, "agent_settled", {}, ctx("memory", undefined));
await fire(pi, "session_shutdown", { reason: "new" }, ctx("memory", undefined));
nothingSent("without a session file");

// Fresh start: SessionStart and the first prompt wait until Pi appends the user
// message, which happens before the first context event.
pi = runtime();
await fire(pi, "session_start", { reason: "startup" }, ctx("a", fileA));
await fire(pi, "before_agent_start", { prompt: "one" }, ctx("a", fileA));
nothingSent("before the session file existed");
// The header line Pi's SessionManager writes first:
// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/coding-agent/src/core/session-manager.ts#L1063-L1070
writeFileSync(fileA, JSON.stringify({ type: "session", version: 3, id: "a", timestamp: new Date().toISOString(), cwd: "/work" }) + "\n");
await fire(pi, "context", { messages: [] }, ctx("a", fileA));
await fire(pi, "agent_settled", {}, ctx("a", fileA));

// /new: the replaced session ends; the new one is replaced before Pi saves it, so it sends nothing.
await fire(pi, "session_shutdown", { reason: "new" }, ctx("a", fileA));
pi = runtime();
await fire(pi, "session_start", { reason: "new" }, ctx("b", fileB));
await fire(pi, "session_shutdown", { reason: "resume" }, ctx("b", fileB));

// Resume: the session file exists, so SessionStart reports at once; other modes and quit stay silent.
pi = runtime();
await fire(pi, "session_start", { reason: "resume" }, ctx("a", fileA, "json"));
await fire(pi, "session_start", { reason: "resume" }, ctx("a", fileA));
await fire(pi, "session_shutdown", { reason: "quit" }, ctx("a", fileA));

// pi --session: Pi reports startup for a saved session, which resumes it.
pi = runtime();
await fire(pi, "session_start", { reason: "startup" }, ctx("a", fileA));
`

func TestPiExtensionReportsResumableSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "agenthook.js")
	missing := filepath.Join(dir, "missing-hook")
	_, err = Install(AgentPi, InstallOptions{
		ConfigPath: path,
		Executable: missing,
		Arguments:  []string{"--source", "missing-hook"},
		Marker:     "--source missing-hook",
		Hooks:      []Hook{{Event: EventSessionStart}},
	})
	require.NoError(err)
	_, err = Install(AgentPi, InstallOptions{
		ConfigPath: path,
		Executable: os.Args[0],
		Arguments:  []string{"-test.run=^TestScriptHookHelper$", "--", "--source", "shared-agent-hook-test"},
		Marker:     testMarker,
		Hooks: []Hook{
			{Event: EventSessionStart},
			// Shorter than the background process's life, so waiting for stderr to close fails.
			{Event: EventUserPromptSubmit, Timeout: 2 * time.Second},
			{Event: EventSessionEnd},
			{Event: EventStop, Timeout: time.Second},
		},
	})
	require.NoError(err)
	data, err := os.ReadFile(path)
	require.NoError(err)
	module := filepath.Join(dir, "extension.mjs")
	require.NoError(os.WriteFile(module, data, 0o600))
	driver := filepath.Join(dir, "driver.mjs")
	require.NoError(os.WriteFile(driver, []byte(piExtensionDriver), 0o600))
	out := filepath.Join(dir, "payloads.jsonl")
	cmd := exec.CommandContext(t.Context(), node, driver, module, dir)
	cmd.Env = append(os.Environ(), "KIT_AGENTHOOK_HELPER_OUT="+out)
	t.Cleanup(func() {
		pid, err := os.ReadFile(filepath.Join(dir, "linger.pid"))
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		require.NoError(err)
		id, err := strconv.Atoi(string(pid))
		require.NoError(err)
		linger, err := os.FindProcess(id)
		require.NoError(err)
		if err := linger.Kill(); !errors.Is(err, os.ErrProcessDone) {
			assert.NoError(err)
		}
	})

	started := time.Now()
	output, err := cmd.CombinedOutput()

	require.NoError(err, string(output))
	// Node waits for a live child or a referenced pipe, so finishing well before
	// the helpers' sleeps end proves the kill and that a background process
	// holding stderr does not keep Pi waiting; a loaded runner can spend tens of
	// seconds on re-execs.
	assert.Less(
		time.Since(started), 3*time.Minute,
		"a hook process or its stderr kept the extension waiting",
	)
	payloads, err := os.ReadFile(out)
	require.NoError(err)
	type report struct {
		Event      string `json:"hook_event_name"`
		SessionID  string `json:"session_id"`
		Transcript string `json:"transcript_path"`
		Reason     string `json:"reason"`
		Prompt     string `json:"prompt"`
		Cwd        string `json:"cwd"`
		HelperCwd  string `json:"helper_cwd"`
	}
	// macOS reports the working directory through /tmp's symlink target.
	realDir, err := filepath.EvalSymlinks(dir)
	require.NoError(err)
	var reports []report
	for line := range strings.Lines(strings.TrimSpace(string(payloads))) {
		var r report
		require.NoError(json.Unmarshal([]byte(line), &r))
		assert.Equal(dir, r.Cwd)
		helperDir, err := filepath.EvalSymlinks(r.HelperCwd)
		require.NoError(err)
		assert.Equal(realDir, helperDir, "the hook ran outside the session's directory")
		r.Cwd, r.HelperCwd = "", ""
		reports = append(reports, r)
	}
	fileA := filepath.Join(dir, "a.jsonl")
	assert.Equal([]report{
		{Event: "session_start", SessionID: "a", Transcript: fileA, Reason: "startup"},
		{Event: "before_agent_start", SessionID: "a", Transcript: fileA, Prompt: "one"},
		{Event: "agent_settled", SessionID: "a", Transcript: fileA},
		{Event: "session_shutdown", SessionID: "a", Transcript: fileA, Reason: "new"},
		{Event: "session_start", SessionID: "a", Transcript: fileA, Reason: "resume"},
		{Event: "session_start", SessionID: "a", Transcript: fileA, Reason: "resume"},
	}, reports)
	// A failed command is reported once its event's other commands have run.
	failed := "agenthook session_start commands failed: " + missing + " --source missing-hook: could not start"
	failures := strings.Split(strings.TrimSpace(string(output)), "\n")
	require.Len(failures, 4, string(output))
	assert.Contains(failures[0], "context: "+failed)
	assert.Contains(failures[1], "agent_settled: agenthook agent_settled commands failed: ")
	assert.Contains(failures[1], "timed out after 1s: helper still settling")
	assert.Contains(failures[2], "session_start: "+failed)
	assert.Contains(failures[3], "session_start: "+failed)
	// The prompt's command exited at once, though a process it started still holds stderr.
	assert.NotContains(string(output), "before_agent_start commands failed")
	assert.NotContains(string(output), "TestScriptHookHelper$ -- --source shared-agent-hook-test: could not start")
}

const openCodePluginDriver = `
import { pathToFileURL } from "node:url";
const plugin = (await import(pathToFileURL(process.argv[2]).href)).default;
// Session directories differ from the terminal's, which payloads must carry.
const sessions = {
	ses_root: { id: "ses_root", location: { directory: "/elsewhere" } },
	ses_child: { id: "ses_child", parentID: "ses_root", location: { directory: "/elsewhere" } },
	ses_b: { id: "ses_b", location: { directory: "/elsewhere" } },
};
const running = new Set();
const setRunning = (id) => running.add(id);
const memory = {};
let route = { type: "session", sessionID: "ses_child" };
let listener;
let poll;
globalThis.setInterval = (callback) => {
	poll = callback;
	return 0;
};
globalThis.clearInterval = () => {};
const api = {
	ui: {
		router: { current: () => route },
		toast: { show: (toast) => console.log(toast.variant + " toast: " + toast.message) },
	},
	storage: {
		memory: (key, { initial }) => {
			memory[key] ??= structuredClone(initial);
			return [memory[key], (mutation) => mutation(memory[key])];
		},
	},
	data: {
		session: {
			get: (id) => sessions[id],
			root: (id) => sessions[id]?.parentID ?? id,
			status: (id) => (running.has(id) ? "running" : "idle"),
		},
		listen: (handler) => {
			listener = handler;
			return () => { listener = undefined; };
		},
	},
};
const fire = (type, sessionID) => {
	if (type === "session.execution.started") setRunning(sessionID);
	if (type === "session.execution.succeeded") running.delete(sessionID);
	listener?.({ details: { type, data: { sessionID } } });
};
const stop = (sessionID) => fire("session.execution.succeeded", sessionID);
const show = (sessionID) => {
	route = sessionID ? { type: "session", sessionID } : { type: "home" };
	poll();
};
if (process.argv[3] === "child route of running root") {
	setRunning("ses_root");
}
let cleanup = await plugin.setup(api);
const reload = async () => {
	await cleanup();
	cleanup = await plugin.setup(api);
};
const scenarios = {
	"late running snapshot": async () => {
		poll();
		setRunning("ses_root");
		poll();
		poll();
		stop("ses_child");
		stop("ses_other");
		stop("ses_root");
	},
	"idle snapshot stops": async () => {
		fire("session.execution.started", "ses_root");
		running.delete("ses_root");
		poll();
		poll();
	},
	"child route of running root": async () => {
		poll();
		stop("ses_root");
	},
	"navigation to running root": async () => {
		setRunning("ses_b");
		show("ses_b");
		stop("ses_root");
		poll();
		stop("ses_b");
	},
	"home keeps root": async () => {
		fire("session.execution.started", "ses_root");
		show(null);
		stop("ses_root");
	},
	"loading route forwards nothing": async () => {
		show("ses_new");
		stop("ses_root");
		sessions.ses_new = { id: "ses_new", location: { directory: "/elsewhere" } };
		poll();
	},
	"deleted root retires": async () => {
		delete sessions.ses_root;
		fire("session.deleted", "ses_root");
		show(null);
		stop("ses_root");
	},
	"reinstall resends running root": async () => {
		fire("session.execution.started", "ses_root");
		await cleanup();
		const reinstalled = (await import(pathToFileURL(process.argv[4]).href)).default;
		cleanup = await reinstalled.setup(api);
		poll();
		stop("ses_root");
	},
	"turn ends during reload": async () => {
		fire("session.execution.started", "ses_root");
		await cleanup();
		stop("ses_root");
		cleanup = await plugin.setup(api);
		poll();
	},
	"reload on home": async () => {
		show(null);
		await reload();
		show("ses_b");
	},
	"reload on home after deletion": async () => {
		show(null);
		await cleanup();
		delete sessions.ses_root;
		fire("session.deleted", "ses_root");
		cleanup = await plugin.setup(api);
		poll();
	},
};
await scenarios[process.argv[3]]();
await cleanup();
`

func TestOpenCodePluginReportsRootSession(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "plugins", "agenthook", "tui.js")
	// A command that cannot start runs first, so every later report must still
	// reach the working one.
	_, err = Install(AgentOpenCode, InstallOptions{
		ConfigPath: path,
		Executable: filepath.Join(dir, "missing-hook"),
		Arguments:  []string{"--source", "failing-hook"},
		Marker:     "--source failing-hook",
	})
	require.NoError(t, err)
	_, err = Install(AgentOpenCode, InstallOptions{
		ConfigPath: path,
		Executable: os.Args[0],
		Arguments:  []string{"-test.run=^TestScriptHookHelper$", "--", "--source", "shared-agent-hook-test"},
		Marker:     testMarker,
	})
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	module := filepath.Join(dir, "plugin.mjs")
	require.NoError(t, os.WriteFile(module, data, 0o600))
	_, err = Install(AgentOpenCode, InstallOptions{
		ConfigPath: path,
		Executable: os.Args[0],
		Arguments:  []string{"-test.run=^TestScriptHookHelper$", "--", "--source", "shared-agent-hook-test", "--reinstalled"},
		Marker:     testMarker,
	})
	require.NoError(t, err)
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	reinstalledModule := filepath.Join(dir, "reinstalled.mjs")
	require.NoError(t, os.WriteFile(reinstalledModule, data, 0o600))
	driver := filepath.Join(dir, "driver.mjs")
	require.NoError(t, os.WriteFile(driver, []byte(openCodePluginDriver), 0o600))

	const (
		startRoot  = `{"hook_event_name":"SessionStart","session_id":"ses_root"}`
		endRoot    = `{"hook_event_name":"SessionEnd","session_id":"ses_root","reason":"other"}`
		stopRoot   = `{"hook_event_name":"Stop","session_id":"ses_root"}`
		startB     = `{"hook_event_name":"SessionStart","session_id":"ses_b"}`
		promptRoot = `{"hook_event_name":"UserPromptSubmit","session_id":"ses_root"}`
	)
	tests := []struct {
		name string
		want []string
	}{
		{"late running snapshot", []string{
			startRoot,
			promptRoot,
			stopRoot,
		}},
		{"idle snapshot stops", []string{startRoot, promptRoot, stopRoot}},
		{"child route of running root", []string{
			startRoot,
			promptRoot,
			stopRoot,
		}},
		{"navigation to running root", []string{
			startRoot, endRoot, startB,
			`{"hook_event_name":"UserPromptSubmit","session_id":"ses_b"}`,
			`{"hook_event_name":"Stop","session_id":"ses_b"}`,
		}},
		{"home keeps root", []string{startRoot, promptRoot, stopRoot}},
		{"loading route forwards nothing", []string{
			startRoot, endRoot, `{"hook_event_name":"SessionStart","session_id":"ses_new"}`,
		}},
		{"deleted root retires", []string{startRoot, endRoot}},
		{"reinstall resends running root", []string{
			startRoot,
			promptRoot,
			startRoot,
			promptRoot,
			stopRoot,
		}},
		{"turn ends during reload", []string{startRoot, promptRoot, startRoot, stopRoot}},
		{"reload on home", []string{startRoot, startRoot, endRoot, startB}},
		{"reload on home after deletion", []string{startRoot, endRoot}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The terminal's directory differs from every session's. macOS reports
			// the working directory through /tmp's symlink target.
			cwd, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			out := filepath.Join(cwd, "payloads.jsonl")
			cmd := exec.CommandContext(t.Context(), node, driver, module, tt.name, reinstalledModule)
			cmd.Dir = cwd
			// The helper finds its output path only in the terminal's env, so each
			// payload proves the hook ran with that env.
			cmd.Env = append(os.Environ(), "KIT_AGENTHOOK_HELPER_OUT="+out)

			output, err := cmd.CombinedOutput()

			require.NoError(t, err, string(output))
			payloads, err := os.ReadFile(out)
			require.NoError(t, err)
			lines := strings.Split(strings.TrimSpace(string(payloads)), "\n")
			require.Len(t, lines, len(tt.want), string(payloads))
			for i, want := range tt.want {
				var expected, got map[string]any
				require.NoError(t, json.Unmarshal([]byte(want), &expected))
				expected["cwd"] = cwd
				// The runtime spawns each hook in the payload's cwd.
				expected["helper_cwd"] = cwd
				require.NoError(t, json.Unmarshal([]byte(lines[i]), &got))
				assert.Equal(t, expected, got)
			}
			// One error toast per event shows the failing command never blocks the next.
			assert.Equal(t, len(tt.want), strings.Count(string(output), "error toast: kenn.agenthook: agenthook"), string(output))
		})
	}
}
