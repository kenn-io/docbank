package provider_test

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/docbank/provider"
)

const helperArg = "docbank-provider-test-helper"

var (
	firstVault  = provider.Vault{ID: uuid.MustParse("22222222-2222-4222-8222-222222222222"), Name: "Studio documents", VaultUUID: uuid.MustParse("33333333-3333-4333-8333-333333333333")}
	secondVault = provider.Vault{ID: uuid.MustParse("44444444-4444-4444-8444-444444444444"), Name: "Reference", VaultUUID: uuid.MustParse("55555555-5555-4555-8555-555555555555")}
)

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == helperArg {
		os.Exit(runHelper(os.Args[2]))
	}
	os.Exit(m.Run())
}

// runHelper is a helper as a hosting service would write it.
func runHelper(mode string) int {
	request, err := provider.DecodeRequest(os.Stdin)
	if err != nil {
		return 2
	}
	response := provider.Response{Version: provider.Version, Operation: request.Operation, RequestID: request.RequestID}
	switch mode {
	case "ready":
		response.Status = provider.StatusReady
		response.Discovery = &provider.Discovery{Enabled: true, CanCreate: true, Vaults: []provider.Vault{firstVault, secondVault}}
	case "unavailable":
		response.Status, response.Reason = provider.StatusUnavailable, "server_unavailable"
	case "wrong-request":
		response.Status, response.RequestID = provider.StatusSignInRequired, uuid.New()
		data, _ := json.Marshal(response)
		_, _ = os.Stdout.Write(data)
		return 0
	case "failed":
		response.Status = provider.StatusSignInRequired
		_ = provider.WriteResponse(os.Stdout, request, response)
		return 1
	case "flood":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte(" "), provider.MaxResponseBytes+1))
		return 0
	}
	if err := provider.WriteResponse(os.Stdout, request, response); err != nil {
		return 1
	}
	return 0
}

func helper(t *testing.T, mode string) []string {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	return []string{executable, helperArg, mode}
}

func discoverRequest(t *testing.T) provider.Request {
	t.Helper()
	request, err := provider.NewDiscoverRequest(provider.Connection{
		InstallationID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		Origin:         "https://vaults.example.test", MountPrefix: "/documents",
	})
	require.NoError(t, err)
	return request
}

func TestExchangeReturnsTheHelpersDecision(t *testing.T) {
	t.Parallel()
	request := discoverRequest(t)

	ready, err := provider.Exchange(t.Context(), helper(t, "ready"), request)
	require.NoError(t, err)
	require.Equal(t, provider.StatusReady, ready.Status)
	require.Equal(t, &provider.Discovery{Enabled: true, CanCreate: true, Vaults: []provider.Vault{firstVault, secondVault}}, ready.Discovery)

	refused, err := provider.Exchange(t.Context(), helper(t, "unavailable"), request)
	require.NoError(t, err)
	require.Equal(t, provider.StatusUnavailable, refused.Status)
	require.Equal(t, "server_unavailable", refused.Reason)
	require.Nil(t, refused.Discovery)
}

func TestExchangeRejectsHelpersThatDoNotAnswerTheRequest(t *testing.T) {
	t.Parallel()
	request := discoverRequest(t)
	for mode, want := range map[string]error{
		"wrong-request": provider.ErrInvalidResponse,
		"failed":        provider.ErrHelperFailed,
		"flood":         provider.ErrInvalidResponse,
	} {
		_, err := provider.Exchange(t.Context(), helper(t, mode), request)
		require.ErrorIs(t, err, want, mode)
	}
	_, err := provider.Exchange(t.Context(), []string{"example-helper"}, request)
	require.ErrorContains(t, err, "absolute executable path")
}

func TestDecodeResponseRejectsResultsOutsideTheContract(t *testing.T) {
	t.Parallel()
	request := discoverRequest(t)
	envelope := fmt.Sprintf(`"version":1,"operation":"discover","request_id":"%s"`, request.RequestID)
	vault := `{"id":"22222222-2222-4222-8222-222222222222","name":"Studio","vault_uuid":"33333333-3333-4333-8333-333333333333"}`
	valid := `{` + envelope + `,"status":"ready","discovery":{"enabled":true,"can_create":false,"vaults":[` + vault + `]}}`
	got, err := provider.DecodeResponse(strings.NewReader(valid), request)
	require.NoError(t, err)
	require.Equal(t, "Studio", got.Discovery.Vaults[0].Name)

	for name, document := range map[string]string{
		"unknown field":      `{` + envelope + `,"status":"denied","token":"x"}`,
		"duplicate key":      `{` + envelope + `,"status":"denied","status":"ready"}`,
		"trailing document":  `{` + envelope + `,"status":"denied"}{}`,
		"ready without list": `{` + envelope + `,"status":"ready","discovery":{"enabled":true,"can_create":true}}`,
		"refusal with list":  `{` + envelope + `,"status":"denied","discovery":{"enabled":true,"can_create":true,"vaults":[]}}`,
		"free-text reason":   `{` + envelope + `,"status":"denied","reason":"Ask Pat"}`,
		"unknown status":     `{` + envelope + `,"status":"approved"}`,
		"duplicate vault":    `{` + envelope + `,"status":"ready","discovery":{"enabled":true,"can_create":true,"vaults":[` + vault + `,` + vault + `]}}`,
		"control in name": `{` + envelope + `,"status":"ready","discovery":{"enabled":true,"can_create":true,"vaults":[` +
			strings.Replace(vault, "Studio", `Stu\u001b[2Jdio`, 1) + `]}}`,
		"other version": strings.Replace(valid, `"version":1`, `"version":2`, 1),
	} {
		_, err := provider.DecodeResponse(strings.NewReader(document), request)
		require.ErrorIs(t, err, provider.ErrInvalidResponse, name)
	}
}

func TestRequestsNameAnHTTPSOriginAndMountPrefix(t *testing.T) {
	t.Parallel()
	installation := uuid.New()
	for _, connection := range []provider.Connection{
		{InstallationID: installation, Origin: "http://vaults.example.test", MountPrefix: "/documents"},
		{InstallationID: installation, Origin: "https://vaults.example.test/documents", MountPrefix: "/documents"},
		{InstallationID: installation, Origin: "https://user@vaults.example.test", MountPrefix: "/documents"},
		{InstallationID: installation, Origin: "https://:443", MountPrefix: "/documents"},
		{InstallationID: installation, Origin: "https://vaults.example.test", MountPrefix: "/documents/"},
		{InstallationID: installation, Origin: "https://vaults.example.test", MountPrefix: "documents"},
		{InstallationID: uuid.Nil(), Origin: "https://vaults.example.test", MountPrefix: "/documents"},
	} {
		_, err := provider.NewDiscoverRequest(connection)
		require.ErrorIs(t, err, provider.ErrInvalidRequest, "%+v", connection)
	}
}
