package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadMCPHTTPRejectsMalformedSelectedBinding(t *testing.T) {
	clearServerEnvironment(t)
	root := privateTestConfigDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(`[mcp.http]
credential_binding='credential:inbound'
[credential_bindings.inbound]
environment_variable='INVALID-NAME'
`), 0600))
	_, err := LoadMCPHTTP(root)
	require.ErrorContains(t, err, "[credential_bindings.inbound] environment_variable must be a variable name")
}

func TestLauncherLoadersCheckOnlyTheirSections(t *testing.T) {
	clearServerEnvironment(t)
	const binding = "[mcp.http]\ncredential_binding='credential:inbound'\n[credential_bindings.inbound]\nenvironment_variable='SCV_INBOUND_TOKEN'\n"
	for _, test := range []struct {
		body           string
		webErr, mcpErr bool
		loadErr        bool
		apiPort        string
		binding        string
	}{
		{body: "[storage]\nbogus=1\n", loadErr: true},
		{body: "[storage]\npack_interval='-1s'\n", loadErr: true},
		{body: "[server]\nidle_timeout='invalid'\n[web\n", webErr: true, mcpErr: true},
		{body: "[web]\nbogus=1\n", webErr: true},
		{body: "[web]\nenabled='yes'\n", webErr: true},
		{body: "[mcp.http]\nbogus=1\n", mcpErr: true},
		{body: "[mcp.http]\ncredential_binding=1\n", mcpErr: true},
		{body: "[credential_bindings.inbound]\nbogus=1\n", mcpErr: true},
		{body: "[web]\nsession_lifetime='1s'\n" + binding, binding: "credential:inbound"},
		{body: "[[watch]]\nname='relative'\nsource='relative/path'\n" + binding, binding: "credential:inbound"},
		{body: binding, apiPort: "invalid", binding: "credential:inbound"},
	} {
		t.Run(test.body, func(t *testing.T) {
			if test.apiPort != "" {
				t.Setenv("DOCBANK_API_PORT", test.apiPort)
			}
			root := privateTestConfigDir(t)
			require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(test.body), 0600))
			enabled, err := WebEnabled(root)
			require.Equal(t, test.webErr, err != nil, "WebEnabled: %v", err)
			selected, err := LoadMCPHTTP(root)
			require.Equal(t, test.mcpErr, err != nil, "LoadMCPHTTP: %v", err)
			if test.binding != "" {
				require.True(t, enabled)
				require.Equal(t, test.binding, selected.MCP.HTTP.CredentialBinding)
			}
			if test.loadErr {
				_, err = Load(root)
				require.ErrorContains(t, err, "loading")
			}
		})
	}
}
