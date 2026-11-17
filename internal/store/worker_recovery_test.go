package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func checkWorkerFailedTaskRetry(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	versions := seedRenditionCatalogVersions(t, s)
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	at := time.Now().UTC()
	claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker", at, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(t.Context(), claim, waiter.ID, at, renditionJobTestSnapshot(request))
	require.NoError(t, err)
	require.NoError(t, s.CheckpointRenditionProvider(t.Context(), claim, "retained-handle", at))
	require.NoError(t, s.MarkRenditionJobFailed(t.Context(), claim, RenditionFailureProviderTaskFailed, at))
	retried, _, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, job.ID, retried.ID)
	claim, err = s.ClaimRenditionJob(t.Context(), job.ID, "retry", time.Now().UTC().Add(time.Second), time.Minute)
	require.NoError(t, err)
	require.Empty(t, claim.ResumeHandle)
	work, err := s.RenditionJobWorkByClaim(t.Context(), claim, time.Now().UTC().Add(time.Second))
	require.NoError(t, err)
	require.Nil(t, work.ExecutionSnapshot)
	require.Equal(t, RenditionPhaseQueued, work.Job.Phase)
}
