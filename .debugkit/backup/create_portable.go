package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/kit/pack"
)

func createPortable(
	ctx context.Context, r *Repo, app App, opts CreateOptions,
) (*Manifest, error) {
	if err := validatePackExtension(app.PackFileExtension()); err != nil {
		return nil, err
	}
	format := opts.MetadataSource.Format()
	if err := validateMetadataFormat(format); err != nil {
		return nil, err
	}
	start := time.Now()
	pr := newProgressEmitter(opts.Progress)
	if opts.ZstdLevel == 0 {
		opts.ZstdLevel = pack.DefaultZstdLevel
	}
	if opts.Freezer == nil {
		opts.Freezer = NoopFreezeCoordinator{}
	}

	lock, err := r.AcquireExclusiveLock("create", opts.ForceUnlock)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()
	if err := r.CleanStaging(); err != nil {
		return nil, err
	}
	known, err := r.LoadBlobIndex()
	if err != nil {
		return nil, err
	}
	parent, err := r.LatestSnapshot()
	if err != nil {
		return nil, err
	}

	pr.emit(ProgressEvent{Stage: ProgressStageFreeze, Total: 1})
	snapshot, err := openMetadataSnapshot(ctx, opts.MetadataSource, opts.Freezer)
	if err != nil {
		return nil, err
	}
	snapshotOpen := true
	defer func() {
		if snapshotOpen {
			_ = snapshot.Close()
		}
	}()
	pr.emit(ProgressEvent{Stage: ProgressStageFreeze, Done: 1, Total: 1, Final: true})

	statsRaw, err := snapshot.Stats(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup: reading portable metadata stats: %w", err)
	}
	if !json.Valid(statsRaw) {
		return nil, errors.New("backup: portable metadata stats are not valid JSON")
	}
	info, err := snapshot.ContentInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup: reading portable content info: %w", err)
	}
	if info == nil {
		return nil, errors.New("backup: portable metadata snapshot returned nil content info")
	}
	auxiliary, err := auxiliaryArtifacts(ctx, snapshot)
	if err != nil {
		return nil, err
	}

	appender := NewPackAppender(r, known, opts.ZstdLevel, nil, app.PackFileExtension())
	ok := false
	defer func() {
		if !ok {
			appender.Abort()
		}
	}()

	metadataID, metadataBytes, metadataRecipe, err := preparePortableMetadata(
		ctx, snapshot, appender, pr)
	if err != nil {
		return nil, err
	}
	manifestAuxiliary, err := captureAuxiliaryArtifacts(
		ctx, r, auxiliary, opts.ZstdLevel, appender,
	)
	if err != nil {
		return nil, err
	}
	snapshotOpen = false
	if err := snapshot.Close(); err != nil {
		return nil, fmt.Errorf("backup: closing portable metadata snapshot: %w", err)
	}

	capture, lists, extras, err := captureSnapshotFiles(
		ctx, r, app, opts, parent, known, info, appender, pr)
	if err != nil {
		return nil, err
	}
	sealed, err := sealSnapshotCapture(ctx, r, appender, pr)
	if err != nil {
		return nil, err
	}
	ok = true
	createdAt, err := nextCreatedAt(time.Now(), parent)
	if err != nil {
		return nil, err
	}
	manifestVersion := portableMetadataManifestVersion
	if len(manifestAuxiliary) > 0 {
		manifestVersion = auxiliaryManifestVersion
	}
	if metadataRecipe != "" || len(capture.Recipes) > 0 {
		manifestVersion = max(manifestVersion, chunkedObjectManifestVersion)
	}
	if len(extras.Recipes) > 0 {
		manifestVersion = max(manifestVersion, chunkedExtrasManifestVersion)
	}
	m := &Manifest{
		FormatVersion:    manifestVersion,
		MinReaderVersion: manifestVersion,
		AppVersion:       app.Version(),
		CreatedAt:        createdAt.Format(time.RFC3339),
		Options: ManifestOptions{
			IncludeConfig: opts.IncludeConfig,
			IncludeTokens: opts.IncludeTokens,
			ZstdLevel:     opts.ZstdLevel,
			Tag:           opts.Tag,
		},
		Metadata:  &ManifestMetadata{Format: format, Blob: metadataID.String(), Bytes: metadataBytes, Recipe: metadataRecipe},
		Auxiliary: manifestAuxiliary,
		Attachments: ManifestAttachments{
			Layout: []string{"loose"}, Rows: info.Rows, Blobs: capture.Blobs,
			BlobBytes: capture.BlobBytes, Recipes: capture.Recipes, Lists: lists,
		},
		Excluded: app.ExcludedPaths(), Stats: statsRaw,
		NewPacks: sealed.newPacks, NewIndex: sealed.newIndex,
		DurationSeconds: time.Since(start).Seconds(), BytesAdded: sealed.bytesAdded,
	}
	if parent != nil {
		m.ParentID = parent.SnapshotID
	}
	m.Extras = extras
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := r.WriteManifest(m)
	if err != nil {
		return nil, err
	}
	m.SnapshotID = id
	return m, nil
}

func openMetadataSnapshot(
	ctx context.Context, source MetadataSource, freezer FreezeCoordinator,
) (MetadataSnapshot, error) {
	snapshot, err := openWhileFrozen(ctx, freezer,
		func() (MetadataSnapshot, error) { return source.OpenSnapshot(ctx) },
		func(snapshot MetadataSnapshot) error {
			if snapshot == nil {
				return nil
			}
			return snapshot.Close()
		})
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, errors.New("backup: metadata source returned nil snapshot")
	}
	return snapshot, nil
}

func validateMetadataFormat(format string) error {
	if format == "" || len(format) > 128 || strings.IndexFunc(format, unicode.IsControl) >= 0 {
		return fmt.Errorf("backup: invalid portable metadata format %q", format)
	}
	return nil
}
