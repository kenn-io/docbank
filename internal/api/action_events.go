package api

import (
	"context"
	"net/http"

	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/telemetry"
)

// MailboxImportResult observes confirmed progress from the daemon's mailbox worker.
func MailboxImportResult(ctx context.Context, s *store.Store, gate *OperationGate, imports *telemetry.Imports) func(string) {
	return func(id string) {
		_ = imports.Observe(ctx, "mailbox:"+id, gate.MutateContext, func() (int64, bool, error) {
			current, err := s.MailboxJob(ctx, "vault:"+s.VaultID(), id)
			return current.Imported, current.ScannedTail, err
		})
	}
}

// PackageImportResult observes confirmed progress from the daemon's package worker.
func PackageImportResult(ctx context.Context, s *store.Store, gate *OperationGate, imports *telemetry.Imports) func(store.PackageImportJob) {
	return func(job store.PackageImportJob) {
		_ = imports.Observe(ctx, "package:"+job.ID, gate.MutateContext, func() (int64, bool, error) {
			current, err := ReadPackageImport(ctx, s, job.Owner, job.OperationID)
			return int64(current.Committed), current.State != "queued" && current.State != "running", err
		})
	}
}

func browserActionSurface(r *http.Request) string {
	if browserSessionRequest(r.Context()) {
		return "web"
	}
	return ""
}

// ActionSurfaceHeader lets an interactive CLI attribute the import it starts.
const ActionSurfaceHeader = "X-Docbank-Surface"

// importActionSurface prefers the browser session over a client's CLI claim.
func importActionSurface(r *http.Request) string {
	if surface := browserActionSurface(r); surface != "" || r.Header.Get(ActionSurfaceHeader) != "cli" {
		return surface
	}
	return "cli"
}
