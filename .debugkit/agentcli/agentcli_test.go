package agentcli_test

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentcli"
)

func mustAgent(t *testing.T, name agentcli.Name, command agentcli.Command) agentcli.Adapter {
	t.Helper()
	agent, err := agentcli.New(name, command)
	require.NoError(t, err)
	return agent
}

func TestSupportedAgents(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	want := []agentcli.Name{agentcli.Codex, agentcli.Claude, agentcli.Gemini, agentcli.Copilot, agentcli.OpenCode, agentcli.Cursor, agentcli.Kiro, agentcli.Kilo, agentcli.Droid, agentcli.Pi, agentcli.Hermes, agentcli.Qwen, agentcli.Muse}
	assert.Equal(want, agentcli.Names())
	for _, name := range want {
		assert.Equal(name, mustAgent(t, name, agentcli.Command{}).Name())
	}
	names := agentcli.Names()
	names[0] = "changed"
	assert.Equal(agentcli.Codex, agentcli.Names()[0])
	_, err := agentcli.New("unknown", agentcli.Command{})
	require.Error(t, err)
}

func TestInvocationContracts(t *testing.T) {
	t.Parallel()
	prompt := "review this change"
	tests := []struct {
		name    agentcli.Name
		session string
		request agentcli.Request
		want    []string
	}{
		{agentcli.Codex, "thread-id", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Model: "gpt-test", Reasoning: agentcli.ReasoningXHigh, OutputFormat: agentcli.OutputJSONL, Sandbox: agentcli.SandboxReadOnly, Approval: agentcli.ApprovalNever, DisableSkills: true, DisableHooks: true, DisableUserConfig: true, DisableSessionStorage: true, ConfigOverrides: []string{"feature.test=true"}}, []string{"codex", "exec", "resume", "-c", "feature.test=true", "--ignore-user-config", "-c", "skills.include_instructions=false", "--disable", "hooks", "--ephemeral", "--model", "gpt-test", "-c", `model_reasoning_effort="xhigh"`, "-c", `sandbox_mode="read-only"`, "-c", `approval_policy="never"`, "--json", "thread-id", "-"}},
		{agentcli.Claude, "", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Model: "sonnet", Reasoning: agentcli.ReasoningHigh, OutputFormat: agentcli.OutputJSONL, Schema: agentcli.JSONSchema{Inline: `{"type":"object"}`}, Approval: agentcli.ApprovalNever, AllowedTools: []string{"Read", "Glob"}, DeniedTools: []string{"Bash"}, DisableSkills: true}, []string{"claude", "--print", "--verbose", "--output-format", "stream-json", "--json-schema", `{"type":"object"}`, "--model", "sonnet", "--effort", "high", "--disable-slash-commands", "--permission-mode", "dontAsk", "--allowedTools", "Read,Glob", "--disallowedTools", "Bash"}},
		{agentcli.Gemini, "session-1", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Model: "gemini-test", OutputFormat: agentcli.OutputJSONL, Approval: agentcli.ApprovalNever}, []string{"gemini", "--output-format", "stream-json", "--resume", "session-1", "--model", "gemini-test", "--approval-mode", "plan", "--prompt", ""}},
		{agentcli.Copilot, "session-1", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Model: "copilot-test", Reasoning: agentcli.ReasoningXHigh, OutputFormat: agentcli.OutputJSONL, Approval: agentcli.ApprovalBypass, DeniedTools: []string{"write"}, DisableBuiltInMCPs: true, DisableContextFiles: true}, []string{"copilot", "--silent", "--allow-all-tools", "--stream", "off", "--output-format", "json", "--resume=session-1", "--model", "copilot-test", "--reasoning-effort", "xhigh", "--allow-all", "--deny-tool", "write", "--disable-builtin-mcps", "--no-custom-instructions", "--prompt", prompt}},
		{agentcli.OpenCode, "session-1", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Model: "provider/model", OutputFormat: agentcli.OutputJSONL}, []string{"opencode", "run", "--format", "json", "--session", "session-1", "--model", "provider/model"}},
		{agentcli.Cursor, "session-1", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Model: "cursor-test", OutputFormat: agentcli.OutputJSONL, Approval: agentcli.ApprovalNever}, []string{"agent", "--print", "--output-format", "stream-json", "--resume", "session-1", "--model", "cursor-test", "--mode", "plan"}},
		{agentcli.Kiro, "session-1", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Reasoning: agentcli.ReasoningXHigh, Approval: agentcli.ApprovalBypass}, []string{"kiro-cli", "chat", "--no-interactive", "--resume-id", "session-1", "--effort", "xhigh", "--trust-all-tools", "--", prompt}},
		{agentcli.Kilo, "session-1", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Model: "provider/model", Reasoning: agentcli.ReasoningXHigh, OutputFormat: agentcli.OutputJSONL, Approval: agentcli.ApprovalBypass}, []string{"kilo", "run", "--format", "json", "--session", "session-1", "--model", "provider/model", "--auto", "--variant", "xhigh"}},
		{agentcli.Droid, "session-1", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Model: "droid-test", Reasoning: agentcli.ReasoningXHigh, OutputFormat: agentcli.OutputJSONL, Autonomy: agentcli.AutonomyMedium, DeniedTools: []string{"execute-cli"}, DisableSkills: true}, []string{"droid", "exec", "--session-id", "session-1", "--model", "droid-test", "--reasoning-effort", "xhigh", "--auto", "medium", "--disabled-tools", "execute-cli", "--disable-builtin-skills", "--output-format", "stream-json"}},
		{agentcli.Muse, "session-1", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: "--classify"}, Provider: "meta", Model: "muse-test", Reasoning: agentcli.ReasoningMaximum, OutputFormat: agentcli.OutputJSONL, Schema: agentcli.JSONSchema{Path: "schema.json"}, Sandbox: agentcli.SandboxDangerFullAccess, Approval: agentcli.ApprovalNever, DisableSessionStorage: true, ConfigOverrides: []string{"session_retention_days=3"}}, []string{"muse", "exec", "--session-id", "session-1", "-c", "session_retention_days=3", "--provider", "meta", "--model", "muse-test", "--reasoning-effort", "max", "--approval-mode", "never", "--disable-sandbox", "--no-session-log", "--json", "--output-schema", "schema.json", "--", "--classify"}},
		{agentcli.Pi, "", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: "--classify", Files: []string{"prompt.md"}}, Provider: "test-provider", Model: "test-model", Reasoning: agentcli.ReasoningMaximum, Schema: agentcli.JSONSchema{Inline: `{"type":"object"}`, Extension: "schema-extension", OutputPath: "result.json"}, DisableBuiltInTools: true, DisableSkills: true, DisableHooks: true, DisablePromptTemplates: true, DisableThemes: true, DisableContextFiles: true, DisableSessionStorage: true}, []string{"pi", "--no-session", "--no-extensions", "--no-builtin-tools", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--extension", "schema-extension", "--json-schema", `{"type":"object"}`, "--json-output", "result.json", "--json-fallback", "none", "--print", "--provider", "test-provider", "--model", "test-model", "--thinking", "max", "--", "@prompt.md", "--classify"}},
	}
	for _, test := range tests {
		t.Run(string(test.name), func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			agent := mustAgent(t, test.name, agentcli.Command{})
			var got agentcli.Invocation
			var err error
			if test.session == "" {
				got, err = agent.Start(test.request)
			} else {
				got, err = agent.Resume(test.session, test.request)
			}
			require.NoError(err)
			assert.Equal(test.want, got.Argv)
			if test.name == agentcli.Codex || test.name == agentcli.Claude || test.name == agentcli.Gemini || test.name == agentcli.OpenCode || test.name == agentcli.Cursor || test.name == agentcli.Kilo || test.name == agentcli.Droid {
				require.NotNil(got.Stdin)
				assert.Equal(test.request.Prompt.Text, *got.Stdin)
			} else {
				assert.Nil(got.Stdin)
			}
		})
	}
}

func TestInteractiveResumePreservesConfiguredOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    agentcli.Name
		command agentcli.Command
		request agentcli.Request
		want    []string
	}{
		{agentcli.Codex, agentcli.Command{Executable: "codex-custom", Options: []string{"--full-auto", "--profile", "team"}}, agentcli.Request{}, []string{"codex-custom", "--full-auto", "--profile", "team", "resume", "session-1"}},
		{agentcli.Claude, agentcli.Command{Executable: "claude-custom", Options: []string{"--setting-sources", "project"}}, agentcli.Request{}, []string{"claude-custom", "--setting-sources", "project", "--resume", "session-1"}},
		{agentcli.Pi, agentcli.Command{Executable: "pi-custom", Options: []string{"--offline"}}, agentcli.Request{}, []string{"pi-custom", "--offline", "--session", "session-1"}},
		{agentcli.Copilot, agentcli.Command{Executable: "copilot-custom", Options: []string{"--add-dir", "shared"}}, agentcli.Request{}, []string{"copilot-custom", "--add-dir", "shared", "--resume=session-1"}},
		{agentcli.Cursor, agentcli.Command{Executable: "cursor-agent", Options: []string{"--workspace", "repo"}}, agentcli.Request{}, []string{"cursor-agent", "--workspace", "repo", "--resume", "session-1"}},
		{agentcli.Droid, agentcli.Command{Executable: "droid-custom", Options: []string{"--append-system-prompt", "Run tests."}}, agentcli.Request{Autonomy: agentcli.AutonomyLow, DisableSkills: true}, []string{"droid-custom", "--append-system-prompt", "Run tests.", "--resume", "session-1", "--auto", "low", "--disable-builtin-skills"}},
		{agentcli.Gemini, agentcli.Command{Executable: "gemini-custom", Options: []string{"--sandbox"}}, agentcli.Request{}, []string{"gemini-custom", "--sandbox", "--resume", "session-1"}},
		{agentcli.Qwen, agentcli.Command{Executable: "qwen-custom", Options: []string{"--sandbox"}}, agentcli.Request{}, []string{"qwen-custom", "--sandbox", "--resume", "session-1"}},
		{agentcli.Hermes, agentcli.Command{Executable: "hermes-custom", Options: []string{"--tui"}}, agentcli.Request{}, []string{"hermes-custom", "--tui", "--resume", "session-1"}},
		{agentcli.OpenCode, agentcli.Command{Executable: "opencode-custom", Options: []string{"--auto"}}, agentcli.Request{}, []string{"opencode-custom", "--auto", "--session", "session-1"}},
		{agentcli.Muse, agentcli.Command{Executable: "muse-custom", Options: []string{"--trust-workspace"}}, agentcli.Request{Approval: agentcli.ApprovalBypass}, []string{"muse-custom", "--trust-workspace", "--yolo", "resume", "session-1"}},
	}
	for _, test := range tests {
		got, err := mustAgent(t, test.name, test.command).Resume("session-1", test.request)
		require.NoError(t, err)
		assert.Equal(t, test.want, got.Argv)
		assert.Nil(t, got.Stdin)
	}
}

