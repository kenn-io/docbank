# MCP registration research

Checked primary sources on 2026-09-26. This note informs the eight clients already
covered by `agenthook`; it does not propose adding other clients.

## Existing installers

- [add-mcp](https://github.com/neon-solutions/add-mcp#readme) is the closest
  reference. Its programmatic API separates agent detection, server upsert,
  listing, and removal. It accepts stdio commands or remote endpoints and maps a
  common server description into client-specific config. It also distinguishes
  project and global scope, HTTP and legacy SSE, and client-specific optional
  fields. Those are useful API boundaries; registry search and interactive
  selection belong in a calling CLI.
- [MCP Tools](https://github.com/f/mcptools/tree/master#llm-apps-config-management)
  has config scanning, multi-client writes, explicit config-file aliases, and
  synchronization with conflict handling. Its README limits config management
  to macOS. Its explicit path override is useful precedent for callers that
  already know the destination.
- [mcp-installer](https://github.com/anaisbetts/mcp-installer#readme) installs npm
  and Python servers for Claude Desktop. It is a package-launch workflow, not a
  source for the eight clients' native configuration formats.
- The current [Smithery CLI](https://github.com/smithery-ai/cli#readme) documents
  registry connections, authentication, tool calls, and publishing. Its current
  README does not establish a native cross-client config-writing contract.

Third-party installers are useful comparisons, but client-owned documentation
and source should decide the emitted format. For example, add-mcp's current
source resolves Copilot through `XDG_CONFIG_HOME`, while GitHub now documents
`COPILOT_HOME` and migration away from previous XDG paths.

## Concrete correctness checks

| Concern | Evidence and implication |
| --- | --- |
| Commented settings | [Gemini's loader](https://github.com/google-gemini/gemini-cli/blob/main/packages/cli/src/config/settings.ts) parses `stripJsonComments(stripUtf8Bom(content))`; [Qwen's loader](https://github.com/QwenLM/qwen-code/blob/main/packages/cli/src/config/settings.ts) parses `stripJsonComments(content)`. A strict JSON reader rejects configurations those clients accept. Registration should accept those native files and retain unrelated settings. |
| Hermes on Windows | [`_get_platform_default_hermes_home`](https://github.com/NousResearch/hermes-agent/blob/main/hermes_constants.py) uses `LOCALAPPDATA`, falling back to `Path.home() / "AppData" / "Local"`, then appends `hermes`. Falling back to `~/.hermes` on Windows chooses the wrong file. |
| Gemini home override | [`homedir()`](https://github.com/google-gemini/gemini-cli/blob/main/packages/core/src/utils/paths.ts) returns `GEMINI_CLI_HOME`; [storage](https://github.com/google-gemini/gemini-cli/blob/main/packages/core/src/config/storage.ts) appends `.gemini`. It is a replacement home, not the final config directory. |
| Qwen home override | [`getGlobalQwenDir()`](https://github.com/QwenLM/qwen-code/blob/main/packages/core/src/config/storage.ts) uses `QWEN_HOME` as the config directory itself. Do not append another `.qwen`. |
| Copilot home override | [GitHub's config reference](https://github.com/github/docs/blob/main/content/copilot/reference/copilot-cli-reference/cli-config-dir-reference.md#changing-the-location-of-the-configuration-directory) says `COPILOT_HOME` replaces the entire `~/.copilot` path. Previous XDG locations are migrated by Copilot itself. |
| Remote transport | [Gemini](https://github.com/google-gemini/gemini-cli/blob/main/docs/tools/mcp-server.md#configuration-structure) distinguishes `httpUrl` for Streamable HTTP and `url` for SSE. [Claude](https://code.claude.com/docs/en/mcp#option-1-add-a-remote-http-server) requires `type: "http"` alongside an HTTP URL. A URL alone is not a portable registration shape. |
| Copilot stdio | [GitHub's MCP reference](https://github.com/github/docs/blob/main/content/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers.md) accepts both `local` and `stdio` and defaults tool selection to `*`. Its user config is `mcp-config.json`, distinct from its JSONC `settings.json`. |

## Scope and preservation

[Claude's scopes](https://code.claude.com/docs/en/mcp#scope-hierarchy-and-precedence)
are user, project, and local. User entries are at the root of `~/.claude.json`;
project entries use `.mcp.json`; local entries are nested under the project path
inside `~/.claude.json`. A config-path override can target project scope, but does
not implement Claude's local scope.

[Copilot](https://github.com/github/docs/blob/main/content/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers.md#adding-per-repository-mcp-servers)
reads `.mcp.json` before `.github/mcp.json` in the same directory and accepts both
`mcpServers` and a bare server map in project files. A writer that always adds
`mcpServers` should not claim to merge every existing Copilot project format.

[Droid](https://docs.factory.ai/cli/configuration/mcp#configuration-file) has user,
ancestor-folder, and project `.factory/mcp.json` files. Client trust and managed
settings can still prevent a successfully written server from loading.

For the requested user-level installer, an explicit path override is sufficient
without automatic scope discovery. Document its expected file schema. Broader
scope resolution needs separate handling of precedence and alternate layouts.

Preserving unrelated values is necessary when writing shared settings files.
Comments must also survive registration, including comments on replaced fields.
Retain the original bytes on a no-op, and reject malformed documents before
writing. Avoid claiming a server is
connected merely because its config was written.

## Selected implementation

The helper uses [Tailscale's HuJSON parser](https://github.com/tailscale/hujson)
to parse and patch JSONC. It retains comments while changing a selected server
entry. It uses standard JSON decoding for semantic validation,
[tomledit](https://github.com/creachadair/tomledit) for TOML syntax edits, and the
existing YAML library's node API for YAML edits. BurntSushi/toml still validates
TOML semantics. Comments from replaced fields remain next to the new server
entry. Quoting and comments on unrelated values are retained, although document
formatting can change. It contains no custom parser.

Explicit SSE uses the mappings below. Project files can be selected by path;
automatic scope discovery and alternate nested layouts remain outside this API.

## Authentication

Literal headers remain supported. `BearerTokenEnvVar` references a variable
without reading its value. For Claude, it emits `headers.Authorization` as
`Bearer ${NAME}`, using [Claude's environment expansion](https://code.claude.com/docs/en/mcp#environment-variable-expansion-in-mcpjson).
For Codex, it emits the native [`bearer_token_env_var` field](https://github.com/openai/codex/blob/main/codex-rs/config/src/mcp_types.rs).
This field supports Claude and Codex remote servers and rejects stdio,
conflicting Authorization headers, and other clients.
The referenced variable must be present in the client's environment.

## Optional additions

- [Claude](https://code.claude.com/docs/en/mcp#use-dynamic-headers-for-custom-authentication)
  also supports dynamic `headersHelper`; [Droid](https://docs.factory.ai/cli/configuration/mcp#add-servers-from-the-cli)
  supports `oauth: false` for header-authenticated servers. These are distinct
  capabilities, not fields that should be copied to every client. Token-file
  snapshots need registration again after rotation.
- Timeouts, environment references, tool filters, OAuth scopes, enablement, and
  approval policies vary by client. [add-mcp's optional-field table](https://github.com/neon-solutions/add-mcp#capability-gated-fields---timeout---scopes---bearer-token-env)
  demonstrates why each requires a native mapping. Keep approvals separate from
  registration; do not add a general unvalidated field passthrough.
- Listing existing client registrations, removal, registry discovery, and
  synchronization are separate operations. None is necessary to discover a
  caller-supplied CLI's published listener and register that listener under a
  caller-owned name.

## Follow-up transport and name checks

The helper's explicit SSE selection should use these native encodings:

| Client | SSE entry | Source |
| --- | --- | --- |
| Claude | `type: "sse"`, `url`, optional `headers` | [Claude MCP reference](https://code.claude.com/docs/en/mcp#option-2-add-a-remote-sse-server) |
| Codex | Reject explicit legacy SSE; its config transport enum has stdio and Streamable HTTP | [Codex config types](https://github.com/openai/codex/blob/main/codex-rs/config/src/mcp_types.rs) |
| Copilot | `type: "sse"`, `url`, optional `headers` | [Copilot MCP reference](https://github.com/github/docs/blob/main/content/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers.md) |
| Cursor | `url`, optional `headers`; the same documented entry represents HTTP and SSE | [Cursor remote-server example](https://cursor.com/docs/mcp.md#configuration) |
| Droid | `type: "sse"`, `url`, optional `headers` | [Droid MCP reference](https://docs.factory.ai/cli/configuration/mcp#configuration-file) |
| Gemini | `url`, optional `headers`; reserve `httpUrl` for Streamable HTTP | [Gemini MCP reference](https://github.com/google-gemini/gemini-cli/blob/main/docs/tools/mcp-server.md#configuration-structure) |
| Hermes | `transport: "sse"`, `url`, optional `headers` | [`_run_http` transport selection](https://github.com/NousResearch/hermes-agent/blob/main/tools/mcp_tool_transport.py) |
| Qwen | `url`, optional `headers`; reserve `httpUrl` for Streamable HTTP | [Qwen MCP reference](https://github.com/QwenLM/qwen-code/blob/main/docs/developers/tools/mcp-server.md#configuration-structure) |

Cursor documents one URL-based shape for both remote transports. This verifies
the config shape, not the exact order in which Cursor attempts transports.

[Claude's CLI](https://code.claude.com/docs/en/mcp#import-mcp-servers-from-claude-desktop)
permits only letters, numbers, hyphens, and underscores in server names.
[Codex's current CLI validator](https://github.com/openai/codex/blob/main/codex-rs/cli/src/mcp_cmd.rs)
accepts nonempty ASCII letters, numbers, and `- _ : @ / .`. These are CLI
restrictions; they do not prove every native file reader imposes the same rule.
No comparable universal name restriction was established for the other six
clients. A shared letters/numbers/hyphens/underscores rule would therefore be
the helper's portable naming contract, rather than every client's exact native
validator.

Claude's custom home path was also checked against Anthropic's published
[2.1.5 npm package](https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-2.1.5.tgz),
reading `package/cli.js` without executing it. Its global config resolver joins
`CLAUDE_CONFIG_DIR || homedir()` with `.claude${suffix}.json`. For the ordinary
empty suffix, `CLAUDE_CONFIG_DIR/.claude.json` is correct. This is versioned
implementation evidence, not a current public stability guarantee.
