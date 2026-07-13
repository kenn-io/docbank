package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/version"
)

type backupCreateRequest struct {
	Repo        string `json:"repo,omitempty"`
	Tag         string `json:"tag,omitempty" maxLength:"256"`
	Jobs        int    `json:"jobs,omitempty" minimum:"0"`
	ForceUnlock bool   `json:"force_unlock,omitempty"`
}

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
		Body backupCreateRequest
	}) (*createOutput, error) {
		repo, err := openBackupRepository(d, in.Body.Repo)
		if err != nil {
			return nil, err
		}
		snapshot, err := createBackupSnapshot(ctx, repo, d, g, in.Body, nil)
		if err != nil {
			return nil, err
		}
		return &createOutput{Body: snapshot}, nil
	})

	streamSchema := api.OpenAPI().Components.Schemas.Schema(
		reflect.TypeFor[BackupCreateEvent](), true, "BackupCreateEvent")
	huma.Register(api, huma.Operation{
		OperationID: "streamBackupSnapshotCreation", Method: http.MethodPost,
		Path:    "/api/v1/backup/snapshots/stream",
		Summary: "Capture a snapshot and stream structured progress",
		Description: "Returns newline-delimited JSON. Progress events precede exactly one terminal " +
			"result or error event; an HTTP 200 only means the stream started.",
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Backup progress followed by one terminal result or error",
				Content: map[string]*huma.MediaType{
					"application/x-ndjson": {Schema: streamSchema},
				},
			},
		},
	}, func(_ context.Context, in *struct {
		Body backupCreateRequest
	}) (*huma.StreamResponse, error) {
		repo, err := openBackupRepository(d, in.Body.Repo)
		if err != nil {
			return nil, err
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			hctx.SetHeader("Content-Type", "application/x-ndjson")
			hctx.SetHeader("Cache-Control", "no-store")
			runCtx, cancel := context.WithCancel(hctx.Context())
			defer cancel()
			stream := newBackupEventWriter(hctx.BodyWriter(), cancel)
			snapshot, createErr := createBackupSnapshot(
				runCtx, repo, d, g, in.Body,
				func(event backup.ProgressEvent) {
					stream.send(BackupCreateEvent{
						Type: "progress", Progress: backupProgress(event),
					})
				})
			if stream.err() != nil {
				return
			}
			if createErr != nil {
				stream.send(BackupCreateEvent{Type: "error", Error: backupProblem(createErr)})
				return
			}
			stream.send(BackupCreateEvent{Type: "result", Snapshot: &snapshot})
		}}, nil
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

func openBackupRepository(d Deps, requested string) (*backup.Repo, error) {
	repoPath, err := backupRepoPath(d, requested)
	if err != nil {
		return nil, err
	}
	repo, err := backup.Open(repoPath)
	if err != nil {
		return nil, NewError(http.StatusUnprocessableEntity, "backup_repository", err.Error())
	}
	return repo, nil
}

func createBackupSnapshot(
	ctx context.Context,
	repo *backup.Repo,
	d Deps,
	g *gate,
	in backupCreateRequest,
	progress func(backup.ProgressEvent),
) (BackupSnapshot, error) {
	var manifest *backup.Manifest
	err := g.capture(func() error {
		var err error
		manifest, err = backupapp.Create(ctx, repo, version.Version, d.Store, d.Blobs,
			backup.CreateOptions{
				Tag: in.Tag, ZstdLevel: d.Cfg.Backup.ZstdLevel,
				Freezer: &gateFreezer{gate: g}, ForceUnlock: in.ForceUnlock, Jobs: in.Jobs,
				Progress: progress,
			})
		return err
	})
	if err != nil {
		return BackupSnapshot{}, fromBackupError(err)
	}
	snapshot, err := backupSnapshot(manifest)
	if err != nil {
		return BackupSnapshot{}, NewError(http.StatusInternalServerError, "backup_manifest", err.Error())
	}
	return snapshot, nil
}

func backupProgress(event backup.ProgressEvent) *BackupProgress {
	return &BackupProgress{
		Stage: string(event.Stage), Done: event.Done, Total: event.Total,
		BytesDone: event.BytesDone, BytesTotal: event.BytesTotal, Final: event.Final,
	}
}

type backupEventWriter struct {
	mu       sync.Mutex
	encoder  *json.Encoder
	flusher  http.Flusher
	cancel   context.CancelFunc
	writeErr error
}

func newBackupEventWriter(w io.Writer, cancel context.CancelFunc) *backupEventWriter {
	return &backupEventWriter{
		encoder: json.NewEncoder(w), flusher: responseFlusher(w), cancel: cancel,
	}
}

func (w *backupEventWriter) send(event BackupCreateEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr != nil {
		return
	}
	if err := w.encoder.Encode(event); err != nil {
		w.writeErr = err
		w.cancel()
		return
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

func (w *backupEventWriter) err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeErr
}

func responseFlusher(w io.Writer) http.Flusher {
	for {
		if flusher, ok := w.(http.Flusher); ok {
			return flusher
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = unwrapper.Unwrap()
	}
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
	return backupProblem(err)
}

func backupProblem(err error) *Error {
	var problem *Error
	if errors.As(err, &problem) {
		return problem
	}
	if errors.Is(err, backup.ErrRepoLocked) {
		return NewError(http.StatusConflict, "backup_locked", err.Error())
	}
	return NewError(http.StatusInternalServerError, "backup_failed", err.Error())
}
