package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"go.kenn.io/kit/pack"
)

// PruneOptions controls physical cleanup, not snapshot retention. Use Forget
// first to select recovery points for removal. DryRun still takes the exclusive
// repository lock so its reference and space accounting use one repository view.
type PruneOptions struct {
	DryRun      bool
	ForceUnlock bool
	Progress    func(ProgressEvent)
}

// PruneResult counts pack files only, excluding indexes and temporary scratch.
// Planned bytes are not net savings: LiveBytesToRewrite is the current encoded
// payload size, and replacement pack sizes depend on compression and framing.
// Actual counts can be partial on error; a directory-sync error means the last
// removal may not be durable. No byte counts imply secure erasure.
type PruneResult struct {
	PacksToRemove      []string // includes old packs selected for rewriting
	PacksToRepack      []string
	BytesToRemove      int64
	LiveBytesToRewrite uint64
	RemovedPacks       []string
	BytesRemoved       int64
	BytesWritten       int64
}

// Prune reclaims unreferenced storage under the exclusive repository lock.
// It deletes wholly unused packs and rewrites packs with less than half their
// encoded payload still needed. Mostly-live packs retain their unused bytes.
// An empty repository has no live content, including after Forget(AllowEmpty).
//
// Replacement packs and a merged live index become durable before old indexes
// are removed and synced. Only then are obsolete packs removed. Cancellation or
// failure may leave extra copies; retrying collects them. Prune requires no
// source application data. It checks reference structure and verifies every
// copied blob, but is not a substitute for a full Verify of unchanged content.
func Prune(ctx context.Context, r *Repo, app App, opts PruneOptions) (_ *PruneResult, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validatePackExtension(app.PackFileExtension()); err != nil {
		return nil, err
	}
	lock, err := r.AcquireExclusiveLockContext(ctx, "prune", opts.ForceUnlock)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, lock.Release()) }()
	dirs, err := openPruneDirs(r)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, dirs.close()) }()
	known, err := r.LoadBlobIndex()
	if err != nil {
		return nil, err
	}
	progress := newProgressEmitter(opts.Progress)
	live, err := collectPruneReferences(ctx, r, app, known, progress)
	if err != nil {
		return nil, err
	}
	plan, err := planPrune(ctx, dirs, app.PackFileExtension(), live)
	if err != nil {
		return nil, err
	}
	result := &plan.result
	if opts.DryRun || (len(result.PacksToRemove) == 0 && len(known) == len(live) && len(plan.indexNames) <= 1) {
		return result, nil
	}
	if err := r.CleanStaging(); err != nil {
		return result, err
	}
	if err := rewritePrunePacks(ctx, r, app, known, live, plan, progress); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	entries := make([]IndexEntry, 0, len(live))
	for _, entry := range live {
		entries = append(entries, entry)
	}
	if _, err := r.WriteIndex(entries); err != nil {
		return result, err
	}
	progress.emit(ProgressEvent{Stage: ProgressStageSeal, Done: 1, Total: 1, Final: true})
	// The loader unions ALL index files; never delete an old pack while even
	// one old index can select it. New ULIDs need not sort last (clock skew).
	if err := dirs.retireIndexes(ctx, plan.indexNames, progress); err != nil {
		return result, err
	}
	if err := dirs.removePacks(ctx, plan, app.PackFileExtension(), progress); err != nil {
		return result, err
	}
	return result, ctx.Err()
}

func collectPruneReferences(ctx context.Context, r *Repo, app App, known map[pack.BlobID]IndexEntry, progress *progressEmitter) (map[pack.BlobID]IndexEntry, error) {
	manifests, err := r.ListSnapshots()
	if err != nil {
		return nil, err
	}
	// The verifier owns reference traversal. Quick mode reads/authenticates
	// maps and lists and resolves content through index + footer, without
	// buffering content. Any incomplete walk aborts cleanup before publication.
	st := &verifyState{
		ctx: ctx, repo: r, app: app, known: known, quick: true,
		readers: map[string]*pack.Reader{}, readerErrs: map[string]error{},
		checked: map[pack.BlobID]bool{}, readDone: map[pack.BlobID]bool{},
		result: &VerifyResult{},
	}
	defer st.closeReaders()
	for i, manifest := range manifests {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st.verifySnapshot(manifest)
		if len(st.result.Problems) > 0 {
			return nil, fmt.Errorf("backup: cannot prune snapshot %s: %s", manifest.SnapshotID, st.result.Problems[0].Detail)
		}
		progress.emit(ProgressEvent{Stage: ProgressStageVerify, Done: int64(i + 1), Total: int64(len(manifests)), Final: i+1 == len(manifests)})
	}
	live := make(map[pack.BlobID]IndexEntry, len(st.checked))
	for id := range st.checked {
		live[id] = known[id]
	}
	return live, ctx.Err()
}

func rewritePrunePacks(ctx context.Context, r *Repo, app App, known, live map[pack.BlobID]IndexEntry, plan *prunePlan, progress *progressEmitter) error {
	// Only copied entries go through the appender; retaining the old known
	// map there would deduplicate against the very packs being retired.
	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, app.PackFileExtension())
	defer appender.Abort()
	repackIDs := make(map[string]bool, len(plan.result.PacksToRepack))
	for _, id := range plan.result.PacksToRepack {
		repackIDs[id] = true
	}
	var copyEntries []IndexEntry
	for _, entry := range live {
		if repackIDs[entry.PackID] {
			copyEntries = append(copyEntries, entry)
		}
	}
	slices.SortFunc(copyEntries, func(a, b IndexEntry) int {
		if a.PackID < b.PackID {
			return -1
		}
		if a.PackID > b.PackID {
			return 1
		}
		if a.Offset < b.Offset {
			return -1
		}
		if a.Offset > b.Offset {
			return 1
		}
		return 0
	})
	for i, entry := range copyEntries {
		if err := copyPruneBlob(ctx, r, known, entry.Blob, app.PackFileExtension(), appender); err != nil {
			return err
		}
		progress.emit(ProgressEvent{Stage: ProgressStagePack, Done: int64(i + 1), Total: int64(len(copyEntries)), Final: i+1 == len(copyEntries)})
	}
	packs, entries, err := appender.Finish()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		live[entry.Blob] = entry
	}
	for _, id := range packs {
		info, err := os.Stat(r.packPath(id, app.PackFileExtension()))
		if err != nil {
			return err
		}
		plan.result.BytesWritten += info.Size()
	}
	return nil
}

func copyPruneBlob(ctx context.Context, r *Repo, known map[pack.BlobID]IndexEntry, id pack.BlobID, ext string, appender *PackAppender) error {
	source, err := r.OpenBlob(ctx, known, id, nil, ext)
	if err != nil {
		return err
	}
	prepared, prepareErr := pack.PrepareBlob(ctx, source, uint64(source.Size()), pack.DefaultZstdLevel, pack.AppendStreamOptions{
		ExpectedID: &id, ScratchDir: r.Path(stagingDirName),
	})
	if err := errors.Join(prepareErr, source.Close()); err != nil {
		if prepared != nil {
			err = errors.Join(err, prepared.Close())
		}
		return fmt.Errorf("backup: copying live blob %s: %w", id, err)
	}
	_, err = appender.AddPrepared(ctx, prepared)
	return err
}
