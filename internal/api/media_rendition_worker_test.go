package api_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

// renditionTestWorker holds the rendition worker of a media test service. The
// test runs queued jobs itself and then reads the finished status, instead of
// polling the API while a background claim loop sleeps between jobs.
type renditionTestWorker struct{ worker *processing.RenditionWorker }

// bind builds the worker for one test server. The constructor requires an
// idle delay, but the delay is never used because the claim loop never runs.
func (w *renditionTestWorker) bind(t *testing.T, deps *api.Deps, service *processing.Service,
	gate *api.OperationGate, owner string,
) {
	t.Helper()
	worker, err := processing.NewRenditionWorker(processing.RenditionWorkerConfig{
		Catalog: deps.Store, Blobs: deps.Blobs, Runtime: service.RenditionRuntimes(), Gate: gate,
		Owner: owner, LeaseDuration: time.Minute, IdleDelay: time.Minute,
	})
	require.NoError(t, err)
	w.worker = worker
}

// runQueued processes every ready rendition job and requires at least one.
func (w *renditionTestWorker) runQueued(t *testing.T) {
	t.Helper()
	require.NotNil(t, w.worker, "the media test service was configured without a rendition worker")
	processed := 0
	for {
		ran, err := w.worker.RunOne(t.Context())
		require.NoError(t, err)
		if !ran {
			break
		}
		processed++
	}
	require.NotZero(t, processed, "no rendition job was ready")
}
