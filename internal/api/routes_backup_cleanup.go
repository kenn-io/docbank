package api

import (
	"context"
	"errors"
	"io/fs"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/version"
	"go.kenn.io/kit/backup"
)

// BackupForgetReport reports selected snapshots and completed removals.
type BackupForgetReport = backupapp.BackupForgetReport

// BackupPruneReport reports planned and completed pack cleanup.
type BackupPruneReport = backupapp.BackupPruneReport

func registerBackupCleanupRoutes(api huma.API, d Deps) {
	type forgetOutput struct{ Body BackupForgetReport }
	huma.Register(api, huma.Operation{
		OperationID: "forgetBackupSnapshots", Method: http.MethodPost, Path: "/api/v1/backup/forget",
		Summary: "Remove selected backup recovery points",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Repo        string   `json:"repo,omitzero"`
			SnapshotIDs []string `json:"snapshot_ids" minItems:"1"`
			DryRun      bool     `json:"dry_run,omitzero"`
			AllowEmpty  bool     `json:"allow_empty,omitzero"`
			ForceUnlock bool     `json:"force_unlock,omitzero"`
		}
	}) (*forgetOutput, error) {
		repo, err := openBackupRepository(ctx, d, in.Body.Repo)
		if err != nil {
			return nil, err
		}
		result, err := backup.Forget(ctx, repo, backup.ForgetOptions{SnapshotIDs: in.Body.SnapshotIDs, DryRun: in.Body.DryRun, AllowEmpty: in.Body.AllowEmpty, ForceUnlock: in.Body.ForceUnlock})
		report := backupapp.ForgetReport(result)
		if err != nil {
			problem := backupProblem(err)
			// Kit wraps the sentinel directly for an unknown selection. Filesystem
			// failures carry a PathError and remain server errors.
			if errors.Is(err, fs.ErrNotExist) {
				if _, ioFailure := errors.AsType[*fs.PathError](err); !ioFailure {
					problem = NewError(http.StatusNotFound, "backup_snapshot_not_found", err.Error())
				}
			}
			if report.HasWork() {
				problem.BackupForget = &report
			}
			return nil, problem
		}
		return &forgetOutput{Body: report}, nil
	})
	type pruneOutput struct{ Body BackupPruneReport }
	huma.Register(api, huma.Operation{
		OperationID: "pruneBackupRepository", Method: http.MethodPost, Path: "/api/v1/backup/prune",
		Summary: "Reclaim unused backup storage",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Repo        string `json:"repo,omitzero"`
			DryRun      bool   `json:"dry_run,omitzero"`
			ForceUnlock bool   `json:"force_unlock,omitzero"`
		}
	}) (*pruneOutput, error) {
		repo, err := openBackupRepository(ctx, d, in.Body.Repo)
		if err != nil {
			return nil, err
		}
		result, err := backup.Prune(ctx, repo, backupapp.New(version.Version), backup.PruneOptions{DryRun: in.Body.DryRun, ForceUnlock: in.Body.ForceUnlock})
		report := backupapp.PruneReport(result)
		if err != nil {
			problem := backupProblem(err)
			if report.HasWork() {
				problem.BackupPrune = &report
			}
			return nil, problem
		}
		return &pruneOutput{Body: report}, nil
	})
}
