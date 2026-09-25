package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentSessionHeaderHasOneStableOwnerAndNoCredentialFallback(t *testing.T) {
	now := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	sessions := newAgentSessionRegistry("synthetic-vault", func() time.Time { return now }, nil)
	issued, err := sessions.issue(agentSessionGrant{Operations: []Operation{OperationRead}, SourceIDs: []string{"synthetic-source"}, TTL: time.Minute})
	require.NoError(t, err)
	var owners []string
	handler := authMiddlewareWithAgentSessions(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, ok := workspaceSnapshotOwner(r.Context())
		assert.True(t, ok)
		principal, ok := PrincipalFromContext(r.Context())
		assert.True(t, ok)
		assert.Equal(t, "agent_session", principal.CredentialKind)
		owners = append(owners, owner)
		w.WriteHeader(http.StatusNoContent)
	}), "master-key", nil, "master-owner", nil, nil, sessions)

	call := func(token string, extra map[string]string) int {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
		request.Header.Set(AgentSessionHeader, token)
		for key, value := range extra {
			request.Header.Set(key, value)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}
	require.Equal(t, http.StatusNoContent, call(issued.Token, nil))
	require.Equal(t, http.StatusNoContent, call(issued.Token, nil))
	require.Equal(t, []string{owners[0], owners[0]}, owners)
	require.NotEqual(t, "master-owner", owners[0])
	require.Equal(t, http.StatusUnauthorized, call(issued.Token, map[string]string{"X-Api-Key": "master-key"}))
	require.Equal(t, http.StatusUnauthorized, call("forged", nil))
	require.True(t, sessions.revoke(issued.ID))
	require.Equal(t, http.StatusUnauthorized, call(issued.Token, nil))
	require.Len(t, owners, 2)
}

