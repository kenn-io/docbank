package api_test

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/capselfhosted"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

func TestMediaOriginsHTTPReportsProbeEvidence(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, func(deps *api.Deps) {
		gate := api.NewOperationGate()
		service, err := processing.NewService(processing.ServiceConfig{
			Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate, SpoolDirectory: filepath.Join(deps.VaultRoot, "blobs", "tmp"),
			Principal: "daemon:operator", MediaTokenKey: [32]byte{1},
			MediaOrigins: map[string]processing.MediaOriginPolicy{"team-cap": {
				OriginID: "team-cap", Provider: capselfhosted.Provider,
				ResolverFingerprint: "resolver", IdentityFingerprint: "identity", DisclosureFingerprint: "disclosure",
				InputClasses: []string{"recording_reference"}, ExactOrigin: "https://cap.example.test",
				CredentialBinding: "credential:cap-a", RecognizePath: capselfhosted.RecognizeSharePath,
			}},
			MediaOriginProbes: map[string]processing.MediaOriginProbe{"team-cap": func(context.Context) (processing.MediaOriginEvidence, error) {
				return processing.MediaOriginEvidence{AdapterContract: capselfhosted.AdapterContract,
					DeploymentRevision: "v-synthetic", ProbeState: string(capselfhosted.ProbeVerified)}, nil
			}},
		})
		require.NoError(t, err)
		require.NoError(t, service.ProbeMediaOrigins(t.Context()))
		deps.Gate = gate
		deps.Processing = service
	})
	client := daemonconn.New(ts.URL, testAPIKey)
	origins, err := client.MediaOrigins(t.Context())
	require.NoError(t, err)
	require.Len(t, origins.Items, 1)
	require.Equal(t, capselfhosted.AdapterContract, origins.Items[0].AdapterContract)
	require.Equal(t, "v-synthetic", origins.Items[0].DeploymentRevision)
	require.Equal(t, string(capselfhosted.ProbeVerified), origins.Items[0].ProbeState)
	require.NotEmpty(t, origins.Items[0].ProbedAt)
	_, err = time.Parse(time.RFC3339Nano, origins.Items[0].ProbedAt)
	require.NoError(t, err)

	reference := "https://cap.example.test/s/vid-1?secret=synthetic"
	response, body := do(t, ts, http.MethodPost, "/api/v1/media/sources", nil, map[string]any{
		"operation_id": uuid.New().String(), "reference_url": reference,
		"credential_binding": "credential:cap-b", "occurrence": map[string]any{
			"ref": "synthetic-message", "revision": "1", "filename": "call.wav",
		},
	})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	require.Contains(t, body, "credential_cross_origin")
	require.NotContains(t, body, reference)
	require.NotContains(t, body, "credential:cap-b")
}
