# Agent CLI command construction

`agentcli` builds argument vectors for coding-agent CLIs. It supplies defaults,
start and resume forms, prompt delivery, and portable controls through `Request`.
It does not execute commands, inspect installed versions, resolve session files,
or manage terminals and persistent state.

`Command.Options` passes through unchanged and in order. The package does not
parse these arguments, restrict them to known flags, or reject overlap with
`Request` fields. Callers own explicitly supplied arguments; the CLI interprets
them. Configured arguments precede request-generated arguments, after the fixed
subcommand for Kiro and noninteractive Droid and Muse Code. Droid's REPL and
`exec` read some flags differently, such as `-r`, so Droid options must suit
both commands when a caller uses one `Command` for both modes. Muse Code
rejects its root options before `exec`, so the same applies to Muse options.

Use `Request.Prompt` for kit-managed prompt delivery; its zero value means no
prompt. The adapter chooses the CLI's normal argument or stdin transport. Use
`Resume` for a saved session. A zero `Command` keeps the default executable and
adds no configured arguments.

## Consumer examples

Forge can preserve a configured interactive command and resume without sending
the original prompt again:

```go
agent, err := agentcli.NewCodex(agentcli.Command{
	Executable: "codex",
	Options:    []string{"--profile", "forge"},
})
if err != nil {
	return err
}
invocation, err := agent.Resume(sessionID, agentcli.Request{})
// invocation.Argv: codex --profile forge resume <sessionID>
```

RoboRev can select an adapter by name and request a noninteractive event stream:

```go
prompt := agentcli.Prompt{Text: reviewPrompt}
agent, err := agentcli.New(agentcli.Codex, agentcli.Command{Executable: configuredExecutable})
if err != nil {
	return err
}
invocation, err := agent.Start(agentcli.Request{
	Mode:         agentcli.NonInteractive,
	Prompt:       prompt,
	OutputFormat: agentcli.OutputJSONL,
	Sandbox:      agentcli.SandboxReadOnly,
	Approval:     agentcli.ApprovalNever,
})
// Pass invocation.Argv[0], invocation.Argv[1:], and *invocation.Stdin to os/exec.
```

Call `Capabilities` before presenting options in a UI. `Start` and `Resume`
still validate every request and return `UnsupportedOptionError` when an option
cannot keep its requested meaning. Explicit `Command.Options` are independent
of those typed request checks.

## Supported agents

`Names` returns thirteen concrete CLI adapters. The modes below describe this
package, not every mode offered by the underlying command. Interactive mode
opens the agent's own terminal UI and takes no prompt for Gemini, GitHub
Copilot, OpenCode, Cursor Agent, Factory Droid, Hermes, and Qwen Code.
Interactive Droid accepts only autonomy and the built-in skills control;
its other controls are `exec` flags. This package passes no prompt or model
to interactive OpenCode, though OpenCode itself accepts `--prompt`.
Interactive Muse Code takes a prompt only when starting a session, because
`muse resume` accepts none.

| Agent | Modes | Prompt | Resume | Output | Reasoning |
| --- | --- | --- | --- | --- | --- |
| Codex | interactive, noninteractive | argument when interactive, stdin when noninteractive | `resume ID`, `exec resume ID` | text, JSONL | low, medium, high, xhigh, maximum |
| Claude Code | interactive, noninteractive | argument when interactive, stdin when noninteractive | `--resume ID` | text, JSON, JSONL | low, medium, high, xhigh, maximum |
| Gemini | interactive, noninteractive | stdin through `--prompt`, noninteractive only | `--resume ID` | text, JSON, JSONL | none |
| GitHub Copilot | interactive, noninteractive | `--prompt`, noninteractive only | `--resume=ID` | text, JSONL | low, medium, high, xhigh, maximum |
| OpenCode | interactive, noninteractive | stdin, noninteractive only | `--session ID`, `run --session ID` | text, JSONL | none |
| Cursor Agent | interactive, noninteractive | stdin, noninteractive only | `--resume ID` | text, JSON, JSONL | none |
| Kiro | noninteractive | argument | `chat --resume-id ID` | text | low, medium, high, xhigh, maximum |
| Kilo | noninteractive | stdin | `run --session ID` | text, JSONL | low, medium, high, xhigh, maximum |
| Factory Droid | interactive, noninteractive | stdin, noninteractive only | `--resume ID`, `exec --session-id ID` | text, JSON, JSONL | low, medium, high, xhigh, maximum, noninteractive only |
| Pi | interactive, noninteractive | argument and `@file` | `--session ID` | text, JSONL | low, medium, high, xhigh, maximum |
| Hermes | interactive | none | `--resume ID` | text | none |
| Qwen Code | interactive | none | `--resume ID` | text | none |
| Muse Code | interactive, noninteractive | argument, start only when interactive | `resume ID`, `exec --session-id ID` | text, JSONL | low, medium, high, xhigh, maximum |

