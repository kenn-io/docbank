package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/docbank/provider"
	"go.kenn.io/docbank/docbank/provider/exampleservice"
	"go.kenn.io/docbank/internal/config"
)

const providerExampleHelperArg = "docbank-provider-example-helper"

// exampleProvider configures a connection named work to the example hosting
// service and returns the service's state file.
func exampleProvider(t *testing.T) string {
	t.Helper()
	t.Setenv(config.ProviderConfigHomeEnv, t.TempDir())
	state := filepath.Join(t.TempDir(), "service.json")
	exampleService(t, state, "init", "--vault", "Studio documents")
	executable, err := os.Executable()
	require.NoError(t, err)
	document, err := json.Marshal(map[string]any{
		"version": 1, "name": "work",
		"origin": "https://vaults.example.test", "mount_prefix": "/documents",
		"browser_origin": "https://app.example.test",
		"helper":         []string{executable, providerExampleHelperArg, "--state", state, "--browser-origin", "https://app.example.test"},
	})
	require.NoError(t, err)
	out, err := configureProvider(t, string(document))
	require.NoError(t, err, out)
	return state
}

// exampleService runs one of the example service's own commands, as the
// person deciding in a browser would.
func exampleService(t *testing.T, state, command string, flags ...string) string {
	t.Helper()
	var out, errs bytes.Buffer
	args := append(append([]string{"--state", state}, flags...), command)
	require.Equal(t, 0, exampleservice.Run(args, nil, &out, &errs), errs.String())
	return out.String()
}

func exampleServiceState(t *testing.T, path string) exampleservice.State {
	t.Helper()
	state, err := exampleservice.Load(path)
	require.NoError(t, err)
	return state
}

// decideWhenAsked answers the waiting open with decision the first time the
// command pauses for the person, in place of the pause.
func decideWhenAsked(t *testing.T, state, decision string) {
	t.Helper()
	pause := providerAccessPause
	t.Cleanup(func() { providerAccessPause = pause })
	providerAccessPause = func(context.Context) error {
		providerAccessPause = pause
		exampleService(t, state, decision)
		return nil
	}
}

func TestProviderAccessNeedsApprovalThenOpensRenewsAndReleases(t *testing.T) {
	state := exampleProvider(t)

	out, err := runCLI(t, "provider", "access", "work", "Studio documents", "--wait", "0")
	require.ErrorContains(t, err, "waiting for your approval at https://app.example.test/approve/", out)
	require.Empty(t, exampleServiceState(t, state).Opens)

	require.Equal(t, "Approved 1 waiting open(s)\n", exampleService(t, state, "approve"))
	out, err = runCLI(t, "provider", "access", "work", "Studio documents", "--wait", "0", "--json")
	require.NoError(t, err, out)
	require.NotContains(t, out, "example_", "the credential is never printed")
	var access providerAccess
	require.NoError(t, json.Unmarshal([]byte(out), &access))
	require.Equal(t, "Studio documents", access.Vault.Name)
	require.Equal(t, []provider.Action{provider.ActionRead}, access.Actions)
	require.Equal(t, "https://vaults.example.test/documents/"+access.Vault.ID.String(), access.Mount)

	opens := exampleServiceState(t, state).Opens
	require.Len(t, opens, 1)
	for _, open := range opens {
		require.Equal(t, int64(2), open.Generation, "the check renews the first credential")
		require.True(t, open.Released, "the check ends its open")
	}

	// Asking for more than the approval covers needs the person again.
	out, err = runCLI(t, "provider", "access", "work", access.Vault.ID.String(), "--actions", "upload,read", "--wait", "0")
	require.ErrorContains(t, err, "waiting for your approval", out)
	exampleService(t, state, "decline")
	out, err = runCLI(t, "provider", "access", "work", "No such vault", "--wait", "0")
	require.ErrorContains(t, err, `has no vault "No such vault"`, out)
}

func TestProviderAccessWaitsWhileThePersonDecides(t *testing.T) {
	state := exampleProvider(t)
	decideWhenAsked(t, state, "approve")
	out, err := runCLI(t, "provider", "access", "work", "Studio documents", "--actions", "read,upload", "--wait", "1m")
	require.NoError(t, err, out)
	page, rest, found := strings.Cut(out, "\nProvider work lets this computer read and upload to Studio documents\n")
	require.True(t, found, out)
	require.Regexp(t, `^Approve this request in your browser:\n  https://app\.example\.test/approve/[0-9a-f-]+$`, page)
	require.Contains(t, rest, "Address: https://vaults.example.test/documents/")
	require.Empty(t, exampleServiceState(t, state).Pending)
}

func TestProviderAccessReportsADeclinedRequest(t *testing.T) {
	state := exampleProvider(t)
	decideWhenAsked(t, state, "decline")
	out, err := runCLI(t, "provider", "access", "work", "Studio documents", "--wait", "1m")
	require.ErrorContains(t, err, "provider work denied the open (approval_declined)", out)
	require.Empty(t, exampleServiceState(t, state).Opens)
}
