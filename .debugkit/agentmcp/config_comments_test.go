package agentmcp_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tailscale/hujson"

	"go.kenn.io/kit/agentmcp"
)

func TestDocumentCommentsSurviveRegistration(t *testing.T) {
	for _, tt := range []struct {
		name     string
		agent    agentmcp.Agent
		original string
		comments []string
	}{
		{"toml sections", agentmcp.AgentCodex, `# user preferences
model = 'example-model' # model note

# service description
[mcp_servers.example] # section note
command = "old" # command note
args = [
  "serve", # argument note
  # final argument note
]
[mcp_servers.example.env] # env note
MODE = "test" # variable note

# another service
[mcp_servers.other]
command = 'other' # other note
`, []string{"# user preferences", "# model note", "# service description", "# section note", "# command note", "# argument note", "# final argument note", "# env note", "# variable note", "# another service", "# other note"}},
		{"toml inline", agentmcp.AgentCodex, `# user preferences
model = 'example-model'
mcp_servers = { example = { command = "old", args = [
  "serve", # nested argument note
] }, other = { command = 'other' } } # inline note
`, []string{"# user preferences", "# nested argument note", "# inline note"}},
		{"toml dotted", agentmcp.AgentCodex, `# user preferences
model = 'example-model'
mcp_servers.example.command = "old" # dotted command note
mcp_servers.example.env.MODE = "test" # dotted env note
mcp_servers.other.command = 'other' # other note
`, []string{"# user preferences", "# dotted command note", "# dotted env note", "# other note"}},
		{"toml parent", agentmcp.AgentCodex, `# user preferences
model = 'example-model'
[mcp_servers] # parent note
example = { command = "old", args = [
  "serve", # nested argument note
] } # inline note
other = { command = 'other' }
`, []string{"# user preferences", "# parent note", "# nested argument note", "# inline note"}},
		{"yaml", agentmcp.AgentHermes, `# user preferences
model: 'example-model' # model note
defaults: &defaults
  message: |-
    this # is literal text
copy: *defaults # alias note
mcp_servers:
  # service description
  example:
    command: old # command note
    args:
      - serve # argument note
    env:
      MODE: test # variable note
  # another service
  other:
    command: 'other' # other note
# final note
`, []string{"# user preferences", "# model note", "# alias note", "# service description", "# command note", "# argument note", "# variable note", "# another service", "# other note", "# final note"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			require.NoError(t, os.WriteFile(path, []byte(tt.original), 0o600))
			before := decodeConfig(t, tt.agent, []byte(tt.original))
			opts := agentmcp.InstallOptions{ConfigPath: path, Name: "example", Server: agentmcp.Server{URL: "https://example.com/mcp", Headers: map[string]string{"Authorization": "Bearer example"}}}
			result, err := agentmcp.Install(tt.agent, opts)
			require.NoError(t, err)
			for _, comment := range tt.comments {
				assert.Contains(t, string(result.Data), comment)
			}
			after := decodeConfig(t, tt.agent, result.Data)
			assert.Equal(t, before["model"], after["model"])
			assert.Contains(t, string(result.Data), "'example-model'")
			servers, ok := after["mcp_servers"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, map[string]any{"command": "other"}, servers["other"])
			headerKey := "headers"
			if tt.agent == agentmcp.AgentCodex {
				headerKey = "http_headers"
			}
			assert.Equal(t, map[string]any{"url": "https://example.com/mcp", headerKey: map[string]any{"Authorization": "Bearer example"}}, servers["example"])
			if tt.agent == agentmcp.AgentHermes {
				assert.Equal(t, before["defaults"], after["defaults"])
				assert.Equal(t, before["copy"], after["copy"])
				assert.Contains(t, string(result.Data), "&defaults")
				assert.Contains(t, string(result.Data), "*defaults")
				assert.Contains(t, string(result.Data), "|-")
			}
			second, err := agentmcp.Install(tt.agent, opts)
			require.NoError(t, err)
			assert.False(t, second.Changed)
			assert.Equal(t, result.Data, second.Data)
		})
	}
}

func TestJSONCReplacementKeepsFieldComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := []byte(`{
  "mcpServers": {
    // server note
    "example": {
      "command": "old", // command note
      "args": [
        "serve" // argument note
      ],
      "env": { /* env note */ "MODE": "test" }
      // final field note
    }
  }
}`)
	require.NoError(t, os.WriteFile(path, original, 0o600))
	opts := agentmcp.InstallOptions{ConfigPath: path, Name: "example", Server: agentmcp.Server{URL: "https://example.com/mcp"}}
	result, err := agentmcp.Install(agentmcp.AgentGemini, opts)
	require.NoError(t, err)
	for _, comment := range []string{"// server note", "// command note", "// argument note", "/* env note */", "// final field note"} {
		assert.Contains(t, string(result.Data), comment)
		assert.Equal(t, 1, bytes.Count(result.Data, []byte(comment)))
	}
	parsed, err := hujson.Parse(bytes.Clone(result.Data))
	require.NoError(t, err)
	parsed.Standardize()
	doc := decodeConfig(t, agentmcp.AgentGemini, parsed.Pack())
	assert.Equal(t, map[string]any{"mcpServers": map[string]any{"example": map[string]any{"httpUrl": "https://example.com/mcp"}}}, doc)
	second, err := agentmcp.Install(agentmcp.AgentGemini, opts)
	require.NoError(t, err)
	assert.False(t, second.Changed)
	assert.Equal(t, result.Data, second.Data)
}

func TestCommentsOnlyDocument(t *testing.T) {
	for _, agent := range []agentmcp.Agent{agentmcp.AgentCodex, agentmcp.AgentHermes} {
		t.Run(string(agent), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			require.NoError(t, os.WriteFile(path, []byte("# keep this configuration note"), 0o600))
			result, err := agentmcp.Install(agent, agentmcp.InstallOptions{ConfigPath: path, Name: "example", Server: agentmcp.Server{Command: "example"}})
			require.NoError(t, err)
			assert.Contains(t, string(result.Data), "# keep this configuration note")
			assert.Equal(t, map[string]any{"mcp_servers": map[string]any{"example": map[string]any{"command": "example"}}}, decodeConfig(t, agent, result.Data))
		})
	}
}

func TestYAMLInheritedServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte(`defaults: &defaults
  # inherited service note
  mcp_servers:
    other:
      command: other
<<: *defaults
`)
	require.NoError(t, os.WriteFile(path, original, 0o600))
	result, err := agentmcp.Install(agentmcp.AgentHermes, agentmcp.InstallOptions{ConfigPath: path, Name: "example", Server: agentmcp.Server{Command: "example"}})
	require.NoError(t, err)
	before := decodeConfig(t, agentmcp.AgentHermes, original)
	after := decodeConfig(t, agentmcp.AgentHermes, result.Data)
	assert.Equal(t, before["defaults"], after["defaults"])
	assert.Equal(t, map[string]any{"example": map[string]any{"command": "example"}, "other": map[string]any{"command": "other"}}, after["mcp_servers"])
	assert.Contains(t, string(result.Data), "# inherited service note")
}
