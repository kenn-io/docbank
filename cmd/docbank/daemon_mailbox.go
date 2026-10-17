package main

import (
	"context"
	"fmt"
	"log/slog"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/mailbox"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/telemetry"
)

func startMailboxJobs(ctx context.Context, supervisor *jobs.Supervisor, s *store.Store, b *blob.Store, spool string, gate *api.OperationGate, logger *slog.Logger, imports *telemetry.Imports) error {
	if err := s.CleanupMailboxContainers(ctx); err != nil {
		return err
	}
	if err := s.ResetMailboxClaims(ctx); err != nil {
		return err
	}
	service := &mailbox.Service{Store: s, Blobs: b, Spool: spool, Mutate: gate.MutateContext, Logger: logger, ImportResult: func(id string) {
		_ = imports.Observe(ctx, "mailbox:"+id, gate.MutateContext, func() (int64, bool, error) {
			current, err := s.MailboxJob(ctx, "vault:"+s.VaultID(), id)
			return current.Imported, current.ScannedTail, err
		})
	}}
	for i := range 2 {
		if err := supervisor.Start(fmt.Sprintf("import:mailbox:%d", i+1), service.RunWorker); err != nil {
			return err
		}
	}
	return nil
}
