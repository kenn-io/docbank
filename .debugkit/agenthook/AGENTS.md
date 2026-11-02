# Agent Hook Package Invariants

- The exported event and tool vocabulary follows Claude Code. Agent-specific
  names belong in profiles; callers should not need native event names to
  describe equivalent hooks.
- Handle must dispatch typed Claude Code inputs after normalizing field, event,
  and tool names. Preserve the complete normalized payload in CommonInput.Raw;
  agent-specific structural promotion belongs here rather than in consumers.
- Handler methods return event-specific output types. Keep NoopHandler complete
  so consumers can embed it and override only the lifecycle events they use.
- Handle owns native response encoding after typed dispatch. A profile must
  translate supported control output into its harness's response schema and
  return an error for unsupported control output; never silently downgrade a
  security decision to observational success. Every profile must explicitly
  select a response format; the zero value is intentionally invalid.
- Handle requires a finite input reader that reaches EOF. Its context governs
  handler execution but cannot interrupt reads from an arbitrary `io.Reader`;
  do not introduce a goroutine-backed reader shim that can leak on cancellation.
- Profiles own config discovery, native event and tool translation, file
  format, response encoding, and profile-specific input requirements. A native
  lifecycle hook that lacks a Claude field leaves the typed field at its zero
  value; it must not weaken validation for profiles whose schema requires that
  field. Add support for a new harness here rather than in each consuming
  application.
- Keep an authoritative upstream documentation or implementation URL beside
  non-obvious native matcher and response mappings so profile behavior can be
  checked without guessing from another agent's vocabulary.
- JSON harnesses use either Claude-style nested handlers or native direct
  entries. Keep those encodings separate, and let each profile own event names,
  timeout units, timeout fields, failure policy, and cross-platform command
  fields. Decision-bearing Cursor registrations are fail-closed because Cursor
  otherwise allows the operation when a hook crashes or emits invalid JSON.
- Script profiles (Pi, OpenCode) own one kit-named module. Edit only its delimited
  registration block, refuse a file without that block, rewrite the runtime
  on every write, delete the module when uninstall removes its last hook, and
  spawn registered argv without a shell on every OS. The runtime stops waiting
  for a command shortly after it exits even when a process it started still
  holds stderr, and keeps draining that pipe without holding the harness open.
  The Pi module reports only from Pi's interactive terminal (`ctx.mode` is
  `tui`), so print, json, and rpc runs, subagents included, stay silent. It
  needs Pi 0.80.4 or later (`ctx.mode` 0.78.1, `agent_settled` 0.80.4).
  A script profile must run its hook commands from the process the user's
  terminal started, never from a shared server, so each report carries that
  terminal's environment.
  The OpenCode module retires its reported root with SessionEnd as soon as
  its terminal shows a session with another root or that root is deleted,
  and never from cleanup, which also runs on hot reload and reinstall. Setup
  re-sends SessionStart for a tracked root with a record, even on the home route,
  then reports UserPromptSubmit if it's running, or Stop if a previously running
  turn became idle during reload. A missing record sends SessionEnd.
  The plugin sends UserPromptSubmit, without a prompt, when the shown root starts
  running, including a turn already running when the terminal adopts it.
  Working state follows OpenCode's running state; a transition to idle sends
  Stop, including shutdown interrupts.
  Cwd is the TUI's launch directory. Each report comes from one terminal process.
  Several terminals can report the same session_id, so hosts should keep one
  record per terminal rather than one per session ID.
- Keep each harness profile in its own agent-named file (`claude.go`,
  `codex.go`, and so on). `profile.go` owns only the shared vocabulary,
  registry, and lookup behavior.
- A marker is a caller-provided, application-namespaced unique substring that
  identifies application-owned commands. Install and uninstall may replace or
  remove only matching commands and must preserve unrelated hooks and top-level
  config.
- Claude exec-form hooks store the executable in `command` and its arguments
  in `args`. Install validation and removal must both match the marker against
  `command` and `args` joined by single spaces; changing one join without the
  other leaves installed hooks that uninstall cannot find.
- A profile whose harness refuses to start without certain top-level config
  fields declares them in `newConfigFields`; install seeds them only when it
  creates the config or finds it empty. Muse Code needs `schema_version`, and
  it rejects Claude's exec form, so its Windows commands use `commandWindows`.
- Do not silently enable an agent's hook auto-approval setting. Installation
  writes registrations; the harness remains responsible for user consent.
- JSON and YAML writes must preserve symlinked config paths and existing file
  mode bits. `writeConfig` resolves a symlinked path only to create the
  target's directory, then writes through the original path with
  `atomicfile.WriteFile(..., WithFollowLink())` so a link retargeted before
  commit is refused rather than writing the stale destination. Pass
  `WithFollowLink` only when the first `Lstat` saw a symlink; a config that
  was a regular file or absent keeps atomicfile's default, which refuses a
  link swapped in before the write. Callers must serialize mutations that
  target the same config path.
- When a config write returns an error wrapping `atomicfile.ErrPublished`,
  `Install` and `Uninstall` return the populated `Result` with that error so
  callers know the planned mutation became visible.
