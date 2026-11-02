package backup

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/kit/pack"
)

type sealedCapture struct {
	newPacks   []string
	newIndex   string
	bytesAdded int64
}

func captureSnapshotFiles(
	ctx context.Context,
	r *Repo,
	app App,
	opts CreateOptions,
	parent *Manifest,
	known map[pack.BlobID]IndexEntry,
	info *ContentInfo,
	appender *PackAppender,
	progress *progressEmitter,
) (*AttachmentCapture, []string, ManifestExtras, error) {
	parentSeen := map[string]bool{}
	if parent != nil {
		var err error
		var parentRefs []ContentRef
		parentRefs, parentSeen, err = LoadListRefs(
			r, known, parent.Attachments.Lists, nil, app.PackFileExtension())
		if err != nil {
			return nil, nil, ManifestExtras{}, err
		}
		if opts.ContentSource != nil {
			// Reuse recorded sizes to keep unchanged chunked objects on the
			// same capture path. Capture still reads and verifies their bytes.
			sizes := make(map[string]int64, len(parentRefs))
			for _, ref := range parentRefs {
				sizes[ref.Hash] = ref.Size
			}
			for i := range info.Refs {
				if info.Refs[i].Size == -1 {
					if size, ok := sizes[info.Refs[i].Hash]; ok {
						info.Refs[i].Size = size
					}
				}
			}
		}
	}
	// Inherit lists only while the parent union remains a subset of the
	// current references. After shrinkage, one fresh full list must replace
	// the inherited union or Verify's population invariant would fail.
	shrunk := parentUnionShrank(parentSeen, info.Refs)
	captureSeen := parentSeen
	if shrunk {
		captureSeen = map[string]bool{}
	}
	capture, err := CaptureAttachments(
		ctx, opts.ContentDir, info.Refs, captureSeen, appender, CaptureOptions{
			Jobs:   opts.Jobs,
			Source: opts.ContentSource,
			Progress: func(done, total int, bytesRead int64) {
				progress.emit(ProgressEvent{
					Stage: ProgressStageAttachments, Done: int64(done),
					Total: int64(total), BytesDone: bytesRead,
				})
			},
		})
	if err != nil {
		return nil, nil, ManifestExtras{}, err
	}
	progress.emit(ProgressEvent{
		Stage: ProgressStageAttachments, Done: capture.Blobs, Total: capture.Blobs,
		BytesDone: capture.BlobBytes, BytesTotal: capture.BlobBytes, Final: true,
	})
	var lists []string
	if shrunk {
		if capture.HasNewList {
			lists = []string{capture.NewListBlob.String()}
		}
	} else {
		if parent != nil {
			lists = append(lists, parent.Attachments.Lists...)
		}
		if capture.HasNewList {
			lists = append(lists, capture.NewListBlob.String())
		}
	}

	extras, err := CaptureExtras(ctx, ExtrasOptions{
		DataDir:               opts.DataDir,
		Spec:                  opts.Extras,
		AllowPlaintextSecrets: opts.AllowPlaintextSecrets,
		Encrypted:             false,
		ContentDirName:        app.ContentDirName(),
		DBFileName:            app.DBFileName(),
	}, appender)
	if err != nil {
		return nil, nil, ManifestExtras{}, err
	}
	return capture, lists, extras, nil
}

func preparePortableMetadata(
	ctx context.Context,
	snapshot MetadataSnapshot,
	appender *PackAppender,
	progress *progressEmitter,
) (pack.BlobID, int64, string, error) {
	metadataReader, metadataBytes, err := snapshot.OpenMetadata(ctx)
	if err != nil {
		if metadataReader != nil {
			err = errors.Join(err, metadataReader.Close())
		}
		return pack.BlobID{}, 0, "", fmt.Errorf("backup: opening portable metadata: %w", err)
	}
	if metadataReader == nil || metadataBytes < 0 || metadataBytes > MaxObjectBytes {
		if metadataReader != nil {
			_ = metadataReader.Close()
		}
		return pack.BlobID{}, 0, "", fmt.Errorf("backup: invalid portable metadata size %d", metadataBytes)
	}
	progress.emit(ProgressEvent{
		Stage: ProgressStageMetadata, Total: 1, BytesTotal: metadataBytes,
	})
	metadataID, size, recipe, captureErr := captureObject(ctx, metadataReader, metadataBytes, nil, appender)
	if err := errors.Join(captureErr, metadataReader.Close()); err != nil {
		return pack.BlobID{}, 0, "", fmt.Errorf("backup: preparing portable metadata: %w", err)
	}
	progress.emit(ProgressEvent{
		Stage: ProgressStageMetadata, Done: 1, Total: 1,
		BytesDone: metadataBytes, BytesTotal: metadataBytes, Final: true,
	})
	return metadataID, size, recipe, nil
}

func sealSnapshotCapture(
	ctx context.Context, r *Repo, appender *PackAppender, progress *progressEmitter,
) (sealedCapture, error) {
	if err := ctx.Err(); err != nil {
		return sealedCapture{}, err
	}
	progress.emit(ProgressEvent{Stage: ProgressStageSeal, Total: 1})
	newPacks, newEntries, err := appender.Finish()
	if err != nil {
		return sealedCapture{}, err
	}
	progress.emit(ProgressEvent{Stage: ProgressStageSeal, Done: 1, Total: 1, Final: true})

	var bytesAdded int64
	for _, entry := range newEntries {
		bytesAdded += int64(entry.StoredLen)
	}
	newIndex := ""
	if len(newEntries) > 0 {
		newIndex, err = r.WriteIndex(newEntries)
		if err != nil {
			return sealedCapture{}, err
		}
	}
	return sealedCapture{newPacks: newPacks, newIndex: newIndex, bytesAdded: bytesAdded}, nil
}