`ReasoningXHigh` and `ReasoningMaximum` are distinct. Adapters with a native
`max` value, including Codex, map only `ReasoningMaximum` to it. Droid accepts
model-dependent reasoning values, and Kilo passes the value as a
provider-specific model variant, so the selected model remains the final
authority for those two commands.

The remaining controls are intentionally uneven:

| Agent | JSON Schema | Execution controls | Customization controls |
| --- | --- | --- | --- |
| Codex | schema file and output path | sandbox and approval modes | disable skill instructions, hooks, user config, or session storage; config overrides |
| Claude Code | inline schema | approval modes; allow, deny, or disable built-in tools | disable skills, all customizations including hooks, or session storage |
| Gemini | none | plan or bypass approval mode | none |
| GitHub Copilot | none | allow and deny tools; full permission bypass | disable built-in MCP servers or context instructions |
| OpenCode | none | none | none |
| Cursor Agent | none | plan or bypass mode | none |
| Kiro | none | trusted-tool allowlist or trust all tools | none |
| Kilo | none | automatic approval | none |
| Factory Droid | none | tool allowlist and denylist; low, medium, or high autonomy; permission bypass | disable built-in skills |
| Pi | inline schema through an explicit extension and output file | allow, deny, or disable built-in tools | skill paths; disable skills, extensions, prompt templates, themes, context files, hooks through extension discovery, or session storage |
| Hermes | none | none | none |
| Qwen Code | none | none | none |
| Muse Code | schema file, noninteractive only | approval modes; full-access sandbox; `--yolo` bypass | disable session storage; config overrides |

The adapters reflect these CLI contracts:

- [Codex noninteractive mode](https://learn.chatgpt.com/docs/non-interactive-mode)
- [Codex configuration reference](https://developers.openai.com/codex/config-reference)
- [Claude Code CLI reference](https://code.claude.com/docs/en/cli-reference)
- [Gemini CLI reference](https://geminicli.com/docs/cli/commands/)
- [GitHub Copilot CLI reference](https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-command-reference)
- [OpenCode CLI reference](https://opencode.ai/docs/cli/)
- [Cursor Agent CLI reference](https://docs.cursor.com/en/cli/reference/parameters)
- [Kiro CLI command reference](https://kiro.dev/docs/reference/cli-commands/)
- [Kilo CLI source](https://github.com/Kilo-Org/kilocode)
- [Factory Droid CLI reference](https://docs.factory.ai/droid-cli/cli-reference)
- [Pi README](https://github.com/earendil-works/pi/tree/main/packages/coding-agent)
- [Hermes CLI source](https://github.com/NousResearch/hermes-agent)
- [Qwen Code source](https://github.com/QwenLM/qwen-code)
- [Muse Code headless mode](https://dev.meta.ai/docs/muse-code/extending#headless-and-ci)
- [Muse Code launch flags](https://dev.meta.ai/docs/muse-code/configuration#launch-flags)

The first consumer migrations should replace Forge's temporary command-option
validator together with its interactive resume switch; Forge should pass its
configured executable and option slice directly to the matching constructor.
RoboRev should replace the argument builders for its ten command-based agents
while keeping stream parsing, installed-version capability probes, environment
filtering, Pi session-file lookup, and process lifecycle code. Its Agent Client
Protocol adapter remains outside this argv package because it owns a protocol
session and process, not a one-shot command shape. Configured arguments remain
caller-owned in every consumer.
