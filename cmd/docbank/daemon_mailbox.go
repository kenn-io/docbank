package main

import (
	"context"
	"fmt"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/mailbox"
	"go.kenn.io/docbank/internal/store"
)

func startMailboxJobs(ctx context.Context, supervisor *jobs.Supervisor, s *store.Store, b *blob.Store, spool string, gate *api.OperationGate) error {
	if err := s.CleanupMailboxContainers(ctx); err != nil {
		return err
	}
	if err := s.ResetMailboxClaims(ctx); err != nil {
		return err
	}
	service := &mailbox.Service{Store: s, Blobs: b, Spool: spool, Mutate: gate.MutateContext}
	for i := range 2 {
		if err := supervisor.Start(fmt.Sprintf("import:mailbox:%d", i+1), service.RunWorker); err != nil {
			return err
		}
	}
	return nil
}
