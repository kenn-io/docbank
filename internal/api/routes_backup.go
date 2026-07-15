package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/version"
	docsqlite "go.kenn.io/docbank/sqlite"
)

type backupCreateRequest struct {
	Repo        string `json:"repo,omitempty"`
	Tag         string `json:"tag,omitempty" maxLength:"256"`
	Jobs        int    `json:"jobs,omitempty" minimum:"0"`
	ForceUnlock bool   `json:"force_unlock,omitempty"`
}

type backupVerifyRequest struct {
	Repo        string `json:"repo,omitempty"`
	SnapshotID  string `json:"snapshot_id,omitempty"`
	All         bool   `json:"all,omitempty"`
	Quick       bool   `json:"quick,omitempty"`
	Jobs        int    `json:"jobs,omitempty" minimum:"0"`
	ForceUnlock bool   `json:"force_unlock,omitempty"`
}

type backupRestoreRequest struct {
	Repo        string `json:"repo,omitempty"`
	Target      string `json:"target"`
	SnapshotID  string `json:"snapshot_id,omitempty"`
	Overwrite   bool   `json:"overwrite,omitempty"`
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
			stream := newEventStreamWriter[BackupCreateEvent](hctx.BodyWriter(), cancel)
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
		Repo string `query:"repo"`
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

	type verifyOutput struct{ Body BackupVerifyReport }
	huma.Register(api, huma.Operation{
		OperationID: "verifyBackupRepository", Method: http.MethodPost, Path: "/api/v1/backup/verify",
		Summary: "Verify backup repository integrity",
	}, func(ctx context.Context, in *struct {
		Body backupVerifyRequest
	}) (*verifyOutput, error) {
		repo, err := openBackupRepository(d, in.Body.Repo)
		if err != nil {
			return nil, err
		}
		report, err := verifyBackupRepository(ctx, repo, in.Body, nil)
		if err != nil {
			return nil, err
		}
		return &verifyOutput{Body: report}, nil
	})

	verifyStreamSchema := api.OpenAPI().Components.Schemas.Schema(
		reflect.TypeFor[BackupVerifyEvent](), true, "BackupVerifyEvent")
	huma.Register(api, huma.Operation{
		OperationID: "streamBackupRepositoryVerification", Method: http.MethodPost,
		Path:    "/api/v1/backup/verify/stream",
		Summary: "Verify a backup repository and stream structured progress",
		Description: "Returns newline-delimited JSON. Progress events precede exactly one terminal " +
			"result or error event; an HTTP 200 only means the stream started.",
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Backup verification progress followed by one terminal result or error",
				Content: map[string]*huma.MediaType{
					"application/x-ndjson": {Schema: verifyStreamSchema},
				},
			},
		},
	}, func(_ context.Context, in *struct {
		Body backupVerifyRequest
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
			stream := newEventStreamWriter[BackupVerifyEvent](hctx.BodyWriter(), cancel)
			report, verifyErr := verifyBackupRepository(runCtx, repo, in.Body,
				func(event backup.ProgressEvent) {
					stream.send(BackupVerifyEvent{
						Type: "progress", Progress: backupProgress(event),
					})
				})
			if stream.err() != nil {
				return
			}
			if verifyErr != nil {
				stream.send(BackupVerifyEvent{Type: "error", Error: backupProblem(verifyErr)})
				return
			}
			stream.send(BackupVerifyEvent{Type: "result", Report: &report})
		}}, nil
	})

	type restoreOutput struct{ Body BackupRestoreReport }
	huma.Register(api, huma.Operation{
		OperationID: "restoreBackupSnapshot", Method: http.MethodPost, Path: "/api/v1/backup/restore",
		Summary: "Restore and prove a snapshot in a separate vault directory",
	}, func(ctx context.Context, in *struct {
		Body backupRestoreRequest
	}) (*restoreOutput, error) {
		repo, target, err := prepareBackupRestore(d, in.Body)
		if err != nil {
			return nil, err
		}
		coordinator := newRestoreTargetCoordinator(
			target, repo.Root(), d.VaultRoot, in.Body.Overwrite)
		report, err := restoreBackupSnapshot(
			ctx, repo, target, in.Body, coordinator, nil, d.Store.SQLiteDriver())
		if err != nil {
			return nil, err
		}
		return &restoreOutput{Body: report}, nil
	})

	restoreStreamSchema := api.OpenAPI().Components.Schemas.Schema(
		reflect.TypeFor[BackupRestoreEvent](), true, "BackupRestoreEvent")
	huma.Register(api, huma.Operation{
		OperationID: "streamBackupSnapshotRestore", Method: http.MethodPost,
		Path:    "/api/v1/backup/restore/stream",
		Summary: "Restore and prove a snapshot while streaming structured progress",
		Description: "Returns newline-delimited JSON. Progress events precede exactly one terminal " +
			"result or error event; an HTTP 200 only means the stream started.",
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Backup restore progress followed by one terminal result or error",
				Content: map[string]*huma.MediaType{
					"application/x-ndjson": {Schema: restoreStreamSchema},
				},
			},
		},
	}, func(_ context.Context, in *struct {
		Body backupRestoreRequest
	}) (*huma.StreamResponse, error) {
		repo, target, err := prepareBackupRestore(d, in.Body)
		if err != nil {
			return nil, err
		}
		coordinator := newRestoreTargetCoordinator(
			target, repo.Root(), d.VaultRoot, in.Body.Overwrite)
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			hctx.SetHeader("Content-Type", "application/x-ndjson")
			hctx.SetHeader("Cache-Control", "no-store")
			runCtx, cancel := context.WithCancel(hctx.Context())
			defer cancel()
			stream := newEventStreamWriter[BackupRestoreEvent](hctx.BodyWriter(), cancel)
			report, restoreErr := restoreBackupSnapshot(
				runCtx, repo, target, in.Body, coordinator,
				func(event backup.ProgressEvent) {
					stream.send(BackupRestoreEvent{
						Type: "progress", Progress: backupProgress(event),
					})
				}, d.Store.SQLiteDriver())
			if stream.err() != nil {
				return
			}
			if restoreErr != nil {
				stream.send(BackupRestoreEvent{Type: "error", Error: backupProblem(restoreErr)})
				return
			}
			stream.send(BackupRestoreEvent{Type: "result", Report: &report})
		}}, nil
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

