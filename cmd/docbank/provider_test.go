package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/BurntSushi/toml"
	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/docbank/provider"
	"go.kenn.io/docbank/internal/providers"
)

const providerTestHelperArg = "docbank-provider-test-helper"

var providerTestSwappedInstallation = uuid.MustParse("66666666-6666-4666-8666-666666666666")

var providerTestVault = provider.Vault{
	ID:        uuid.MustParse("22222222-2222-4222-8222-222222222222"),
	Name:      "Studio documents",
	VaultUUID: uuid.MustParse("33333333-3333-4333-8333-333333333333"),
}

// runProviderTestHelper answers one discovery and records the request it got.
func runProviderTestHelper(mode, record string) int {
	request, err := provider.DecodeRequest(os.Stdin)
	if err != nil {
		return 2
	}
	data, _ := json.Marshal(request)
	if os.WriteFile(record, data, 0o600) != nil {
		return 1
	}
	response := provider.Response{Version: provider.Version, Operation: request.Operation, RequestID: request.RequestID}
	if mode == "swap" && swapProviderInstallation() != nil {
		return 1
	}
	switch mode {
	case "ready", "swap":
		response.Status = provider.StatusReady
		response.Discovery = &provider.Discovery{Enabled: true, CanCreate: true, Vaults: []provider.Vault{providerTestVault}}
	default:
		response.Status = provider.StatusSignInRequired
	}
	if provider.WriteResponse(os.Stdout, request, response) != nil {
		return 1
	}
	return 0
}

// swapProviderInstallation saves a different installation ID under the
// configuration lock, which setup must not be holding while the helper runs.
func swapProviderInstallation() error {
	store, err := providers.DefaultStore()
	if err != nil {
		return err
	}
	lock := flock.New(store.Path() + ".lock")
	if locked, err := lock.TryLock(); err != nil || !locked {
		return errors.New("setup holds the configuration lock")
	}
	defer func() { _ = lock.Unlock() }()
	var data bytes.Buffer
	if toml.NewEncoder(&data).Encode(providers.File{InstallationID: providerTestSwappedInstallation}) != nil {
		return errors.New("encoding the configuration")
	}
	return os.WriteFile(store.Path(), data.Bytes(), 0o600)
}

func providerDocument(t *testing.T, name, mode, record string) string {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	data, err := json.Marshal(map[string]any{
		"version": 1, "name": name,
		"origin": "https://vaults.example.test", "mount_prefix": "/documents",
		"browser_origin": "https://vaults.example.test",
		"helper":         []string{executable, providerTestHelperArg, mode, record},
	})
	require.NoError(t, err)
	return string(data)
}

func configureProvider(t *testing.T, document string) (string, error) {
	t.Helper()
	rootCmd.SetIn(strings.NewReader(document))
	t.Cleanup(func() { rootCmd.SetIn(nil) })
	return runCLI(t, "provider", "configure")
}

func recordedRequest(t *testing.T, record string) provider.Request {
	t.Helper()
	data, err := os.ReadFile(record)
	require.NoError(t, err)
	var request provider.Request
	require.NoError(t, json.Unmarshal(data, &request))
	return request
}

func TestProviderConfigureSavesOnlyAfterTheHelperAnswers(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv(providers.ConfigHomeEnv, configHome)
	record := filepath.Join(t.TempDir(), "request.json")

	out, err := configureProvider(t, providerDocument(t, "work", "sign-in", record))
	require.ErrorContains(t, err, "needs you to sign in", out)
	require.NoFileExists(t, filepath.Join(configHome, "providers.toml"))

	out, err = configureProvider(t, providerDocument(t, "work", "ready", record))
	require.NoError(t, err, out)
	require.Equal(t, "Connected provider work: 1 vault(s)\n", out)
	first := recordedRequest(t, record)
	require.Equal(t, "https://vaults.example.test", first.Origin)
	require.Equal(t, "/documents", first.MountPrefix)

	// Reconfiguring keeps the installation ID and the other providers.
	out, err = configureProvider(t, providerDocument(t, "home", "ready", record))
	require.NoError(t, err, out)
	require.Equal(t, first.InstallationID, recordedRequest(t, record).InstallationID)
	out, err = runCLI(t, "provider", "list")
	require.NoError(t, err, out)
	require.Equal(t, "home\thttps://vaults.example.test/documents\nwork\thttps://vaults.example.test/documents\n", out)

	// A failed check leaves the saved connection in place.
	saved, err := os.ReadFile(filepath.Join(configHome, "providers.toml"))
	require.NoError(t, err)
	_, err = configureProvider(t, providerDocument(t, "work", "sign-in", record))
	require.Error(t, err)
	unchanged, err := os.ReadFile(filepath.Join(configHome, "providers.toml"))
	require.NoError(t, err)
	require.Equal(t, string(saved), string(unchanged))

	out, err = runCLI(t, "provider", "vaults", "work", "--json")
	require.NoError(t, err, out)
	var discovery provider.Discovery
	require.NoError(t, json.Unmarshal([]byte(out), &discovery))
	require.Equal(t, provider.Discovery{Enabled: true, CanCreate: true, Vaults: []provider.Vault{providerTestVault}}, discovery)
	require.Equal(t, first.InstallationID, recordedRequest(t, record).InstallationID)
}

func TestProviderConfigureRejectsUnsafeConnections(t *testing.T) {
	t.Setenv(providers.ConfigHomeEnv, t.TempDir())
	record := filepath.Join(t.TempDir(), "request.json")
	for name, edit := range map[string]func(string) string{
		"plain HTTP origin": func(document string) string {
			return strings.Replace(document, `"origin":"https://`, `"origin":"http://`, 1)
		},
		"helper on PATH": func(document string) string {
			executable, _ := os.Executable()
			quoted, _ := json.Marshal(executable)
			return strings.Replace(document, string(quoted), `"example-helper"`, 1)
		},
		"unknown field": func(document string) string {
			return strings.Replace(document, `{`, `{"token":"x",`, 1)
		},
		"origin without host": func(document string) string {
			return strings.Replace(document, `"origin":"https://vaults.example.test"`, `"origin":"https://:443"`, 1)
		},
		"oversized document": func(document string) string {
			return document + strings.Repeat(" ", provider.MaxRequestBytes)
		},
	} {
		_, err := configureProvider(t, edit(providerDocument(t, "work", "ready", record)))
		require.Error(t, err, name)
		require.NoFileExists(t, record, name)
	}
	_, err := configureProvider(t, providerDocument(t, "Work Laptop", "ready", record))
	require.ErrorContains(t, err, "provider name")
}

func TestProviderConfigureAsksTheHelperWithoutHoldingTheLock(t *testing.T) {
	t.Setenv(providers.ConfigHomeEnv, t.TempDir())
	record := filepath.Join(t.TempDir(), "request.json")

	// The helper fails unless it can take the configuration lock, then saves a
	// different installation ID as another setup would.
	_, err := configureProvider(t, providerDocument(t, "work", "swap", record))
	require.ErrorContains(t, err, "run the setup again")
	store, err := providers.DefaultStore()
	require.NoError(t, err)
	file, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, providerTestSwappedInstallation, file.InstallationID)
	require.Empty(t, file.Providers)

	out, err := configureProvider(t, providerDocument(t, "work", "ready", record))
	require.NoError(t, err, out)
	require.Equal(t, providerTestSwappedInstallation, recordedRequest(t, record).InstallationID)
}
