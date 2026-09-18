package main

import (
	"context"
	"log/slog"

	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func startPageRuntime(ctx context.Context, cfg config.Config, supervisor *jobs.Supervisor, catalog *store.Store, blobs *blob.Store, gate *api.OperationGate, logger *slog.Logger) (*pagerender.Runtime, error) {
	if cfg.PageRuntime == nil {
		return nil, nil //nolint:nilnil // An absent optional capability is a valid daemon configuration.
	}
	engine, err := pagerender.New(ctx, *cfg.PageRuntime)
	if err != nil {
		logger.Warn("optional page runtime unavailable", "error", err)
		return nil, nil //nolint:nilnil // Failed optional qualification must preserve ordinary daemon operation.
	}
	worker, err := processing.NewPageWorker(catalog, blobs, engine, gate)
	if err != nil {
		return nil, err
	}
	if err := supervisor.Start("pages:render", worker.Run); err != nil {
		return nil, err
	}
	return engine, nil
}