func verifyBackupRepository(
	ctx context.Context,
	repo *backup.Repo,
	in backupVerifyRequest,
	progress func(backup.ProgressEvent),
) (BackupVerifyReport, error) {
	if in.All && in.SnapshotID != "" {
		return BackupVerifyReport{}, NewError(http.StatusUnprocessableEntity, "validation",
			"snapshot_id and all are mutually exclusive")
	}
	result, err := backup.Verify(ctx, repo, backupapp.New(version.Version), backup.VerifyOptions{
		SnapshotID: in.SnapshotID, All: in.All, Quick: in.Quick, Jobs: in.Jobs,
		ForceUnlock: in.ForceUnlock, Progress: progress,
	})
	if err != nil {
		return BackupVerifyReport{}, fromBackupError(err)
	}
	report := BackupVerifyReport{
		Snapshots: result.Snapshots, BlobsChecked: result.BlobsChecked,
		BytesRead: result.BytesRead,
		Problems:  make([]BackupVerifyProblem, 0, len(result.Problems)),
	}
	for _, problem := range result.Problems {
		report.Problems = append(report.Problems, BackupVerifyProblem{
			SnapshotID: problem.SnapshotID, Detail: problem.Detail,
		})
	}
	return report, nil
}

func prepareBackupRestore(
	d Deps, in backupRestoreRequest,
) (*backup.Repo, string, error) {
	repo, err := openBackupRepository(d, in.Repo)
	if err != nil {
		return nil, "", err
	}
	if in.Target == "" || !filepath.IsAbs(in.Target) {
		return nil, "", NewError(http.StatusUnprocessableEntity, "validation",
			"backup restore target must be an absolute server path")
	}
	target, err := canonicalServerPath(in.Target)
	if err != nil {
		return nil, "", NewError(http.StatusUnprocessableEntity, "validation",
			fmt.Sprintf("resolving backup restore target %q: %v", in.Target, err))
	}
	repoRoot, err := canonicalServerPath(repo.Root())
	if err != nil {
		return nil, "", NewError(http.StatusUnprocessableEntity, "backup_repository",
			fmt.Sprintf("resolving backup repository: %v", err))
	}
	overlaps, overlapErr := pathsOverlap(target, repoRoot)
	if overlapErr != nil {
		return nil, "", NewError(http.StatusUnprocessableEntity, "validation",
			fmt.Sprintf("checking backup repository overlap: %v", overlapErr))
	}
	if overlaps {
		return nil, "", NewError(http.StatusUnprocessableEntity, "validation",
			"backup restore target must be disjoint from the backup repository")
	}
	if d.VaultRoot != "" {
		vaultRoot, resolveErr := canonicalServerPath(d.VaultRoot)
		if resolveErr != nil {
			return nil, "", NewError(http.StatusInternalServerError, "backup_failed",
				fmt.Sprintf("resolving live vault root: %v", resolveErr))
		}
		overlaps, overlapErr = pathsOverlap(target, vaultRoot)
		if overlapErr != nil {
			return nil, "", NewError(http.StatusInternalServerError, "backup_failed",
				fmt.Sprintf("checking live vault overlap: %v", overlapErr))
		}
		if overlaps {
			return nil, "", NewError(http.StatusUnprocessableEntity, "validation",
				"backup restore target must be disjoint from the running vault")
		}
	}
	entries, readErr := os.ReadDir(target)
	if readErr == nil && restoreTargetHasPayload(entries) && !in.Overwrite {
		return nil, "", NewError(http.StatusConflict, "backup_restore_target_not_empty",
			"backup restore target is not empty; set overwrite to merge into it")
	}
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return nil, "", NewError(http.StatusUnprocessableEntity, "validation",
			fmt.Sprintf("reading backup restore target: %v", readErr))
	}
	return repo, target, nil
}

