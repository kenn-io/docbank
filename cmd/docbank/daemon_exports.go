package main

import (
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func startExportWorker(supervisor *jobs.Supervisor, catalog *store.Store, blobs *blob.Store, root string, gate *api.OperationGate) (*processing.ExportWorker, error) {
	worker, err := processing.NewExportWorker(catalog, blobs, root, gate)
	if err != nil {
		return nil, err
	}
	if err = supervisor.StartWorker("exports:write", worker.Run); err != nil {
		return nil, err
	}
	return worker, nil
}
