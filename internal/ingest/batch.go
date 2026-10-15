package ingest

import (
	"context"
	"errors"
	"os"
	"sync"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

type pendingLocalFile struct {
	parent               *ingestDirectory
	openPath, sourcePath string
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
		workers.Go(func() {
			file.content, file.err = files.ing.prepareLocalFileWith(
				ctx, func() (*os.File, error) { return blob.OpenNoFollow(file.openPath) },
				file.sourcePath, progress, nil,
			)
		})
	}
	workers.Wait()
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
			added, resolution, err = files.recordPreparedFile(ctx, file.parent, file.sourcePath, file.content)
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
