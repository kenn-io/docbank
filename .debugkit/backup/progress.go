package backup

// ProgressStage names one phase of a backup operation that reports progress.
type ProgressStage string

const (
	// ProgressStagePruneIndexes covers retiring superseded repository indexes.
	ProgressStagePruneIndexes ProgressStage = "prune_indexes"
	// ProgressStagePrunePacks covers deleting obsolete pack files.
	ProgressStagePrunePacks ProgressStage = "prune_packs"
	// ProgressStageFreeze covers opening the frozen read session.
	ProgressStageFreeze ProgressStage = "freeze"
	// ProgressStageScan covers the full page-hash scan.
	ProgressStageScan ProgressStage = "scan"
	// ProgressStageMetadata covers portable metadata capture or restore.
	ProgressStageMetadata ProgressStage = "metadata"
	// ProgressStagePack covers compressing and writing changed-page blobs
	// into pack files. On a first backup this is most of the wall clock
	// (every page is new); on a no-change backup it is skipped entirely.
	ProgressStagePack ProgressStage = "pack"
	// ProgressStageAttachments covers attachment content capture.
	ProgressStageAttachments ProgressStage = "attachments"
	// ProgressStageSeal covers sealing packs and writing the index.
	ProgressStageSeal ProgressStage = "seal"
	// ProgressStageVerify covers Verify's per-snapshot and per-blob checks.
	ProgressStageVerify ProgressStage = "verify"
	// ProgressStageRestoreDB covers materializing the database file from
	// page-map runs during Restore.
	ProgressStageRestoreDB ProgressStage = "db"
	// ProgressStageExtras covers laying out captured extras files during
	// Restore.
	ProgressStageExtras ProgressStage = "extras"
	// ProgressStageIntegrityCheck covers Restore's optional SQLite
	// integrity_check. The check reports only start and completion because
	// SQLite does not expose intermediate progress.
	ProgressStageIntegrityCheck ProgressStage = "integrity_check"
	// ProgressStageRestoreStats covers recomputing and comparing the restored
	// database's application-defined manifest statistics.
	ProgressStageRestoreStats ProgressStage = "restore_stats"
	// ProgressStageProof is retained for source compatibility. Restore emits
	// the more specific IntegrityCheck and RestoreStats stages instead.
	//
	// Deprecated: use ProgressStageIntegrityCheck and ProgressStageRestoreStats.
	ProgressStageProof ProgressStage = "proof"
)

// ProgressEvent reports one step of progress within a Stage. Done and Total
// are item counts (pages, files, blobs, snapshots) — Total is 0 when the
// item count isn't known in advance. BytesDone and BytesTotal are the
// corresponding byte counts where meaningful; BytesTotal is 0 when the byte
// total isn't known ahead of time (a renderer can still show BytesDone and a
// derived rate). Final marks the last event the operation will emit for
// this Stage, i.e., the stage has completed.
type ProgressEvent struct {
	Stage      ProgressStage
	Done       int64
	Total      int64
	BytesDone  int64
	BytesTotal int64
	Final      bool
}

// progressEmitter calls an optional callback with each ProgressEvent. A nil
// callback (and a nil *progressEmitter itself) makes emit a no-op, so
// callers never need to scatter nil checks around their own code.
// internal/backup emits every event unconditionally and cheaply — it does
// no throttling of its own; that's a rendering concern the CLI owns, since
// only the renderer knows whether its output is a live terminal or a pipe.
type progressEmitter struct {
	fn func(ProgressEvent)
}

// newProgressEmitter builds a progressEmitter over fn. fn may be nil, which
// makes the emitter fully silent.
func newProgressEmitter(fn func(ProgressEvent)) *progressEmitter {
	return &progressEmitter{fn: fn}
}

// emit calls the emitter's callback with ev. No-op when the emitter (or its
// callback) is nil.
func (e *progressEmitter) emit(ev ProgressEvent) {
	if e == nil || e.fn == nil {
		return
	}
	e.fn(ev)
}
