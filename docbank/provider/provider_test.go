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

var testConnection = provider.Connection{
	InstallationID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
	Origin:         "https://vaults.example.test", MountPrefix: "/documents",
}

func openRequest(t *testing.T, actions ...provider.Action) provider.Request {
	t.Helper()
	request, err := provider.NewOpenRequest(testConnection, provider.VaultAccess{
		VaultID: firstVault.ID, VaultUUID: firstVault.VaultUUID, Actions: actions,
	})
	require.NoError(t, err)
	return request
}

const (
	readyAccess = `"access":{"actions":["read"],"mount":"https://vaults.example.test/documents/22222222-2222-4222-8222-222222222222",` +
		`"credential":"secret-1","generation":1,"expires_at":"2030-01-01T00:15:00Z","approval_expires_at":"2030-03-01T00:00:00Z"}`
)

func envelope(request provider.Request) string {
	return fmt.Sprintf(`"version":1,"operation":%q,"request_id":%q`, request.Operation, request.RequestID)
}

func TestOpenRenewAndReleaseResults(t *testing.T) {
	t.Parallel()
	open := openRequest(t, provider.ActionRead, provider.ActionUpload)

	ready, err := provider.DecodeResponse(strings.NewReader(`{`+envelope(open)+`,"status":"ready",`+readyAccess+`}`), open)
	require.NoError(t, err)
	require.Equal(t, "secret-1", ready.Access.Credential)
	require.Equal(t, []provider.Action{provider.ActionRead}, ready.Access.Actions)
	require.Equal(t, int64(1), ready.Access.Generation)

	pending, err := provider.DecodeResponse(strings.NewReader(
		`{`+envelope(open)+`,"status":"approval_required","approval_url":"https://vaults.example.test/approve/1?from=app"}`), open)
	require.NoError(t, err)
	require.Equal(t, "https://vaults.example.test/approve/1?from=app", pending.ApprovalURL)

	renew, err := provider.NewRenewRequest(open, 1)
	require.NoError(t, err)
	require.Equal(t, open.RequestID, renew.OpenRequestID)
	renewed := strings.NewReplacer(`"generation":1`, `"generation":2`, "secret-1", "secret-2").Replace(readyAccess)
	next, err := provider.DecodeResponse(strings.NewReader(`{`+envelope(renew)+`,"status":"ready",`+renewed+`}`), renew)
	require.NoError(t, err)
	require.Equal(t, int64(2), next.Access.Generation)

	issued := strings.Replace(renewed, `"credential":"secret-2",`, "", 1)
	conflict, err := provider.DecodeResponse(strings.NewReader(
		`{`+envelope(renew)+`,"status":"conflict","reason":"already_issued",`+issued+`}`), renew)
	require.NoError(t, err)
	require.Empty(t, conflict.Access.Credential)

	release, err := provider.NewReleaseRequest(open)
	require.NoError(t, err)
	released, err := provider.DecodeResponse(strings.NewReader(`{`+envelope(release)+`,"status":"released"}`), release)
	require.NoError(t, err)
	require.Equal(t, provider.StatusReleased, released.Status)
}

func TestRootMountPrefixAcceptsVaultsAndRejectsTheRoot(t *testing.T) {
	t.Parallel()
	connection := testConnection
	connection.MountPrefix = "/"
	open, err := provider.NewOpenRequest(connection, provider.VaultAccess{
		VaultID: firstVault.ID, VaultUUID: firstVault.VaultUUID, Actions: []provider.Action{provider.ActionRead},
	})
	require.NoError(t, err)
	renew, err := provider.NewRenewRequest(open, 1)
	require.NoError(t, err)
	access := func(mountPath string, generation int) string {
		return strings.NewReplacer("/documents/22222222-2222-4222-8222-222222222222", mountPath,
			`"generation":1`, fmt.Sprintf(`"generation":%d`, generation)).Replace(readyAccess)
	}
	const vaultPath = "/22222222-2222-4222-8222-222222222222"

	for _, c := range []struct {
		request    provider.Request
		generation int
	}{{open, 1}, {renew, 2}} {
		ready, err := provider.DecodeResponse(strings.NewReader(
			`{`+envelope(c.request)+`,"status":"ready",`+access(vaultPath, c.generation)+`}`), c.request)
		require.NoError(t, err, c.request.Operation)
		require.Equal(t, "https://vaults.example.test"+vaultPath, ready.Access.Mount)

		for _, root := range []string{"/", "", "//" + vaultPath[1:]} {
			_, err = provider.DecodeResponse(strings.NewReader(
				`{`+envelope(c.request)+`,"status":"ready",`+access(root, c.generation)+`}`), c.request)
			require.ErrorIs(t, err, provider.ErrInvalidResponse, "%s %q", c.request.Operation, root)
		}
	}
}

