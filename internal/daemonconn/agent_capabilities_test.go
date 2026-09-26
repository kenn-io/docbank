package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/agentops"
	"go.kenn.io/docbank/internal/api"
)

func TestAgentCapabilitiesRejectInconsistentDaemonAuthority(t *testing.T) {
	operation := agentops.CurrentOperations()[0]
	for _, candidate := range agentops.CurrentOperations() {
		if candidate.ID == "list_tags" {
			operation = candidate
			break
		}
	}
	for _, testCase := range []struct {
		name   string
		routes []agentops.Route
		digest string
	}{
		{name: "missing referenced route", digest: "sha256:" + strings.Repeat("a", 64)},
		{name: "invalid registry digest", routes: []agentops.Route{{ID: "GET /api/v1/tags", Method: "GET", Pattern: "/api/v1/tags", Class: agentops.Read}}, digest: "bad"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/agent/capabilities" {
					t.Errorf("wrong capability path: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.MarshalWrite(w, api.AgentCapabilities{Contract: agentops.Schema,
					Server: agentops.ServerCapabilities{VaultID: "11111111-1111-4111-8111-111111111111",
						Version: "v1", RegistryDigest: testCase.digest,
						Routes: testCase.routes, Operations: []agentops.Operation{operation},
						Features: []agentops.Feature{{Name: "native_documents", State: "available"}},
					},
				}); err != nil {
					t.Errorf("write synthetic response: %v", err)
				}
			}))
			t.Cleanup(server.Close)
			_, err := New(server.URL, "synthetic-key").AgentCapabilities(t.Context())
			require.Error(t, err)
		})
	}
}

func TestAgentSessionPrincipalBindsExactLiveGrant(t *testing.T) {
	vaultID := "11111111-1111-4111-8111-111111111111"
	versionID := "22222222-2222-4222-8222-222222222222"
	session := api.AgentSessionProjection{
		SubjectID: "agent:" + strings.Repeat("a", 32), CredentialKind: "agent_session",
		Audience: "docbank:" + vaultID, Operations: []api.Operation{api.OperationRead},
		SourceIDs: []string{versionID}, GrantRevision: 1, ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}
	for _, testCase := range []struct {
		name   string
		change func(*api.AgentSessionProjection)
		valid  bool
	}{
		{name: "valid", valid: true},
		{name: "wrong audience", change: func(s *api.AgentSessionProjection) { s.Audience = "docbank:other" }},
		{name: "missing source", change: func(s *api.AgentSessionProjection) { s.SourceIDs = nil }},
		{name: "duplicate source", change: func(s *api.AgentSessionProjection) { s.SourceIDs = []string{versionID, versionID} }},
		{name: "expired", change: func(s *api.AgentSessionProjection) { s.ExpiresAt = time.Now().Add(-time.Minute) }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			projection := session
			if testCase.change != nil {
				testCase.change(&projection)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/agent/capabilities", r.URL.Path)
				assert.Equal(t, strings.Repeat("b", 64), r.Header.Get(api.AgentSessionHeader))
				assert.Empty(t, r.Header.Get("X-Api-Key"))
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, api.AgentCapabilities{Contract: agentops.Schema,
					Server: agentops.ServerCapabilities{VaultID: vaultID}, Session: &projection}))
			}))
			t.Cleanup(server.Close)
			connection, err := AttachAgentSession(server.URL, strings.Repeat("b", 64))
			require.NoError(t, err)
			actual, err := connection.AgentSessionPrincipal(t.Context())
			if !testCase.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, session.SourceIDs, actual.SourceIDs)
			require.Equal(t, session.SubjectID, actual.SubjectID)
			require.False(t, actual.Local)
		})
	}
}