func TestAgentSessionGrantIsBoundedAndRevokedWithItsOwner(t *testing.T) {
	now := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	var revoked []string
	registry := newAgentSessionRegistry("synthetic-vault", func() time.Time { return now },
		func(owner string) { revoked = append(revoked, owner) })
	issued, err := registry.issue(agentSessionGrant{
		Operations: []Operation{OperationRead}, SourceIDs: []string{"synthetic-source"},
		TTL: time.Minute,
	})
	require.NoError(t, err)
	require.NotEmpty(t, issued.ID)
	require.NotEmpty(t, issued.Token)
	require.NotEqual(t, issued.ID, issued.Token)
	require.Equal(t, now.Add(time.Minute), issued.ExpiresAt)

	principal, lifetime, ok := registry.authenticate(issued.Token)
	require.True(t, ok)
	require.Equal(t, "agent_session", principal.CredentialKind)
	require.Equal(t, "docbank:synthetic-vault", principal.Audience)
	require.Equal(t, []Operation{OperationRead}, principal.Operations)
	require.Equal(t, []string{"synthetic-source"}, principal.SourceIDs)
	policy := NewOperationPolicy(OperationPolicyOptions{Authority: registry,
		Now: func() time.Time { return now }})
	decision, err := policy.Authorize(t.Context(), OperationAuthorizationRequest{
		Principal: principal, Operation: OperationRead,
		SourceIDs: []string{"synthetic-source"}, RequireAll: true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, decision.CacheKey)
	_, err = policy.Authorize(t.Context(), OperationAuthorizationRequest{
		Principal: principal, Operation: OperationRead,
		SourceIDs: []string{"foreign-source"}, RequireAll: true,
	})
	require.ErrorIs(t, err, ErrOperationDenied)

	require.True(t, registry.revoke(issued.ID))
	select {
	case <-lifetime.Done():
	case <-t.Context().Done():
		t.Fatal("revocation did not cancel the active session")
	}
	_, _, ok = registry.authenticate(issued.Token)
	require.False(t, ok)
	_, err = registry.CurrentGrant(context.Background(), principal.SubjectID)
	require.ErrorIs(t, err, ErrOperationGrantRevoked)
	require.Len(t, revoked, 1)
	require.Equal(t, operationCacheKey(principal.SubjectID, principal.GrantRevision, OperationRead, nil), revoked[0])
}

func TestAgentSessionRejectsEmptySourceScope(t *testing.T) {
	registry := newAgentSessionRegistry("synthetic-vault", time.Now, nil)
	_, err := registry.issue(agentSessionGrant{Operations: []Operation{OperationRead}, TTL: time.Minute})
	require.ErrorIs(t, err, ErrOperationDenied)
}

func TestAgentSessionRegistryEnforcesCapacityAndExpiry(t *testing.T) {
	now := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	registry := newAgentSessionRegistry("synthetic-vault", func() time.Time { return now }, nil)
	for range maxAgentSessions {
		_, err := registry.issue(agentSessionGrant{Operations: []Operation{OperationRead}, SourceIDs: []string{"synthetic-source"}, TTL: time.Minute})
		require.NoError(t, err)
	}
	_, err := registry.issue(agentSessionGrant{Operations: []Operation{OperationRead}, SourceIDs: []string{"synthetic-source"}, TTL: time.Minute})
	require.ErrorIs(t, err, ErrAgentSessionLimit)
	now = now.Add(2 * time.Minute)
	issued, err := registry.issue(agentSessionGrant{Operations: []Operation{OperationRead}, SourceIDs: []string{"synthetic-source"}, TTL: time.Minute})
	require.NoError(t, err, "expired sessions must release bounded capacity")
	_, _, ok := registry.authenticate(issued.Token)
	require.True(t, ok)
	now = now.Add(time.Minute)
	_, _, ok = registry.authenticate(issued.Token)
	require.False(t, ok, "a token is invalid at its expiry instant")
	_, err = registry.issue(agentSessionGrant{Operations: []Operation{OperationRead}, TTL: 24 * time.Hour})
	require.Error(t, err, "a caller cannot mint an unbounded credential")
	_, err = registry.issue(agentSessionGrant{Operations: []Operation{OperationMetadataMutation}, TTL: time.Minute})
	require.ErrorIs(t, err, ErrOperationDenied, "write grants need a publication fence before minting")
}

func TestAgentSessionRegistryShutdownCancelsEveryLease(t *testing.T) {
	registry := newAgentSessionRegistry("synthetic-vault", time.Now, nil)
	issued, err := registry.issue(agentSessionGrant{Operations: []Operation{OperationRead}, SourceIDs: []string{"synthetic-source"}, TTL: time.Minute})
	require.NoError(t, err)
	_, lifetime, ok := registry.authenticate(issued.Token)
	require.True(t, ok)
	registry.closeAll()
	require.ErrorIs(t, lifetime.Err(), context.Canceled)
	_, _, ok = registry.authenticate(issued.Token)
	require.False(t, ok)
}

func TestAgentSessionRevocationRunsCleanupAfterRegistryUnlock(t *testing.T) {
	var registry *agentSessionRegistry
	var subject string
	cleanupDone := make(chan struct{})
	registry = newAgentSessionRegistry("synthetic-vault", time.Now, func(string) {
		_, _ = registry.CurrentGrant(context.Background(), subject)
		close(cleanupDone)
	})
	issued, err := registry.issue(agentSessionGrant{Operations: []Operation{OperationRead}, SourceIDs: []string{"synthetic-source"}, TTL: time.Minute})
	require.NoError(t, err)
	principal, _, ok := registry.authenticate(issued.Token)
	require.True(t, ok)
	subject = principal.SubjectID
	go registry.revoke(issued.ID)
	select {
	case <-cleanupDone:
	case <-time.After(2 * time.Second):
		t.Fatal("revoke cleanup blocked on the session registry lock")
	}
}

func TestAgentSessionExpiryCancelsActiveRequestWithoutAnotherRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newAgentSessionRegistry("synthetic-vault", time.Now, nil)
		issued, err := registry.issue(agentSessionGrant{Operations: []Operation{OperationRead}, SourceIDs: []string{"synthetic-source"}, TTL: time.Minute})
		require.NoError(t, err)
		_, lifetime, ok := registry.authenticate(issued.Token)
		require.True(t, ok)
		select {
		case <-lifetime.Done():
		case <-time.After(2 * time.Minute):
			t.Fatal("agent session did not cancel its active request at expiry")
		}
	})
}
