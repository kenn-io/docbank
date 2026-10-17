package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScopedLoaders(t *testing.T) {
	clearServerEnvironment(t)
	for _, body := range []string{
		"[web]\nsession_lifetime='1s'\n",
		"[storage]\npack_max_bytes=-1\n",
		"[server]\nbind_addr='0.0.0.0'\n",
		"[[watch]]\nname='relative'\nsource='relative/path'\n",
		"[embedding_profiles.bad]\ndimensions=-1\n",
		"[credential_bindings.unused]\nenvironment_variable='not a variable'\n",
	} {
		t.Run(body, func(t *testing.T) {
			root := privateTestConfigDir(t)
			body += "[mcp.http]\ncredential_binding='credential:inbound'\n[credential_bindings.inbound]\nenvironment_variable='SCV_INBOUND_TOKEN'\n"
			require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(body), 0600))
			selected, err := LoadMCPHTTP(root)
			require.NoError(t, err)
			require.Equal(t, "credential:inbound", selected.MCP.HTTP.CredentialBinding)
			cfg, err := Load(root)
			if err == nil {
				err = cfg.Validate()
			}
			require.Error(t, err)
		})
	}
}

func TestScopedLoadersIgnoreDaemonEnvironment(t *testing.T) {
	clearServerEnvironment(t)
	t.Setenv("DOCBANK_API_PORT", "invalid")
	t.Setenv("DOCBANK_API_KEY", "synthetic-key")
	t.Setenv("DOCBANK_API_KEY_FILE", "missing-secret-file")
	root := privateTestConfigDir(t)
	_, err := LoadWeb(root)
	require.NoError(t, err)
	_, err = LoadMCPHTTP(root)
	require.NoError(t, err)
	_, err = Load(root)
	require.Error(t, err)
}

func TestScopedLoadersValidateOwnedSections(t *testing.T) {
	clearServerEnvironment(t)
	for _, test := range []struct {
		body, section string
		load          func(string) (Config, error)
	}{
		{"[web]\nsession_lifetime='1s'\n", "[web]", LoadWeb},
		{"[mcp.http]\ncredential_binding='credential:missing'\n", "[mcp.http]", LoadMCPHTTP},
		{"[mcp.http]\ncredential_binding='credential:inbound'\n[credential_bindings.inbound]\nenvironment_variable='invalid name'\n", "[credential_bindings.inbound]", LoadMCPHTTP},
	} {
		root := privateTestConfigDir(t)
		require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(test.body), 0600))
		_, err := test.load(root)
		require.ErrorContains(t, err, test.section)
	}
}

func TestWebLauncherDoesNotRequireDaemonEnvironmentKey(t *testing.T) {
	clearServerEnvironment(t)
	root := privateTestConfigDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[web]\npublic_origin='https://archive.example'\n"), 0600))
	_, err := LoadWeb(root)
	require.NoError(t, err)
	cfg, err := Load(root)
	require.NoError(t, err)
	require.ErrorContains(t, cfg.Validate(), "server.api_key")
}

func TestClientConfigWarningsResolveValidPathsAndDefaults(t *testing.T) {
	clearServerEnvironment(t)
	root := privateTestConfigDir(t)
	body := "[[watch]]\nname='inbox'\nsource='~/synthetic-inbox'\ndestination='/inbox'\n[store_bindings.archive]\nkind='filesystem'\npath='relative/archive'\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(body), 0600))
	var warnings strings.Builder
	require.NoError(t, CheckClient(root, &warnings))
	require.Empty(t, warnings.String())
}

func TestWebLoaderIgnoresUnrelatedPolicyWithoutDefaultFallback(t *testing.T) {
	clearServerEnvironment(t)
	root := privateTestConfigDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[web]\nenabled=false\n[storage]\npack_max_bytes=-1\n[mcp.http]\ncredential_binding='invalid'\n"), 0600))
	c, err := LoadWeb(root)
	require.NoError(t, err)
	require.False(t, c.Web.Enabled)
	require.EqualValues(t, -1, c.Storage.PackMaxBytes)
}

func TestEveryLoaderRejectsBrokenTOML(t *testing.T) {
	for _, body := range []string{"[web\n", "[web]\nunknown_setting=true\n"} {
		root := privateTestConfigDir(t)
		require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(body), 0600))
		for _, load := range []func(string) (Config, error){Parse, Load, LoadWeb, LoadMCPHTTP} {
			_, err := load(root)
			require.ErrorContains(t, err, "loading")
		}
		var warnings strings.Builder
		require.ErrorContains(t, CheckClient(root, &warnings), "loading")
		require.Empty(t, warnings.String())
	}
}

func TestNegativeDurationsAreSectionPolicy(t *testing.T) {
	clearServerEnvironment(t)
	for _, body := range []string{
		"[web]\nsession_lifetime='-1s'\n",
		"[storage]\npack_interval='-1s'\n",
		"[server]\nidle_timeout='-1s'\n",
	} {
		root := privateTestConfigDir(t)
		require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(body), 0600))
		_, err := LoadMCPHTTP(root)
		require.NoError(t, err)
		c, err := Load(root)
		require.NoError(t, err)
		require.Error(t, c.Validate())
	}
}

func TestDaemonEnvironmentErrorsNameServerSection(t *testing.T) {
	clearServerEnvironment(t)
	t.Setenv("DOCBANK_API_PORT", "invalid")
	_, err := Load(privateTestConfigDir(t))
	require.ErrorContains(t, err, "[server]")
	t.Setenv("DOCBANK_API_PORT", "")
	t.Setenv("DOCBANK_API_KEY", "synthetic-key")
	t.Setenv("DOCBANK_API_KEY_FILE", "missing")
	_, err = Load(privateTestConfigDir(t))
	require.ErrorContains(t, err, "[server]")
}

func TestClientConfigWarningExplainsStartupEnvironment(t *testing.T) {
	clearServerEnvironment(t)
	t.Setenv("DOCBANK_API_KEY", strings.Repeat("x", 32))
	root := privateTestConfigDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[web]\npublic_origin='https://archive.example'\n"), 0600))
	c, err := Load(root)
	require.NoError(t, err)
	require.NoError(t, c.Validate())
	var warnings strings.Builder
	require.NoError(t, CheckClient(root, &warnings))
	require.Contains(t, warnings.String(), "file-only")
	require.Contains(t, warnings.String(), "environment overrides")
	require.NotContains(t, warnings.String(), "unused")
}
