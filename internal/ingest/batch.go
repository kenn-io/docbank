package ingest

import (
	"context"
	"errors"
	"os"
	"sync"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

var openBatchSource = blob.OpenNoFollow

type pendingLocalFile struct {
	parent               *ingestDirectory
	openPath, sourcePath string
	replacement          *store.Node // nil for ordinary imports; ID zero for an absent target.
	content              localFileContent
	err                  error
}

// flush overlaps durable publication, then commits in input order. Joining
// every preparation before metadata and cleanup keeps the mutation lease's
// lifetime unchanged and prevents deleting loose duplicates beneath a reader.
func (files *filesystemIngest) flush(
	ctx context.Context, rep *Report, progress *progressTracker,
) (retErr error) {
	if len(files.pending) == 0 {
		return nil
	}
	pending := files.pending
	files.pending = nil
	if err := ctx.Err(); err != nil {
		return err
	}
	var workers sync.WaitGroup
	for i := range pending {
		file := &pending[i]
		if file.err != nil {
			continue
		}
		workers.Go(func() {
			file.content, file.err = files.ing.prepareLocalFileWith(
				ctx, func() (*os.File, error) { return openBatchSource(file.openPath) },
				file.sourcePath, progress, nil,
			)
		})
	}
	workers.Wait()
	// Publication order can differ from input order. An earlier duplicate may
	// need a later writer's verified receipt to restore missing authority.
	created := make(map[string]store.BlobPhysical, len(pending))
	for _, file := range pending {
		if file.err == nil && file.content.physical.Created {
			created[file.content.hash] = file.content.physical
		}
	}
	defer func() {
		// Cancellation can leave prepared files that never reached a transaction.
		for _, file := range pending {
			if file.content.hash != "" {
				retErr = errors.Join(retErr, files.ing.cleanupLoose(file.content.hash))
			}
		}
	}()
	for i := range pending {
		file := &pending[i]
		if err := ctx.Err(); err != nil {
			return err
		}
		var added bool
		var resolution store.IngestDirectoryResolution
		err := file.err
		if err == nil {
			if physical, ok := created[file.content.hash]; ok {
				file.content.physical = physical
			}
			if file.replacement != nil {
				added, resolution, err = files.recordReplacement(
					ctx, file.parent, file.sourcePath, file.content, file.replacement,
				)
			} else {
				added, resolution, err = files.recordPreparedFile(ctx, file.parent, file.sourcePath, file.content)
			}
		}
		if err == nil && added && (file.replacement == nil || file.replacement.ID == 0) {
			rep.Created++
		}
		if file.content.hash != "" {
			err = mutationCleanupResult(err, files.ing.cleanupLoose(file.content.hash))
			file.content.hash = ""
		}
		if err := files.recordOutcome(ctx, rep, file.sourcePath, added, resolution, err, progress); err != nil {
			return err
		}
	}
	return nil
}
