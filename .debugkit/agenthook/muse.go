package agenthook

import "path/filepath"

// Muse Code documents hook sources and events but not the payload, matcher,
// or response contract. The mappings below were checked against Muse Code
// 1.4.7, which loads Claude-format hooks:
// https://dev.meta.ai/docs/muse-code/extending#hooks
func museProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentMuse, DisplayName: "Muse Code",
			ConfigEnvironment: "XDG_CONFIG_HOME", ConfigFilename: "settings.json",
			SupportedEvents: []Event{
				EventSessionStart, EventUserPromptSubmit, EventPreToolUse,
				EventPostToolUse, EventPostToolUseFailure, EventPermissionRequest,
				EventNotification, EventStop, EventSessionEnd,
			},
		},
		formatNestedJSON,
		// Muse reports its shell tool as bash. A matcher made only of
		// identifier characters matches a tool name exactly, so bash does not
		// also select bash_input.
		"bash",
		func() (string, error) { return userDotDir(filepath.Join(".config", "muse")) },
	)
	// User hooks live in the hooks block of $XDG_CONFIG_HOME/muse/settings.json,
	// which defaults to ~/.config/muse/settings.json. Muse refuses to start
	// when that file omits schema_version:
	// https://dev.meta.ai/docs/muse-code/configuration#settings-file
	spec.configEnvSubdir = "muse"
	spec.newConfigFields = map[string]any{"schema_version": 1}
	// Muse honors Claude's JSON response contract and sends SessionStart source
	// and SessionEnd reason. It rejects Claude's exec form ("command" plus
	// "args") but accepts commandWindows.
	spec.responseFormat = responseClaude
	spec.sessionSourceRequirement = inputRequired
	spec.sessionEndReasonRequirement = inputRequired
	spec.windowsCommandStyle = windowsCommandNested
	return spec
}
