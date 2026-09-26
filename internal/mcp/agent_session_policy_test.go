package mcp

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/agentops"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestScopedAgentSessionOptionsLoadAndRecheckLiveGrant(t *testing.T) {
	vaultID := "11111111-1111-4111-8111-111111111111"
	versionID := "22222222-2222-4222-8222-222222222222"
	var revoked atomic.Bool
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/agent/capabilities", r.URL.Path)
		if revoked.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, api.AgentCapabilities{Contract: agentops.Schema,
			Server: agentops.ServerCapabilities{VaultID: vaultID},
			Session: &api.AgentSessionProjection{
				SubjectID: "agent:" + strings.Repeat("a", 32), CredentialKind: "agent_session",
				Audience: "docbank:" + vaultID, Operations: []api.Operation{api.OperationRead},
				SourceIDs: []string{versionID}, GrantRevision: 1, ExpiresAt: time.Now().Add(time.Hour),
			},
		}))
	}))
	t.Cleanup(daemon.Close)
	connection, err := daemonconn.AttachAgentSession(daemon.URL, strings.Repeat("b", 64))
	require.NoError(t, err)
	options, err := ScopedAgentSessionOptions(t.Context(), connection, ServerOptions{AllowExportWrites: true})
	require.NoError(t, err)
	require.True(t, options.ScopedAgentSession)
	require.False(t, options.Principal.Local)
	require.Equal(t, []string{versionID}, options.Principal.SourceIDs)
	require.False(t, options.AllowExportWrites)
	policy := newOperationPolicy(options.OperationPolicy, options.Principal)
	decision, err := policy.authorize(t.Context(), api.OperationRead, []string{versionID}, true, true)
	require.NoError(t, err)
	require.Equal(t, []string{versionID}, decision.SourceIDs)
	revoked.Store(true)
	_, err = policy.authorize(context.Background(), api.OperationRead, []string{versionID}, true, true)
	require.ErrorIs(t, err, api.ErrOperationGrantRevoked)
}