func restoreBackupSnapshot(
	ctx context.Context,
	repo *backup.Repo,
	target string,
	in backupRestoreRequest,
	coordinator restoreTargetCoordinator,
	progress func(backup.ProgressEvent),
	driver docsqlite.Driver,
) (BackupRestoreReport, error) {
	return restoreBackupSnapshotWith(
		ctx, repo, target, in, coordinator, progress, driver, backupapp.Restore)
}

type backupRestoreRunner func(
	context.Context, *backup.Repo, string, backup.RestoreOptions,
) (*backup.RestoreResult, error)

func restoreBackupSnapshotWith(
	ctx context.Context,
	repo *backup.Repo,
	target string,
	in backupRestoreRequest,
	coordinator restoreTargetCoordinator,
	progress func(backup.ProgressEvent),
	driver docsqlite.Driver,
	run backupRestoreRunner,
) (report BackupRestoreReport, retErr error) {
	if err := coordinator.Prepare(ctx); err != nil {
		return report, err
	}
	defer func() {
		if err := coordinator.ReleasePreparation(); err != nil {
			retErr = errors.Join(retErr, NewError(http.StatusInternalServerError, "backup_failed",
				fmt.Sprintf("releasing backup restore target preparation: %v", err)))
		}
	}()
	result, err := run(ctx, repo, version.Version, backup.RestoreOptions{
		SnapshotID: in.SnapshotID, TargetDir: target, Overwrite: true,
		Jobs: in.Jobs, ForceUnlock: in.ForceUnlock, Progress: progress,
		TargetCoordinator: coordinator,
		SQLiteOpener:      backupapp.SQLiteOpener(driver),
	})
	if err != nil {
		return report, fromBackupError(err)
	}
	return backupRestoreReport(target, result), nil
}

