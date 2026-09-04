package docbank

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/version"
)

var (
	// ErrBackupRepositoryLocked means another writer owns the snapshot repository.
	ErrBackupRepositoryLocked = backup.ErrRepoLocked
	// ErrBackupRestoreTargetActive means another vault or restore owns an overlapping root.
	ErrBackupRestoreTargetActive = backupapp.ErrRestoreTargetActive
	// ErrBackupRestoreTargetChanged means the target path stopped naming the held directory.
	ErrBackupRestoreTargetChanged = backupapp.ErrRestoreTargetChanged
	// ErrBackupRestoreTargetNotEmpty means overwrite was required for an existing target payload.
	ErrBackupRestoreTargetNotEmpty = backupapp.ErrRestoreTargetNotEmpty
	// ErrBackupRestoreTargetOverlap means the target or repository overlaps protected vault storage.
	ErrBackupRestoreTargetOverlap = backupapp.ErrRestoreTargetOverlap
)

// BackupRepository is one initialized immutable snapshot repository.
type BackupRepository struct {
	repo *backup.Repo
}

// BackupOptions controls one embedded vault snapshot.
type BackupOptions struct {
	Tag         string
	ZstdLevel   int
	Jobs        int
	ForceUnlock bool
	Progress    func(BackupProgress)
}

// BackupVerifyOptions controls repository verification. SnapshotID selects
// the latest snapshot when empty. All and SnapshotID are mutually exclusive.
type BackupVerifyOptions struct {
	SnapshotID  string
	All         bool
	Quick       bool
	Jobs        int
	ForceUnlock bool
	Progress    func(BackupProgress)
}

// BackupRestoreOptions controls restore into a separate vault root.
type BackupRestoreOptions struct {
	SnapshotID  string
	Target      string
	Overwrite   bool
	Jobs        int
	ForceUnlock bool
	Progress    func(BackupProgress)
}

// BackupProgress reports one structured stage update.
type BackupProgress struct {
	Stage      string
	Done       int64
	Total      int64
	BytesDone  int64
	BytesTotal int64
	Final      bool
}

// BackupSnapshot summarizes one immutable recovery point.
type BackupSnapshot struct {
	ID              string
	ParentID        string
	CreatedAt       string
	Tag             string
	MetadataFormat  string
	Nodes           int64
	Files           int64
	Blobs           int64
	BlobBytes       int64
	PacksAdded      int
	BytesAdded      int64
	DurationSeconds float64
}

// BackupVerifyProblem identifies one repository-integrity finding.
type BackupVerifyProblem struct {
	SnapshotID string
	Detail     string
}

// BackupVerifyReport summarizes a completed repository verification pass.
type BackupVerifyReport struct {
	Snapshots    []string
	BlobsChecked int64
	BytesRead    int64
	Problems     []BackupVerifyProblem
}

// BackupRestoreProof states the checks completed before restore publication.
type BackupRestoreProof struct {
	ContentVerified bool
	SQLiteIntegrity bool
	ManifestStats   bool
}

// BackupRestoreReport summarizes a completely materialized restored vault.
type BackupRestoreReport struct {
	SnapshotID      string
	Target          string
	DatabasePath    string
	DatabaseBytes   int64
	ContentBlobs    int64
	ContentBytes    int64
	PackedBlobs     int64
	LooseBlobs      int64
	Packs           int
	ExtrasFiles     int
	DurationSeconds float64
	Proof           BackupRestoreProof
}

// InitBackupRepository initializes an empty immutable snapshot repository.
func InitBackupRepository(root string) (*BackupRepository, error) {
	canonical, err := home.CanonicalRoot(root)
	if err != nil {
		return nil, fmt.Errorf("initializing backup repository: %w", err)
	}
	repo, err := backup.Init(canonical)
	if err != nil {
		return nil, fmt.Errorf("initializing backup repository: %w", err)
	}
	return &BackupRepository{repo: repo}, nil
}

