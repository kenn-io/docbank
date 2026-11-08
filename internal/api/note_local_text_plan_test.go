package api_test

import (
	"encoding/json/v2"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestNoteLocalTextPlanRequiresAuthenticationAndConsent(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, func(deps *api.Deps) {
		name, profile, err := processing.NewNotePlaintextProfile()
		require.NoError(t, err)
		gate := api.NewOperationGate()
		deps.Gate = gate
		deps.Processing, err = processing.NewService(t.Context(), processing.ServiceConfig{
			Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate,
			SpoolDirectory: filepath.Join(deps.VaultRoot, "blobs", "tmp"),
			Principal:      "daemon:operator", Scope: "document-processing",
			Profiles: map[string]processing.ProfileConfig{name: profile},
		})
		require.NoError(t, err)
	})
	const route = "/api/v1/notes/local-text-plan"
	response, _ := do(t, ts, http.MethodGet, route, map[string]string{"X-Api-Key": ""}, nil)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	response, body := do(t, ts, http.MethodGet, route, nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var plan api.NoteLocalTextPlan
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	require.Equal(t, processing.NotePlaintextProfileName, plan.Plan.Selector.Profile)
	require.True(t, plan.Plan.ConsentRequired)
	require.NotEmpty(t, plan.ConsentRequest.ProfileFingerprint)
	require.Equal(t, plan.Plan.ProfileFingerprint, plan.ConsentRequest.ProfileFingerprint)
	_, err := catalog.BeginNotePublicationScan(t.Context(), store.NotePublicationBeginRequest{
		Name: "local-text-plan", Destination: "/notes", PolicyFingerprint: strings.Repeat("a", 64), OperationID: uuid.NewV4().String(),
	})
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodGet, "/api/v1/note-publications/local-text-plan", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var status api.NotePublicationStatus
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	require.Equal(t, "local-text-plan", status.Name)
}