func TestUnsupportedRequestsReturnTypedErrors(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	tests := []struct {
		name    agentcli.Name
		request agentcli.Request
		option  string
	}{
		{agentcli.Codex, agentcli.Request{Mode: agentcli.NonInteractive, OutputFormat: agentcli.OutputJSON}, "output format"},
		{agentcli.Claude, agentcli.Request{Sandbox: agentcli.SandboxReadOnly}, "sandbox"},
		{agentcli.Pi, agentcli.Request{Approval: agentcli.ApprovalNever}, "approval mode"},
		{agentcli.Copilot, agentcli.Request{OutputFormat: agentcli.OutputJSONL}, "output format"},
		{agentcli.Gemini, agentcli.Request{Prompt: agentcli.Prompt{Text: "review"}}, "prompt"},
		{agentcli.Droid, agentcli.Request{Reasoning: agentcli.ReasoningHigh}, "reasoning"},
		{agentcli.Droid, agentcli.Request{Model: "droid-test"}, "model"},
		{agentcli.Droid, agentcli.Request{Approval: agentcli.ApprovalBypass}, "approval mode"},
		{agentcli.Droid, agentcli.Request{AllowedTools: []string{"Read"}}, "tools"},
		{agentcli.Droid, agentcli.Request{DeniedTools: []string{"Execute"}}, "tools"},
		{agentcli.Cursor, agentcli.Request{Prompt: agentcli.Prompt{Text: "review"}}, "prompt"},
		{agentcli.Copilot, agentcli.Request{Prompt: agentcli.Prompt{Text: "review"}}, "prompt"},
		{agentcli.Qwen, agentcli.Request{Prompt: agentcli.Prompt{Text: "review"}}, "prompt"},
		{agentcli.Hermes, agentcli.Request{Prompt: agentcli.Prompt{Text: "review"}}, "prompt"},
		{agentcli.Hermes, agentcli.Request{Mode: agentcli.NonInteractive}, "mode"},
		{agentcli.Qwen, agentcli.Request{Mode: agentcli.NonInteractive}, "mode"},
		{agentcli.OpenCode, agentcli.Request{Prompt: agentcli.Prompt{Text: "review"}}, "prompt"},
		{agentcli.OpenCode, agentcli.Request{Model: "provider/model"}, "model"},
		{agentcli.Muse, agentcli.Request{Schema: agentcli.JSONSchema{Path: "schema.json"}}, "JSON schema path"},
		{agentcli.Muse, agentcli.Request{Sandbox: agentcli.SandboxReadOnly}, "sandbox"},
	}
	for _, test := range tests {
		_, err := mustAgent(t, test.name, agentcli.Command{}).Start(test.request)
		var unsupported *agentcli.UnsupportedOptionError
		require.ErrorAs(err, &unsupported)
		assert.Equal(test.name, unsupported.Agent)
		assert.Equal(test.option, unsupported.Option)
		assert.NotEmpty(unsupported.Hint)
	}
	_, err := mustAgent(t, agentcli.Codex, agentcli.Command{}).Resume("--last", agentcli.Request{})
	require.Error(err)
}

