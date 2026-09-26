package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