func restoreTargetHasPayload(entries []os.DirEntry) bool {
	for _, entry := range entries {
		if entry.Name() != "vault.lock" {
			return true
		}
	}
	return false
}

func backupRestoreReport(target string, result *backup.RestoreResult) BackupRestoreReport {
	fallbackCounts := make(map[string]int)
	for _, fallback := range result.PackFallbacks {
		fallbackCounts[string(fallback.Reason)]++
	}
	reasons := make([]string, 0, len(fallbackCounts))
	for reason := range fallbackCounts {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	fallbacks := make([]BackupRestoreFallback, 0, len(reasons))
	for _, reason := range reasons {
		fallbacks = append(fallbacks, BackupRestoreFallback{
			Reason: reason, Count: fallbackCounts[reason],
		})
	}
	return BackupRestoreReport{
		SnapshotID: result.SnapshotID, Target: target, DatabasePath: result.DBPath,
		DatabaseBytes: result.DBBytes, DocumentBlobs: result.AttachmentBlobs,
		DocumentBytes: result.AttachmentBytes, PackedBlobs: result.PackedAttachmentBlobs,
		LooseBlobs: result.LooseAttachmentBlobs, Packs: result.AttachmentPacks,
		Fallbacks: fallbacks, ExtrasFiles: result.ExtrasFiles,
		DurationSeconds: result.Duration.Seconds(),
		Proof: BackupRestoreProof{
			ContentVerified: true,
			SQLiteIntegrity: result.DatabaseIntegrityChecked,
			ManifestStats:   true,
		},
	}
}

// canonicalServerPath resolves symlinks in the existing prefix while retaining
// a not-yet-created suffix. This makes overlap checks meaningful for a new
// restore target nested beneath a symlinked repository or vault path.
func canonicalServerPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(abs)
	var missing []string
	for {
		resolved, evalErr := filepath.EvalSymlinks(current)
		if evalErr == nil {
			for _, v := range slices.Backward(missing) {
				resolved = filepath.Join(resolved, v)
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(evalErr, fs.ErrNotExist) {
			return "", evalErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", evalErr
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func pathsOverlap(a, b string) (bool, error) {
	if pathContains(a, b) || pathContains(b, a) {
		return true, nil
	}
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		overlaps, err := existingAncestorMatches(pair[0], pair[1])
		if err != nil {
			return false, err
		}
		if overlaps {
			return true, nil
		}
	}
	return false, nil
}

// existingAncestorMatches supplements filepath.Rel with filesystem identity.
// This catches case- and normalization-equivalent spellings on filesystems
// where distinct lexical paths name the same directory.
func existingAncestorMatches(path, protected string) (bool, error) {
	protectedInfo, err := os.Stat(protected)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking protected path %q: %w", protected, err)
	}
	current := path
	for {
		info, statErr := os.Stat(current)
		if statErr == nil && os.SameFile(info, protectedInfo) {
			return true, nil
		}
		if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			return false, fmt.Errorf("checking path ancestor %q: %w", current, statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil
		}
		current = parent
	}
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func backupProgress(event backup.ProgressEvent) *BackupProgress {
	return &BackupProgress{
		Stage: string(event.Stage), Done: event.Done, Total: event.Total,
		BytesDone: event.BytesDone, BytesTotal: event.BytesTotal, Final: event.Final,
	}
}

type eventStreamWriter[T any] struct {
	mu       sync.Mutex
	encoder  *json.Encoder
	flusher  http.Flusher
	cancel   context.CancelFunc
	writeErr error
}

func newEventStreamWriter[T any](w io.Writer, cancel context.CancelFunc) *eventStreamWriter[T] {
	return &eventStreamWriter[T]{
		encoder: json.NewEncoder(w), flusher: responseFlusher(w), cancel: cancel,
	}
}

func (w *eventStreamWriter[T]) send(event T) {
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

func (w *eventStreamWriter[T]) err() error {
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