func TestMountPrefixesThatNeedEscapingHaveOneAcceptedAddress(t *testing.T) {
	t.Parallel()
	const vault = "/22222222-2222-4222-8222-222222222222"
	for prefix, escaped := range map[string]string{
		"/my documents": "/my%20documents",
		"/文档":           "/%E6%96%87%E6%A1%A3",
	} {
		connection := testConnection
		connection.MountPrefix = prefix
		open, err := provider.NewOpenRequest(connection, provider.VaultAccess{
			VaultID: firstVault.ID, VaultUUID: firstVault.VaultUUID, Actions: []provider.Action{provider.ActionRead},
		})
		require.NoError(t, err)
		renew, err := provider.NewRenewRequest(open, 1)
		require.NoError(t, err)
		for _, c := range []struct {
			request    provider.Request
			generation int
		}{{open, 1}, {renew, 2}} {
			decode := func(mountPath string) (provider.Response, error) {
				access := strings.NewReplacer("/documents"+vault, mountPath,
					`"generation":1`, fmt.Sprintf(`"generation":%d`, c.generation)).Replace(readyAccess)
				return provider.DecodeResponse(strings.NewReader(
					`{`+envelope(c.request)+`,"status":"ready",`+access+`}`), c.request)
			}
			ready, err := decode(escaped + vault)
			require.NoError(t, err, "%s %s", c.request.Operation, prefix)
			require.Equal(t, "https://vaults.example.test"+escaped+vault, ready.Access.Mount)

			for _, other := range []string{
				prefix + vault,                           // not percent-encoded
				strings.ToLower(escaped) + vault + "%41", // a letter encoded needlessly
				escaped + "%2F" + vault[1:],              // an encoded separator
				escaped + "/%2E%2E" + vault,              // an encoded climb
			} {
				_, err = decode(other)
				require.ErrorIs(t, err, provider.ErrInvalidResponse, "%s %q", c.request.Operation, other)
			}
		}
	}
}

func TestOpenAndRenewRejectResultsOutsideTheContract(t *testing.T) {
	t.Parallel()
	open := openRequest(t, provider.ActionRead)
	renew, err := provider.NewRenewRequest(open, 1)
	require.NoError(t, err)
	release, err := provider.NewReleaseRequest(open)
	require.NoError(t, err)
	discover := discoverRequest(t)
	ready := func(request provider.Request, access string) string {
		return `{` + envelope(request) + `,"status":"ready",` + access + `}`
	}
	change := func(old, replacement string) string { return strings.Replace(readyAccess, old, replacement, 1) }

	for name, c := range map[string]struct {
		request  provider.Request
		document string
	}{
		"ready without a credential":      {open, ready(open, change(`"credential":"secret-1",`, ""))},
		"credential with a space":         {open, ready(open, change("secret-1", "secret 1"))},
		"conflict with a credential":      {open, `{` + envelope(open) + `,"status":"conflict",` + readyAccess + `}`},
		"mount on another origin":         {open, ready(open, change("vaults.example.test/documents", "other.example.test/documents"))},
		"mount outside the prefix":        {open, ready(open, change("/documents/2222", "/other/2222"))},
		"mount is the prefix":             {open, ready(open, change("/documents/22222222-2222-4222-8222-222222222222", "/documents"))},
		"mount climbs out of the prefix":  {open, ready(open, change("/documents/2222", "/documents/../2222"))},
		"mount with a query":              {open, ready(open, change(`8222-222222222222",`, `8222-222222222222?to=x",`))},
		"action the request did not name": {open, ready(open, change(`["read"]`, `["upload"]`))},
		"unknown action":                  {open, ready(open, change(`["read"]`, `["delete"]`))},
		"no actions":                      {open, ready(open, change(`["read"]`, `[]`))},
		"generation zero":                 {open, ready(open, change(`"generation":1`, `"generation":0`))},
		"renewal that does not advance":   {renew, ready(renew, readyAccess)},
		"credential outlives approval":    {open, ready(open, change("2030-03-01", "2029-03-01"))},
		"expiry with an offset":           {open, ready(open, change("00:15:00Z", "00:15:00+02:00"))},
		"missing approval end":            {open, ready(open, change(`,"approval_expires_at":"2030-03-01T00:00:00Z"`, ""))},
		"approval for a renewal":          {renew, `{` + envelope(renew) + `,"status":"approval_required","approval_url":"https://vaults.example.test/approve"}`},
		"approval page without HTTPS":     {open, `{` + envelope(open) + `,"status":"approval_required","approval_url":"http://vaults.example.test/approve"}`},
		"approval page with a login":      {open, `{` + envelope(open) + `,"status":"approval_required","approval_url":"https://pat@vaults.example.test/approve"}`},
		"approval without a page":         {open, `{` + envelope(open) + `,"status":"approval_required"}`},
		"released open":                   {open, `{` + envelope(open) + `,"status":"released"}`},
		"release that grants access":      {release, ready(release, readyAccess)},
		"discovery that grants access":    {discover, ready(discover, readyAccess)},
		"open answered with vaults":       {open, `{` + envelope(open) + `,"status":"ready","discovery":{"enabled":true,"can_create":true,"vaults":[]}}`},
		"refusal with access":             {open, `{` + envelope(open) + `,"status":"denied",` + readyAccess + `}`},
	} {
		_, err := provider.DecodeResponse(strings.NewReader(c.document), c.request)
		require.ErrorIs(t, err, provider.ErrInvalidResponse, name)
	}
}

