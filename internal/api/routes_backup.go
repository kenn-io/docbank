package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/version"
)

func registerBackupRoutes(api huma.API, d Deps, g *gate) {
	type initOutput struct{ Body BackupRepository }
	huma.Register(api, huma.Operation{
		OperationID: "initBackupRepository", Method: http.MethodPost, Path: "/api/v1/backup/init",
		Summary: "Initialize an immutable backup repository",
	}, func(_ context.Context, in *struct {
		Body struct {
			Repo string `json:"repo,omitempty"`
		}
	}) (*initOutput, error) {
		repoPath, err := backupRepoPath(d, in.Body.Repo)
		if err != nil {
			return nil, err
		}
		repo, err := backup.Init(repoPath)
		if err != nil {
			return nil, NewError(http.StatusConflict, "backup_repository", err.Error())
		}
		return &initOutput{Body: BackupRepository{ID: repo.Config().RepoID, Path: repo.Root()}}, nil
	})

	type createOutput struct{ Body BackupSnapshot }
	huma.Register(api, huma.Operation{
		OperationID: "createBackupSnapshot", Method: http.MethodPost, Path: "/api/v1/backup/snapshots",
		Summary: "Capture a verified logical snapshot of the live vault",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Repo        string `json:"repo,omitempty"`
			Tag         string `json:"tag,omitempty" maxLength:"256"`
			Jobs        int    `json:"jobs,omitempty" minimum:"0"`
			ForceUnlock bool   `json:"force_unlock,omitempty"`
		}
	}) (*createOutput, error) {
		repoPath, err := backupRepoPath(d, in.Body.Repo)
		if err != nil {
			return nil, err
		}
		repo, err := backup.Open(repoPath)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "backup_repository", err.Error())
		}
		manifest, err := backupapp.Create(ctx, repo, version.Version, d.Store, d.Blobs,
			backup.CreateOptions{
				Tag: in.Body.Tag, ZstdLevel: d.Cfg.Backup.ZstdLevel,
				Freezer: &gateFreezer{gate: g}, ForceUnlock: in.Body.ForceUnlock, Jobs: in.Body.Jobs,
			})
		if err != nil {
			return nil, fromBackupError(err)
		}
		snapshot, err := backupSnapshot(manifest)
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "backup_manifest", err.Error())
		}
		return &createOutput{Body: snapshot}, nil
	})

	type listOutput struct{ Body BackupSnapshotList }
	huma.Register(api, huma.Operation{
		OperationID: "listBackupSnapshots", Method: http.MethodGet, Path: "/api/v1/backup/snapshots",
		Summary: "List snapshots in a backup repository",
	}, func(_ context.Context, in *struct {
		Repo string `query:"repo,omitempty"`
	}) (*listOutput, error) {
		repoPath, err := backupRepoPath(d, in.Repo)
		if err != nil {
			return nil, err
		}
		repo, err := backup.Open(repoPath)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "backup_repository", err.Error())
		}
		manifests, err := repo.ListSnapshots()
		if err != nil {
			return nil, fromBackupError(err)
		}
		out := &listOutput{Body: BackupSnapshotList{Items: make([]BackupSnapshot, 0, len(manifests))}}
		for _, manifest := range manifests {
			snapshot, err := backupSnapshot(manifest)
			if err != nil {
				return nil, NewError(http.StatusInternalServerError, "backup_manifest", err.Error())
			}
			out.Body.Items = append(out.Body.Items, snapshot)
		}
		return out, nil
	})
}

func backupRepoPath(d Deps, requested string) (string, error) {
	repo := requested
	if repo == "" {
		repo = d.Cfg.Backup.Repo
	}
	if repo == "" {
		return "", NewError(http.StatusUnprocessableEntity, "backup_repository",
			"no backup repository configured; pass repo or set [backup] repo in config.toml")
	}
	if !filepath.IsAbs(repo) {
		return "", NewError(http.StatusUnprocessableEntity, "validation",
			fmt.Sprintf("backup repository %q must be an absolute server path", repo))
	}
	return filepath.Clean(repo), nil
}

func backupSnapshot(manifest *backup.Manifest) (BackupSnapshot, error) {
	if manifest == nil {
		return BackupSnapshot{}, errors.New("backup manifest is nil")
	}
	stats, err := backupapp.ParseStats(manifest.Stats)
	if err != nil {
		return BackupSnapshot{}, fmt.Errorf("snapshot %s: %w", manifest.SnapshotID, err)
	}
	metadataFormat := "sqlite-page-map"
	if manifest.Metadata != nil {
		metadataFormat = manifest.Metadata.Format
	}
	return BackupSnapshot{
		ID: manifest.SnapshotID, ParentID: manifest.ParentID, CreatedAt: manifest.CreatedAt,
		Tag: manifest.Options.Tag, MetadataFormat: metadataFormat,
		Nodes: stats.Nodes, Files: stats.Files, Blobs: stats.Blobs, BlobBytes: stats.BlobBytes,
		PacksAdded: len(manifest.NewPacks), BytesAdded: manifest.BytesAdded,
		DurationSeconds: manifest.DurationSeconds,
	}, nil
}

func fromBackupError(err error) error {
	if errors.Is(err, backup.ErrRepoLocked) {
		return NewError(http.StatusConflict, "backup_locked", err.Error())
	}
	return NewError(http.StatusInternalServerError, "backup_failed", err.Error())
}
