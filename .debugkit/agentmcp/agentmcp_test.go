package agentmcp_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tailscale/hujson"
	"gopkg.in/yaml.v3"

	"go.kenn.io/kit/agentmcp"
)

func TestMain(m *testing.M) {
	if response := os.Getenv("KIT_MCP_TEST_RESPONSE"); response != "" {
		if len(os.Args) != 4 || os.Args[1] != "mcp" || os.Args[2] != "status" || os.Args[3] != "--json" {
			os.Exit(2)
		}
		fmt.Print(response)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestMarshalConfig(t *testing.T) {
	stdio := agentmcp.Server{Command: `C:\Program Files\example.exe`, Args: []string{"mcp", "--vault", `C:\Data Files\vault`}, Env: map[string]string{"EXAMPLE_HOME": `C:\Data Files\vault`}}
	headers := map[string]string{"X-Client": "example"}
	http := agentmcp.Server{URL: "https://example.com/mcp", Headers: headers, BearerTokenEnvVar: "EXAMPLE_TOKEN"}
	for _, tt := range []struct {
		name, key, expected string
		agent               agentmcp.Agent
		server              agentmcp.Server
	}{
		{"codex stdio", "mcp_servers", `{"command":"C:\\Program Files\\example.exe","args":["mcp","--vault","C:\\Data Files\\vault"],"env":{"EXAMPLE_HOME":"C:\\Data Files\\vault"}}`, agentmcp.AgentCodex, stdio},
		{"claude http", "mcpServers", `{"type":"http","url":"https://example.com/mcp","headers":{"X-Client":"example","Authorization":"Bearer ${EXAMPLE_TOKEN}"}}`, agentmcp.AgentClaude, http},
		{"codex http", "mcp_servers", `{"url":"https://example.com/mcp","http_headers":{"X-Client":"example"},"bearer_token_env_var":"EXAMPLE_TOKEN"}`, agentmcp.AgentCodex, http},
		{"claude bearer only", "mcpServers", `{"type":"http","url":"https://example.com/mcp","headers":{"Authorization":"Bearer ${EXAMPLE_TOKEN}"}}`, agentmcp.AgentClaude, agentmcp.Server{URL: "https://example.com/mcp", BearerTokenEnvVar: "EXAMPLE_TOKEN"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			t.Setenv("CODEX_HOME", dir)
			t.Setenv("EXAMPLE_TOKEN", "test-token-value")
			path, err := agentmcp.ConfigPath(tt.agent)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, []byte("invalid client config"), 0o600))
			data, err := agentmcp.MarshalConfig(tt.agent, "example.with.dot", tt.server)
			require.NoError(t, err)
			doc := decodeConfig(t, tt.agent, data)
			servers, ok := doc[tt.key].(map[string]any)
			require.True(t, ok)
			entry, err := json.Marshal(servers["example.with.dot"])
			require.NoError(t, err)
			assert.JSONEq(t, tt.expected, string(entry))
			assert.Len(t, doc, 1)
			original, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, "invalid client config", string(original))
		})
	}
	assert.Equal(t, map[string]string{"X-Client": "example"}, headers)
}

func TestMarshalConfigDeterministicJSON(t *testing.T) {
	server := agentmcp.Server{URL: "https://example.com/mcp", Headers: map[string]string{"X-Client": "example", "Authorization": "Bearer example"}}
	expected, err := agentmcp.MarshalConfig(agentmcp.AgentClaude, "example", server)
	require.NoError(t, err)
	for range 20 {
		data, err := agentmcp.MarshalConfig(agentmcp.AgentClaude, "example", server)
		require.NoError(t, err)
		require.Equal(t, string(expected), string(data))
	}
}