func TestVaultRequestsNameOneVaultAndItsActions(t *testing.T) {
	t.Parallel()
	open := openRequest(t, provider.ActionRead, provider.ActionUpload)
	document, err := json.Marshal(open)
	require.NoError(t, err)
	decoded, err := provider.DecodeRequest(bytes.NewReader(document))
	require.NoError(t, err)
	require.Equal(t, open, decoded)

	for name, change := range map[string][2]string{
		"no actions":            {`"actions":["read","upload"]`, `"actions":[]`},
		"unknown action":        {`"upload"`, `"delete"`},
		"repeated action":       {`"upload"`, `"read"`},
		"unsorted actions":      {`["read","upload"]`, `["upload","read"]`},
		"no vault identity":     {`"vault_uuid":"33333333-3333-4333-8333-333333333333",`, ""},
		"open of another open":  {`"actions"`, `"open_request_id":"66666666-6666-4666-8666-666666666666","actions"`},
		"open with generation":  {`"actions"`, `"generation":1,"actions"`},
		"discovery of a vault":  {`"operation":"open"`, `"operation":"discover"`},
		"renewal of no open":    {`"operation":"open"`, `"operation":"renew"`},
		"release of no open":    {`"operation":"open"`, `"operation":"release"`},
		"operation not offered": {`"operation":"open"`, `"operation":"delete"`},
	} {
		changed := strings.Replace(string(document), change[0], change[1], 1)
		require.NotEqual(t, string(document), changed, name)
		_, err := provider.DecodeRequest(strings.NewReader(changed))
		require.ErrorIs(t, err, provider.ErrInvalidRequest, name)
	}

	_, err = provider.NewRenewRequest(open, 0)
	require.ErrorIs(t, err, provider.ErrInvalidRequest)
	_, err = provider.NewReleaseRequest(discoverRequest(t))
	require.ErrorIs(t, err, provider.ErrInvalidRequest)
}

func TestApprovalPagesStayOnTheBrowserOrigin(t *testing.T) {
	t.Parallel()
	const origin = "https://vaults.example.test"
	require.NoError(t, provider.ValidateApprovalURL(origin+"/approve/1?from=app", origin))
	for _, page := range []string{
		"https://other.example.test/approve/1",
		"https://vaults.example.test.other.example/approve/1",
		"https://vaults.example.test:8443/approve/1",
		"http://vaults.example.test/approve/1",
		"https://pat@vaults.example.test/approve/1",
		"https://vaults.example.test/approve/1#decide",
		"/approve/1",
	} {
		require.Error(t, provider.ValidateApprovalURL(page, origin), page)
	}
	require.Error(t, provider.ValidateApprovalURL(origin+"/approve/1", origin+"/app"))
}
