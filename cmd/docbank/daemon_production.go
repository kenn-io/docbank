package main

import (
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

// startProductionWorker renders queued jobs under the daemon's vault supervisor.
func startProductionWorker(supervisor *jobs.Supervisor, catalog *store.Store, blobs *blob.Store) error {
	worker := &production.Worker{Store: catalog,
		Source:    processing.ProductionSourceOpener{Catalog: catalog, Blobs: blobs},
		Pages:     processing.ProductionPageStageAdapter{Catalog: catalog, Blobs: blobs},
		Artifacts: processing.ProductionFinalArtifactAdapter{Catalog: catalog, Blobs: blobs},
		WorkerID:  "production-daemon"}
	return supervisor.Start("production:render", worker.Run)
}