func TestMarshalConfigInvalid(t *testing.T) {
	for _, tt := range []struct {
		label, name string
		agent       agentmcp.Agent
		server      agentmcp.Server
	}{
		{"unknown agent", "example", "unknown", agentmcp.Server{Command: "example"}},
		{"empty name", " ", agentmcp.AgentClaude, agentmcp.Server{Command: "example"}},
		{"stdio bearer", "example", agentmcp.AgentClaude, agentmcp.Server{Command: "example", BearerTokenEnvVar: "TOKEN"}},
		{"invalid bearer name", "example", agentmcp.AgentClaude, agentmcp.Server{URL: "https://example.com/mcp", BearerTokenEnvVar: "TOKEN:-literal-secret"}},
		{"unsupported bearer", "example", agentmcp.AgentCursor, agentmcp.Server{URL: "https://example.com/mcp", BearerTokenEnvVar: "TOKEN"}},
		{"conflicting bearer", "example", agentmcp.AgentClaude, agentmcp.Server{URL: "https://example.com/mcp", BearerTokenEnvVar: "TOKEN", Headers: map[string]string{"authorization": "Bearer literal"}}},
		{"invalid transport", "example", agentmcp.AgentCodex, agentmcp.Server{Transport: "unknown", URL: "https://example.com/mcp"}},
	} {
		t.Run(tt.label, func(t *testing.T) {
			data, err := agentmcp.MarshalConfig(tt.agent, tt.name, tt.server)
			require.Error(t, err)
			assert.Empty(t, data)
		})
	}
}

func TestInstall(t *testing.T) {
	for _, tt := range []struct {
		agent   agentmcp.Agent
		initial string
		key     string
		http    string
		stdio   string
	}{
		{agentmcp.AgentClaude, `{"keep":9007199254740993,"mcpServers":{"other":{"command":"other"}}}`, "mcpServers", `{"type":"http","url":"http://localhost:1234/mcp","headers":{"Authorization":"Bearer test"}}`, `{"type":"stdio","command":"example","args":["mcp","serve"],"env":{"EXAMPLE_MODE":"local"}}`},
		{agentmcp.AgentCodex, "keep = 9007199254740993\n[mcp_servers.other]\ncommand = 'other'\n", "mcp_servers", `{"url":"http://localhost:1234/mcp","http_headers":{"Authorization":"Bearer test"}}`, `{"command":"example","args":["mcp","serve"],"env":{"EXAMPLE_MODE":"local"}}`},
		{agentmcp.AgentCopilot, `{"keep":9007199254740993,"mcpServers":{"other":{"command":"other"}}}`, "mcpServers", `{"type":"http","url":"http://localhost:1234/mcp","headers":{"Authorization":"Bearer test"},"tools":["*"]}`, `{"type":"local","command":"example","args":["mcp","serve"],"env":{"EXAMPLE_MODE":"local"},"tools":["*"]}`},
		{agentmcp.AgentCursor, `{"keep":9007199254740993,"mcpServers":{"other":{"command":"other"}}}`, "mcpServers", `{"url":"http://localhost:1234/mcp","headers":{"Authorization":"Bearer test"}}`, `{"command":"example","args":["mcp","serve"],"env":{"EXAMPLE_MODE":"local"}}`},
		{agentmcp.AgentDroid, `{"keep":9007199254740993,"mcpServers":{"other":{"command":"other"}}}`, "mcpServers", `{"type":"http","url":"http://localhost:1234/mcp","headers":{"Authorization":"Bearer test"}}`, `{"type":"stdio","command":"example","args":["mcp","serve"],"env":{"EXAMPLE_MODE":"local"}}`},
		{agentmcp.AgentGemini, `{"keep":9007199254740993,"mcpServers":{"other":{"command":"other"}}}`, "mcpServers", `{"httpUrl":"http://localhost:1234/mcp","headers":{"Authorization":"Bearer test"}}`, `{"command":"example","args":["mcp","serve"],"env":{"EXAMPLE_MODE":"local"}}`},
		{agentmcp.AgentHermes, "keep: 9007199254740993\nmcp_servers:\n  other:\n    command: other\n", "mcp_servers", `{"url":"http://localhost:1234/mcp","headers":{"Authorization":"Bearer test"}}`, `{"command":"example","args":["mcp","serve"],"env":{"EXAMPLE_MODE":"local"}}`},
		{agentmcp.AgentQwen, `{"keep":9007199254740993,"mcpServers":{"other":{"command":"other"}}}`, "mcpServers", `{"httpUrl":"http://localhost:1234/mcp","headers":{"Authorization":"Bearer test"}}`, `{"command":"example","args":["mcp","serve"],"env":{"EXAMPLE_MODE":"local"}}`},
	} {
		t.Run(string(tt.agent), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			require.NoError(t, os.WriteFile(path, []byte(tt.initial), 0o640))
			opts := agentmcp.InstallOptions{ConfigPath: path, Name: "example.with.dot", Server: agentmcp.Server{URL: "http://localhost:1234/mcp", Headers: map[string]string{"Authorization": "Bearer test"}}}
			plan, err := agentmcp.PlanInstall(tt.agent, opts)
			require.NoError(t, err)
			assert.True(t, plan.Changed)
			original, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tt.initial, string(original))
			for _, expected := range []string{tt.http, tt.stdio} {
				result, err := agentmcp.Install(tt.agent, opts)
				require.NoError(t, err)
				assert.True(t, result.Changed)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, result.Data, data)
				doc := decodeConfig(t, tt.agent, data)
				servers, ok := doc[tt.key].(map[string]any)
				require.True(t, ok)
				entry, err := json.Marshal(servers[opts.Name])
				require.NoError(t, err)
				assert.JSONEq(t, expected, string(entry))
				assert.Equal(t, map[string]any{"command": "other"}, servers["other"])
				assert.Contains(t, string(data), "9007199254740993")
				second, err := agentmcp.Install(tt.agent, opts)
				require.NoError(t, err)
				assert.False(t, second.Changed)
				assert.Equal(t, data, second.Data)
				opts.Server = agentmcp.Server{Command: "example", Args: []string{"mcp", "serve"}, Env: map[string]string{"EXAMPLE_MODE": "local"}}
			}
			if runtime.GOOS != "windows" {
				info, err := os.Stat(path)
				require.NoError(t, err)
				assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
			}
		})
	}
}

