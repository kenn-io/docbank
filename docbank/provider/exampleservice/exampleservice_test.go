package exampleservice_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/docbank/provider"
	"go.kenn.io/docbank/docbank/provider/exampleservice"
)

const helperArg = "docbank-example-helper"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == helperArg {
		os.Exit(exampleservice.Run(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// service is one example service: its state file and the helper argv Docbank
// would be configured with.
type service struct {
	t      *testing.T
	state  string
	helper []string
}

func newService(t *testing.T) service {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	s := service{t: t, state: filepath.Join(t.TempDir(), "service.json")}
	s.helper = []string{executable, helperArg, "--state", s.state, "--browser-origin", "https://app.example.test"}
	require.Empty(t, s.run("init", "--vault", "Studio documents"))
	return s
}

// run runs an operator command, as the person at the service would.
func (s service) run(command string, flags ...string) string {
	s.t.Helper()
	var out, errs bytes.Buffer
	args := append(append([]string{"--state", s.state}, flags...), command)
	require.Equal(s.t, 0, exampleservice.Run(args, nil, &out, &errs), errs.String())
	return out.String()
}

func (s service) ask(request provider.Request) provider.Response {
	s.t.Helper()
	response, err := provider.Exchange(s.t.Context(), s.helper, request)
	require.NoError(s.t, err)
	return response
}

func (s service) connection() provider.Connection {
	return provider.Connection{InstallationID: uuid.New(), Origin: "https://vaults.example.test", MountPrefix: "/documents"}
}

func (s service) vault(connection provider.Connection) provider.Vault {
	s.t.Helper()
	discover, err := provider.NewDiscoverRequest(connection)
	require.NoError(s.t, err)
	found := s.ask(discover)
	require.Equal(s.t, provider.StatusReady, found.Status)
	require.Len(s.t, found.Discovery.Vaults, 1)
	return found.Discovery.Vaults[0]
}

func (s service) openRequest(connection provider.Connection, actions ...provider.Action) provider.Request {
	s.t.Helper()
	vault := s.vault(connection)
	request, err := provider.NewOpenRequest(connection, provider.VaultAccess{VaultID: vault.ID, VaultUUID: vault.VaultUUID, Actions: actions})
	require.NoError(s.t, err)
	return request
}

func TestAnOpenWaitsForThePersonThenRenewsAndReleases(t *testing.T) {
	t.Parallel()
	s := newService(t)
	connection := s.connection()
	require.Equal(t, "Studio documents", s.vault(connection).Name)
	open := s.openRequest(connection, provider.ActionRead)

	waiting := s.ask(open)
	require.Equal(t, provider.StatusApprovalRequired, waiting.Status)
	require.NoError(t, provider.ValidateApprovalURL(waiting.ApprovalURL, "https://app.example.test"))
	require.Equal(t, waiting.ApprovalURL, s.ask(open).ApprovalURL, "asking again shows the same page")

	require.Equal(t, "Approved 1 waiting open(s)\n", s.run("approve"))
	ready := s.ask(open)
	require.Equal(t, provider.StatusReady, ready.Status)
	require.Equal(t, int64(1), ready.Access.Generation)
	require.Equal(t, "https://vaults.example.test/documents/"+open.VaultID.String(), ready.Access.Mount)
	require.Equal(t, exampleservice.CredentialLifetime, ready.Access.ExpiresAt.Sub(ready.Access.ApprovalExpiresAt.Add(-exampleservice.ApprovalLifetime)))

	repeated := s.ask(open)
	require.Equal(t, provider.StatusConflict, repeated.Status, "a credential is returned once")
	require.Empty(t, repeated.Access.Credential)

	renew, err := provider.NewRenewRequest(open, 1)
	require.NoError(t, err)
	renewed := s.ask(renew)
	require.Equal(t, provider.StatusReady, renewed.Status)
	require.Equal(t, int64(2), renewed.Access.Generation)
	require.NotEqual(t, ready.Access.Credential, renewed.Access.Credential)

	stale, err := provider.NewRenewRequest(open, 1)
	require.NoError(t, err)
	behind := s.ask(stale)
	require.Equal(t, provider.StatusConflict, behind.Status)
	require.Equal(t, exampleservice.ReasonGenerationChanged, behind.Reason)
	require.Equal(t, int64(2), behind.Access.Generation)

	release, err := provider.NewReleaseRequest(open)
	require.NoError(t, err)
	require.Equal(t, provider.StatusReleased, s.ask(release).Status)
	current, err := provider.NewRenewRequest(open, 2)
	require.NoError(t, err)
	ended := s.ask(current)
	require.Equal(t, provider.StatusDenied, ended.Status)
	require.Equal(t, exampleservice.ReasonOpenReleased, ended.Reason)

	// The approval outlives the open, for this installation and these actions only.
	require.Equal(t, provider.StatusReady, s.ask(s.openRequest(connection, provider.ActionRead)).Status)
	require.Equal(t, provider.StatusApprovalRequired, s.ask(s.openRequest(connection, provider.ActionRead, provider.ActionUpload)).Status)
	require.Equal(t, provider.StatusApprovalRequired, s.ask(s.openRequest(s.connection(), provider.ActionRead)).Status)
}

func TestADeclinedOpenIsDeniedAndAnotherVaultIsUnknown(t *testing.T) {
	t.Parallel()
	s := newService(t)
	connection := s.connection()
	open := s.openRequest(connection, provider.ActionUpload)
	require.Equal(t, provider.StatusApprovalRequired, s.ask(open).Status)
	require.Equal(t, "Declined 1 waiting open(s)\n", s.run("decline"))
	declined := s.ask(open)
	require.Equal(t, provider.StatusDenied, declined.Status)
	require.Equal(t, exampleservice.ReasonApprovalDeclined, declined.Reason)
	require.Equal(t, "Approved 0 waiting open(s)\n", s.run("approve"), "a declined open is decided")

	other, err := provider.NewOpenRequest(connection, provider.VaultAccess{
		VaultID: uuid.New(), VaultUUID: uuid.New(), Actions: []provider.Action{provider.ActionRead},
	})
	require.NoError(t, err)
	unknown := s.ask(other)
	require.Equal(t, provider.StatusDenied, unknown.Status)
	require.Equal(t, exampleservice.ReasonVaultUnknown, unknown.Reason)
}