// OpenBackupRepository opens an existing compatible snapshot repository.
func OpenBackupRepository(root string) (*BackupRepository, error) {
	canonical, err := home.CanonicalRoot(root)
	if err != nil {
		return nil, fmt.Errorf("opening backup repository: %w", err)
	}
	repo, err := backup.Open(canonical)
	if err != nil {
		return nil, fmt.Errorf("opening backup repository: %w", err)
	}
	return &BackupRepository{repo: repo}, nil
}

// ID returns the repository's stable identity.
func (r *BackupRepository) ID() string {
	if r == nil || r.repo == nil {
		return ""
	}
	return r.repo.Config().RepoID
}

// Root returns the repository's canonical filesystem root.
func (r *BackupRepository) Root() string {
	if r == nil || r.repo == nil {
		return ""
	}
	return r.repo.Root()
}

// Snapshots lists immutable recovery points from oldest to newest.
func (r *BackupRepository) Snapshots() ([]BackupSnapshot, error) {
	if r == nil || r.repo == nil {
		return nil, errors.New("docbank backup repository is required")
	}
	manifests, err := r.repo.ListSnapshots()
	if err != nil {
		return nil, fmt.Errorf("listing backup snapshots: %w", err)
	}
	snapshots := make([]BackupSnapshot, 0, len(manifests))
	for _, manifest := range manifests {
		snapshot, err := backupSnapshot(manifest)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

// CreateBackup captures a coherent logical snapshot. The metadata freeze is
// short; ordinary appends resume while physical maintenance remains fenced.
func (v *Vault) CreateBackup(
	ctx context.Context, repository *BackupRepository, opts BackupOptions,
) (BackupSnapshot, error) {
	if err := v.begin(); err != nil {
		return BackupSnapshot{}, err
	}
	defer v.lifecycle.RUnlock()
	if repository == nil || repository.repo == nil {
		return BackupSnapshot{}, errors.New("docbank backup repository is required")
	}
	if err := backupapp.ValidateDisjointRoots(v.root.Name(), repository.repo.Root()); err != nil {
		return BackupSnapshot{}, err
	}
	v.preservation.RLock()
	defer v.preservation.RUnlock()
	manifest, err := backupapp.Create(ctx, repository.repo, version.Version, v.metadata, v.blobs,
		backup.CreateOptions{
			Tag: opts.Tag, ZstdLevel: opts.ZstdLevel, Jobs: opts.Jobs,
			ForceUnlock: opts.ForceUnlock, Progress: backupProgressCallback(opts.Progress),
			Freezer: &vaultBackupFreezer{vault: v},
		})
	if err != nil {
		return BackupSnapshot{}, fmt.Errorf("creating backup snapshot: %w", err)
	}
	return backupSnapshot(manifest)
}

// Verify checks repository structure and, unless Quick is set, every selected
// content blob.
func (r *BackupRepository) Verify(
	ctx context.Context, opts BackupVerifyOptions,
) (BackupVerifyReport, error) {
	if r == nil || r.repo == nil {
		return BackupVerifyReport{}, errors.New("docbank backup repository is required")
	}
	if opts.All && opts.SnapshotID != "" {
		return BackupVerifyReport{}, errors.New("docbank backup snapshot ID and all are mutually exclusive")
	}
	result, err := backup.Verify(ctx, r.repo, backupapp.New(version.Version), backup.VerifyOptions{
		SnapshotID: opts.SnapshotID, All: opts.All, Quick: opts.Quick, Jobs: opts.Jobs,
		ForceUnlock: opts.ForceUnlock, Progress: backupProgressCallback(opts.Progress),
	})
	if err != nil {
		return BackupVerifyReport{}, fmt.Errorf("verifying backup repository: %w", err)
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

// RestoreBackup materializes and proves a snapshot in a root disjoint from
// both the live vault and repository. It never replaces the open vault.
func (v *Vault) RestoreBackup(
	ctx context.Context, repository *BackupRepository, opts BackupRestoreOptions,
) (report BackupRestoreReport, retErr error) {
	if err := v.begin(); err != nil {
		return BackupRestoreReport{}, err
	}
	defer v.lifecycle.RUnlock()
	if repository == nil || repository.repo == nil {
		return BackupRestoreReport{}, errors.New("docbank backup repository is required")
	}
	if opts.Target == "" {
		return BackupRestoreReport{}, errors.New("docbank backup restore target is required")
	}
	target, err := home.CanonicalRoot(opts.Target)
	if err != nil {
		return BackupRestoreReport{}, fmt.Errorf("resolving backup restore target: %w", err)
	}
	if err := backupapp.ValidateDisjointRoots(target, repository.repo.Root(), v.root.Name()); err != nil {
		return BackupRestoreReport{}, err
	}
	coordinator := backupapp.NewRestoreTargetCoordinator(
		target, repository.repo.Root(), v.root.Name(), opts.Overwrite,
	)
	if err := coordinator.Prepare(ctx); err != nil {
		return BackupRestoreReport{}, fmt.Errorf("preparing backup restore target: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, coordinator.ReleasePreparation())
	}()
	result, err := backupapp.RestoreWithDriver(
		ctx, repository.repo, version.Version, v.metadata.SQLiteDriver(), backup.RestoreOptions{
			SnapshotID: opts.SnapshotID, TargetDir: target, Overwrite: true,
			Jobs: opts.Jobs, ForceUnlock: opts.ForceUnlock,
			Progress:          backupProgressCallback(opts.Progress),
			TargetCoordinator: coordinator,
		})
	if err != nil {
		return BackupRestoreReport{}, fmt.Errorf("restoring backup snapshot: %w", err)
	}
	return BackupRestoreReport{
		SnapshotID: result.SnapshotID, Target: target, DatabasePath: result.DBPath,
		DatabaseBytes: result.DBBytes, ContentBlobs: result.AttachmentBlobs,
		ContentBytes: result.AttachmentBytes, PackedBlobs: result.PackedAttachmentBlobs,
		LooseBlobs: result.LooseAttachmentBlobs, Packs: result.AttachmentPacks,
		ExtrasFiles: result.ExtrasFiles, DurationSeconds: result.Duration.Seconds(),
		Proof: BackupRestoreProof{
			ContentVerified: true, SQLiteIntegrity: result.DatabaseIntegrityChecked,
			ManifestStats: true,
		},
	}, nil
}

type vaultBackupFreezer struct {
	vault *Vault
	held  bool
}

func (f *vaultBackupFreezer) Begin(ctx context.Context) error {
	if f.held {
		return errors.New("docbank backup freeze is already held")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.vault.mutation.Lock()
	f.held = true
	return nil
}

func (f *vaultBackupFreezer) End(context.Context) error {
	if !f.held {
		return errors.New("docbank backup freeze is not held")
	}
	f.held = false
	f.vault.mutation.Unlock()
	return nil
}

func backupProgressCallback(callback func(BackupProgress)) func(backup.ProgressEvent) {
	if callback == nil {
		return nil
	}
	return func(event backup.ProgressEvent) {
		callback(BackupProgress{
			Stage: string(event.Stage), Done: event.Done, Total: event.Total,
			BytesDone: event.BytesDone, BytesTotal: event.BytesTotal, Final: event.Final,
		})
	}
}

func backupSnapshot(manifest *backup.Manifest) (BackupSnapshot, error) {
	if manifest == nil {
		return BackupSnapshot{}, errors.New("docbank backup manifest is nil")
	}
	stats, err := backupapp.ParseStats(manifest.Stats)
	if err != nil {
		return BackupSnapshot{}, fmt.Errorf("reading backup snapshot %s: %w", manifest.SnapshotID, err)
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