func decodeConfig(t *testing.T, agent agentmcp.Agent, data []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	switch agent {
	case agentmcp.AgentCodex:
		_, err := toml.Decode(string(data), &doc)
		require.NoError(t, err)
	case agentmcp.AgentHermes:
		require.NoError(t, yaml.Unmarshal(data, &doc))
	default:
		require.NoError(t, json.Unmarshal(data, &doc))
	}
	return doc
}

func TestConfigPath(t *testing.T) {
	for _, tt := range []struct {
		agent              agentmcp.Agent
		variable, filename string
	}{
		{agentmcp.AgentClaude, "CLAUDE_CONFIG_DIR", ".claude.json"},
		{agentmcp.AgentCodex, "CODEX_HOME", "config.toml"},
		{agentmcp.AgentCopilot, "COPILOT_HOME", "mcp-config.json"},
		{agentmcp.AgentGemini, "GEMINI_CLI_HOME", filepath.Join(".gemini", "settings.json")},
		{agentmcp.AgentHermes, "HERMES_HOME", "config.yaml"},
		{agentmcp.AgentQwen, "QWEN_HOME", "settings.json"},
	} {
		t.Run(string(tt.agent), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(tt.variable, dir)
			path, err := agentmcp.ConfigPath(tt.agent)
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(dir, tt.filename), path)
		})
	}
}

func TestDiscover(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-token\n"), 0o600))
	want := []agentmcp.Listener{{PID: 123, Transport: "http", URL: "http://127.0.0.1:1234/mcp", BackendURL: "unix:///example.sock", TokenPath: tokenPath}, {Transport: "http", URL: "http://127.0.0.1:5678/mcp"}}
	data, err := json.Marshal(want)
	require.NoError(t, err)
	t.Setenv("KIT_MCP_TEST_RESPONSE", string(data))
	listeners, err := agentmcp.Discover(t.Context(), executable)
	require.NoError(t, err)
	require.Equal(t, want, listeners)
	server, err := listeners[0].Server()
	require.NoError(t, err)
	assert.Equal(t, agentmcp.Server{URL: want[0].URL, Headers: map[string]string{"Authorization": "Bearer test-token"}}, server)
	for _, response := range []string{"null", "{}", "usage: mcp", `[{"transport":"stdio"}]`, `[{"transport":"http","url":"file:///tmp/a"}]`} {
		t.Setenv("KIT_MCP_TEST_RESPONSE", response)
		_, err := agentmcp.Discover(t.Context(), executable)
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = agentmcp.Discover(ctx, executable)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestInvalidConfigUnchanged(t *testing.T) {
	for _, tt := range []struct {
		agent agentmcp.Agent
		data  string
	}{
		{agentmcp.AgentClaude, "null"},
		{agentmcp.AgentClaude, `{"mcpServers":[]}`},
		{agentmcp.AgentClaude, `{"mcpServers":null}`},
		{agentmcp.AgentCodex, "mcp_servers = 12"},
		{agentmcp.AgentHermes, "mcp_servers: []"},
	} {
		t.Run(string(tt.agent)+tt.data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			require.NoError(t, os.WriteFile(path, []byte(tt.data), 0o600))
			_, err := agentmcp.Install(tt.agent, agentmcp.InstallOptions{ConfigPath: path, Name: "example", Server: agentmcp.Server{Command: "example"}})
			require.Error(t, err)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tt.data, string(data))
		})
	}
}

func TestInstallSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "config")
	require.NoError(t, os.WriteFile(target, []byte("{}"), 0o600))
	require.NoError(t, os.Symlink(target, link))
	result, err := agentmcp.Install(agentmcp.AgentClaude, agentmcp.InstallOptions{ConfigPath: link, Name: "example", Server: agentmcp.Server{Command: "example"}})
	require.NoError(t, err)
	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, result.Data, data)
}

func TestCommentedConfig(t *testing.T) {
	for _, agent := range []agentmcp.Agent{agentmcp.AgentGemini, agentmcp.AgentQwen, agentmcp.AgentCursor} {
		t.Run(string(agent), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			original := []byte(`{
  // Keep the user's model choice.
  "model": "example",
  "endpoint": "https://example.com/path//not-a-comment",
  "mcpServers": {
    /* Keep this server's notes. */
    "other": {"command": "other"}
  }
}`)
			if agent == agentmcp.AgentGemini {
				original = append([]byte{0xef, 0xbb, 0xbf}, original...)
			}
			require.NoError(t, os.WriteFile(path, original, 0o600))
			opts := agentmcp.InstallOptions{ConfigPath: path, Name: "example/~service", Server: agentmcp.Server{Command: "example"}}
			result, err := agentmcp.Install(agent, opts)
			require.NoError(t, err)
			assert.Contains(t, string(result.Data), "// Keep the user's model choice.")
			assert.Contains(t, string(result.Data), "/* Keep this server's notes. */")
			data := result.Data
			if agent == agentmcp.AgentGemini {
				require.Equal(t, []byte{0xef, 0xbb, 0xbf}, data[:3])
				data = data[3:]
			}
			value, err := hujson.Parse(data)
			require.NoError(t, err)
			value = value.Clone()
			value.Standardize()
			doc := decodeConfig(t, agent, value.Pack())
			assert.Equal(t, "https://example.com/path//not-a-comment", doc["endpoint"])
			servers, ok := doc["mcpServers"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, map[string]any{"command": "example"}, servers[opts.Name])
			assert.Equal(t, map[string]any{"command": "other"}, servers["other"])
			second, err := agentmcp.Install(agent, opts)
			require.NoError(t, err)
			assert.False(t, second.Changed)
			assert.Equal(t, result.Data, second.Data)
		})
	}
}

func TestSSE(t *testing.T) {
	for _, tt := range []struct {
		agent    agentmcp.Agent
		expected string
	}{
		{agentmcp.AgentClaude, `{"type":"sse","url":"https://example.com/events"}`},
		{agentmcp.AgentCopilot, `{"type":"sse","url":"https://example.com/events","tools":["*"]}`},
		{agentmcp.AgentCursor, `{"url":"https://example.com/events"}`},
		{agentmcp.AgentDroid, `{"type":"sse","url":"https://example.com/events"}`},
		{agentmcp.AgentGemini, `{"url":"https://example.com/events"}`},
		{agentmcp.AgentHermes, `{"transport":"sse","url":"https://example.com/events"}`},
		{agentmcp.AgentQwen, `{"url":"https://example.com/events"}`},
	} {
		t.Run(string(tt.agent), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nested", "config")
			result, err := agentmcp.Install(tt.agent, agentmcp.InstallOptions{ConfigPath: path, Name: "example", Server: agentmcp.Server{Transport: agentmcp.TransportSSE, URL: "https://example.com/events"}})
			require.NoError(t, err)
			doc := decodeConfig(t, tt.agent, result.Data)
			key := "mcpServers"
			if tt.agent == agentmcp.AgentHermes {
				key = "mcp_servers"
			}
			servers, ok := doc[key].(map[string]any)
			require.True(t, ok)
			entry, err := json.Marshal(servers["example"])
			require.NoError(t, err)
			assert.JSONEq(t, tt.expected, string(entry))
		})
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	_, err := agentmcp.Install(agentmcp.AgentCodex, agentmcp.InstallOptions{ConfigPath: path, Name: "example", Server: agentmcp.Server{Transport: agentmcp.TransportSSE, URL: "https://example.com/events"}})
	require.Error(t, err)
	assert.NoFileExists(t, path)
}