func TestMuseInteractivePrompt(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	agent := mustAgent(t, agentcli.Muse, agentcli.Command{})
	request := agentcli.Request{Prompt: agentcli.Prompt{Text: "-fix the build"}}

	got, err := agent.Start(request)
	require.NoError(err)
	assert.Equal([]string{"muse", "--", "-fix the build"}, got.Argv)

	_, err = agent.Resume("session-1", request)
	var unsupported *agentcli.UnsupportedOptionError
	require.ErrorAs(err, &unsupported)
	assert.Equal("prompt", unsupported.Option)
}

func TestCapabilitiesAreExplicitAndIndependent(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	codex := mustAgent(t, agentcli.Codex, agentcli.Command{})
	got := codex.Capabilities()
	assert.True(got.Resume)
	assert.True(got.JSONSchemaPath)
	assert.False(got.JSONSchemaInline)
	assert.Equal(agentcli.DisableHooksOnly, got.DisableHooks)
	assert.Equal([]agentcli.ReasoningLevel{agentcli.ReasoningLow, agentcli.ReasoningMedium, agentcli.ReasoningHigh, agentcli.ReasoningXHigh, agentcli.ReasoningMaximum}, got.ReasoningLevels)
	got.Modes[0], got.ReasoningLevels[0] = "changed", "changed"
	assert.Equal(agentcli.Interactive, codex.Capabilities().Modes[0])
	assert.Equal(agentcli.ReasoningLow, codex.Capabilities().ReasoningLevels[0])
}

func TestConfiguredArgumentsPassThrough(t *testing.T) {
	t.Parallel()
	for _, name := range agentcli.Names() {
		t.Run(string(name), func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			options := []string{"--future-flag", "value with spaces", "--model", "configured", "--", "", "operand"}
			agent := mustAgent(t, name, agentcli.Command{Executable: "custom-worker", Options: options})
			capabilities := agent.Capabilities()
			request := agentcli.Request{Mode: agentcli.Interactive}
			if slices.Contains(capabilities.Modes, agentcli.NonInteractive) {
				request.Mode = agentcli.NonInteractive
			}
			if capabilities.Model {
				request.Model = "requested"
			}
			for _, resume := range []bool{false, true} {
				var got agentcli.Invocation
				var err error
				if resume {
					got, err = agent.Resume("session-1", request)
				} else {
					got, err = agent.Start(request)
				}
				require.NoError(err)
				assert.Equal("custom-worker", got.Argv[0])
				start := 1
				if name == agentcli.Kiro || name == agentcli.Droid || name == agentcli.Muse {
					start++ // These CLIs put configured arguments after their subcommand.
				}
				assert.Equal(options, got.Argv[start:start+len(options)])
				if request.Model != "" {
					assert.Contains(got.Argv[start+len(options):], "requested")
				}
			}
		})
	}
}

func TestXHighAndMaximumStayDistinct(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	tests := []struct {
		name           agentcli.Name
		xhigh, maximum string
	}{
		{agentcli.Codex, `model_reasoning_effort="xhigh"`, `model_reasoning_effort="max"`},
		{agentcli.Claude, "xhigh", "max"},
		{agentcli.Pi, "xhigh", "max"},
		{agentcli.Copilot, "xhigh", "max"},
		{agentcli.Kilo, "xhigh", "max"},
		{agentcli.Kiro, "xhigh", "max"},
		{agentcli.Droid, "xhigh", "max"},
	}
	for _, test := range tests {
		agent := mustAgent(t, test.name, agentcli.Command{})
		xhigh, err := agent.Start(agentcli.Request{Mode: agentcli.NonInteractive, Reasoning: agentcli.ReasoningXHigh})
		require.NoError(err)
		maximum, err := agent.Start(agentcli.Request{Mode: agentcli.NonInteractive, Reasoning: agentcli.ReasoningMaximum})
		require.NoError(err)
		assert.Contains(xhigh.Argv, test.xhigh)
		assert.Contains(maximum.Argv, test.maximum)
	}
}

func TestClaudeCanDisableAllBuiltInTools(t *testing.T) {
	got, err := mustAgent(t, agentcli.Claude, agentcli.Command{}).Start(agentcli.Request{Mode: agentcli.NonInteractive, DisableBuiltInTools: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"claude", "--print", "--tools", ""}, got.Argv)
}
