package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLauncherLoadersCheckOnlyTheirSections(t *testing.T) {
	clearServerEnvironment(t)
	const binding = "[mcp.http]\ncredential_binding='credential:inbound'\n[credential_bindings.inbound]\nenvironment_variable='SCV_INBOUND_TOKEN'\n"
	for _, test := range []struct {
		body                string
		webErr, webDisabled bool
		mcpErr              string
		apiPort             string
		binding             string
		bindingEnvironment  string
	}{
		{body: "[storage]\nbogus=1\n"},
		{body: "[storage]\npack_interval='-1s'\n"},
		{body: "[web\n", webErr: true, mcpErr: "loading"},
		{body: "[web]\nbogus=1\n", webErr: true},
		{body: "[web]\nenabled='yes'\n", webErr: true},
		{body: "[mcp.http]\nbogus=1\n", mcpErr: "unknown key"},
		{body: "[mcp.http]\ncredential_binding=1\n", mcpErr: "loading"},
		{body: "[credential_bindings.inbound]\nbogus=1\n", mcpErr: "unknown key"},
		{body: "[mcp.http]\ncredential_binding='credential:inbound'\n[credential_bindings.inbound]\nenvironment_variable='INVALID-NAME'\n", mcpErr: "[credential_bindings.inbound] environment_variable must be a variable name"},
		{body: "[WEB]\ntypo=true\n", webErr: true},
		{body: "[MCP.http]\ntypo=true\n", mcpErr: "unknown key"},
		{body: "[Credential_Bindings.named]\ntypo=true\n", mcpErr: "unknown key"},
		{body: "[WEB]\nenabled=false\n[MCP.http]\ncredential_binding='credential:named'\n[Credential_Bindings.named]\nenvironment_variable='DOCBANK_MCP_HTTP_TOKEN'\n", webDisabled: true, binding: "credential:named", bindingEnvironment: "DOCBANK_MCP_HTTP_TOKEN"},
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
			if test.mcpErr != "" {
				require.ErrorContains(t, err, test.mcpErr)
			} else {
				require.NoError(t, err)
			}
			if test.binding != "" {
				require.Equal(t, !test.webDisabled, enabled)
				require.Equal(t, test.binding, selected.MCP.HTTP.CredentialBinding)
			}
			if test.bindingEnvironment != "" {
				require.Equal(t, test.bindingEnvironment, selected.CredentialBindings["named"].EnvironmentVariable)
			}
		})
	}
}
