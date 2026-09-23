package mcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestMCPPolicyWritesSafeAuditRecord(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	policy := newOperationPolicy(nil, api.LocalAdminPrincipal(), logger)
	_, err := policy.authorize(t.Context(), api.OperationRead, []string{"private-source"}, true, true)
	require.NoError(t, err)
	assert.Contains(t, output.String(), `"msg":"operation_authorization"`)
	assert.Contains(t, output.String(), `"outcome":"allowed"`)
	assert.NotContains(t, output.String(), "private-source")
}

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
