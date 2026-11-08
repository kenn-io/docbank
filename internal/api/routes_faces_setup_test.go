package api_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/localface"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
)

func TestFaceSetupRoutesRequireReviewedConsent(t *testing.T) {
	t.Parallel()
	var starts atomic.Int32
	ts, _ := newTestServer(t, func(deps *api.Deps) {
		profile, descriptor, err := config.LocalFaces()
		require.NoError(t, err)
		provider, err := localface.New(descriptor, func(context.Context) (string, error) {
			return "", errors.New("setup must not invoke inference")
		})
		require.NoError(t, err)
		deps.Gate = api.NewOperationGate()
		service, err := processing.NewService(t.Context(), processing.ServiceConfig{
			Catalog: deps.Store, Blobs: deps.Blobs, Gate: deps.Gate, Lifecycle: t.Context(),
			SpoolDirectory: t.TempDir(),
			Profiles: map[string]processing.ProfileConfig{config.LocalFacesProfile: {
				Profile: profile, EmbeddingProviders: map[string]document.EmbeddingProvider{config.LocalFacesProfile: provider},
			}},
		})
		require.NoError(t, err)
		t.Cleanup(service.Stop)
		deps.Processing = service
		deps.Faces = &processing.FaceSetup{Service: service, Startup: func(context.Context) error { starts.Add(1); return nil }}
	})
	response, body := do(t, ts, http.MethodPost, "/api/v1/processing/faces/plans", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var plan api.LocalTextPlan
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	require.Equal(t, config.LocalFacesProfile, plan.Profile)
	for _, request := range []api.FaceSetupApplyRequest{
		{PlanFingerprint: plan.Fingerprint},
		{PlanFingerprint: processingTestHash("stale"), Consent: true},
	} {
		response, body = do(t, ts, http.MethodPost, "/api/v1/processing/faces/setup", nil, request)
		if request.Consent {
			require.Equal(t, http.StatusConflict, response.StatusCode, body)
			require.Contains(t, body, `"code":"processing_plan_changed"`)
		} else {
			require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
			require.Contains(t, body, `"code":"consent_required"`)
			require.Zero(t, starts.Load(), "refusing HTTP consent must precede runtime startup")
		}
	}
	starts.Store(0)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/faces/setup", nil, api.FaceSetupApplyRequest{PlanFingerprint: plan.Fingerprint, Consent: true})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.EqualValues(t, 1, starts.Load())
}
