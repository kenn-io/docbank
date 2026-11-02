package agenthook

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed opencode_plugin.js
var openCodePlugin string

func openCodeProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentOpenCode, DisplayName: "OpenCode",
			ConfigEnvironment: "OPENCODE_CONFIG_DIR",
			// The 2.x TUI loads the tui entrypoint of each <config>/plugins/<dir>/,
			// in the terminal's own process; the shared service skips a directory
			// with no server or index entry, and 1.x reads only plugins/*.js:
			// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/tui/src/plugin/discovery.ts#L24-L58
			// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/tui/src/plugin/context.tsx#L670
			// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/plugin/src/host.ts#L43
			ConfigFilename: filepath.Join("plugins", "agenthook", "tui.js"),
			SupportedEvents: []Event{
				EventSessionStart, EventUserPromptSubmit, EventStop, EventSessionEnd,
			},
		},
		formatScript,
		"",
		openCodeDefaultDir,
	)
	spec.script = openCodePlugin
	// The plugin ignores command output, so control decisions have nowhere to go.
	spec.responseFormat = responseObservational
	// The plugin reports a root session when the terminal shows it, which has
	// no Claude source.
	spec.sessionSourceRequirement = inputOptional
	// The plugin reports a running turn from execution status, which carries no prompt.
	spec.userPromptRequirement = inputOptional
	// The plugin sends SessionEnd only when its terminal moves to another root or
	// the root is deleted, always with reason other.
	spec.sessionEndReasonRequirement = inputRequired
	return spec
}

// openCodeDefaultDir follows OpenCode's XDG rule on every OS:
// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/util/src/global-roots.ts#L7
func openCodeDefaultDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "opencode"), nil
	}
	return userDotDir(filepath.Join(".config", "opencode"))
}
