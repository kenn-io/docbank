package processing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/kit/embedclient"
)

func TestFaceGenericPlanDisclosesDiscoveryScope(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	selector := Selector{NodeID: x.f.node.ID, ContentVersionID: x.f.node.CurrentVersionID, Profile: config.LocalFacesProfile}
	plan, err := x.service.Plan(t.Context(), selector)
	require.NoError(t, err)
	require.Contains(t, plan.Estimate.Scope, "throughout this vault")
	require.Contains(t, plan.Estimate.Scope, "Hidden")
	setup, err := (&FaceSetup{Service: x.service}).Plan(t.Context())
	require.NoError(t, err)
	require.Equal(t, plan.Estimate.Scope, setup.Scope)
}

func TestFaceAggregateStatusIncludesLiveJobs(t *testing.T) {
	t.Parallel()
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "failed"}[invalid], func(t *testing.T) {
			x := newFaceEmbeddingFixture(t, false)
			if invalid {
				x.vectorForBox = func(embedclient.FaceBox) []float32 { return []float32{1} }
			}
			selector := Selector{NodeID: x.f.node.ID, ContentVersionID: x.f.node.CurrentVersionID, Profile: config.LocalFacesProfile}
			plan, err := x.service.Plan(t.Context(), selector)
			require.NoError(t, err)
			var queued, running Status
			var queuedErr, runningErr error
			var id string
			x.onRequest = func() { running, runningErr = x.service.Status(t.Context(), id) }
			job, err := x.service.StartWithProgress(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, func(job Job) {
				id = job.ID
				queued, queuedErr = x.service.Status(t.Context(), id)
			})
			require.NoError(t, err)
			require.NotEmpty(t, id)
			require.NoError(t, queuedErr)
			require.NoError(t, runningErr)
			assert.Equal(t, "queued", queued.State)
			assert.Equal(t, []string{id}, queued.EmbeddingJobIDs)
			assert.Equal(t, "running", running.State)
			assert.Equal(t, []string{id}, running.EmbeddingJobIDs)
			after, err := x.service.Status(t.Context(), job.ID)
			require.NoError(t, err)
			if invalid {
				assert.Equal(t, "partial", after.State)
				assert.NotEmpty(t, after.FailureCode)
			} else {
				assert.Equal(t, "completed", after.State)
				assert.Equal(t, 1, after.CompletedBindings)
			}
		})
	}
}
