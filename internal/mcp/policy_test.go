package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestMCPPolicyNarrowsSourcesAndRechecksCurrentGrant(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	principal := api.Principal{SubjectID: "subject:mcp", CredentialKind: "machine",
		Audience: "docbank:test", Operations: []api.Operation{api.OperationRead},
		SourceIDs: []string{"allowed", "second"}, GrantRevision: 4, ExpiresAt: now.Add(time.Hour)}
	authority := &mcpGrantAuthority{grant: principal}
	policy := newOperationPolicy(api.NewOperationPolicy(api.OperationPolicyOptions{
		Authority: authority, Now: func() time.Time { return now },
	}), principal)

	decision, err := policy.authorize(t.Context(), api.OperationRead,
		[]string{"hidden", "second", "allowed", "second"}, false, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"second", "allowed"}, decision.SourceIDs)

	authority.mutate(func(grant *api.Principal) { grant.GrantRevision++ })
	_, err = policy.authorize(t.Context(), api.OperationRead, []string{"allowed"}, true, true)
	require.ErrorIs(t, err, api.ErrOperationGrantRevoked)

	authority.set(principal)
	authority.mutate(func(grant *api.Principal) { grant.ExpiresAt = now })
	_, err = policy.authorize(t.Context(), api.OperationRead, []string{"allowed"}, true, true)
	assert.ErrorIs(t, err, api.ErrOperationGrantExpired)
}

func TestScopedVersionPageDoesNotDiscloseIncompleteGrantFilteredPagination(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	principal := api.Principal{SubjectID: "synthetic-version-reader", CredentialKind: "agent",
		Audience: "docbank:test", Operations: []api.Operation{api.OperationRead},
		SourceIDs: []string{"synthetic-version"}, GrantRevision: 1, ExpiresAt: now.Add(time.Hour)}
	policy := newOperationPolicy(api.NewOperationPolicy(api.OperationPolicyOptions{
		Authority: &mcpGrantAuthority{grant: principal}, Now: func() time.Time { return now },
	}), principal)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return nil, errors.New("daemon must not be contacted for an unqualified scoped page")
	}, func(*daemonconn.Connection) error { return nil })
	_, err := listDocumentVersionsScoped(t.Context(), lease, policy,
		[]byte(`{"node_id":7,"limit":1,"offset":0}`))
	require.ErrorIs(t, err, api.ErrOperationDenied)
}

func TestDirectMCPHiddenWriteIsDeniedBeforeDispatch(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	principal := api.Principal{SubjectID: "subject:readonly", CredentialKind: "machine",
		Audience: "docbank:test", Operations: []api.Operation{api.OperationRead},
		SourceIDs: []string{"allowed"}, GrantRevision: 1, ExpiresAt: now.Add(time.Hour)}
	authority := &mcpGrantAuthority{grant: principal}
	policy := newOperationPolicy(api.NewOperationPolicy(api.OperationPolicyOptions{
		Authority: authority, Now: func() time.Time { return now },
	}), principal)
	raw, err := json.Marshal(startProcessingInput{ContentVersionID: "hidden", PlanFingerprint: "plan"})
	require.NoError(t, err)
	_, output := startProcessingSchemas()
	result, err := executeProcessingToolWithPolicy(t.Context(), nil, newProcessingPlanRegistry(), policy,
		mustResolveSchema(output), raw)
	assert.Nil(t, result)
	require.ErrorIs(t, err, api.ErrOperationDenied)
	code, _ := stableDomainError(err)
	assert.Equal(t, "operation_denied", code)
}

func TestMCPProtectedSelectorUsesNotFound(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	principal := api.Principal{SubjectID: "subject:selector", CredentialKind: "machine",
		Audience: "docbank:test", Operations: []api.Operation{api.OperationRead},
		SourceIDs: []string{"allowed"}, GrantRevision: 1, ExpiresAt: now.Add(time.Hour)}
	authority := &mcpGrantAuthority{grant: principal}
	policy := newOperationPolicy(api.NewOperationPolicy(api.OperationPolicyOptions{
		Authority: authority, Now: func() time.Time { return now },
	}), principal)
	_, err := policy.authorize(t.Context(), api.OperationRead, []string{"hidden"}, true, true)
	require.ErrorIs(t, err, api.ErrOperationNotFound)
	code, _ := stableDomainError(err)
	assert.Equal(t, "not_found", code)
}

type mcpGrantAuthority struct {
	mu    sync.Mutex
	grant api.Principal
}

func (a *mcpGrantAuthority) CurrentGrant(context.Context, string) (api.Principal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.grant, nil
}

func (a *mcpGrantAuthority) set(grant api.Principal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.grant = grant
}

func (a *mcpGrantAuthority) mutate(fn func(*api.Principal)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fn(&a.grant)
}
