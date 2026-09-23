package processing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfstamp"
	"go.kenn.io/docbank/internal/store"
)

// batesWorkerTimeout bounds each supervised PDF worker call, not the whole export.
const batesWorkerTimeout = 2 * time.Minute

// PublishBatesExport runs bounded Bates PDF transformations and publishes the
// verified artifact. Exact retries reuse the immutable artifact.
func PublishBatesExport(ctx context.Context, catalog *store.Store, blobs *blob.Store, allocationID string, recipe pdfstamp.Recipe) (store.BatesArtifact, error) {
	recipe = recipe.Normalized()
	if catalog == nil || blobs == nil {
		return store.BatesArtifact{}, errors.New("Bates export requires catalog and blob storage") //nolint:staticcheck // Bates is a proper name.
	}
	if existing, err := catalog.BatesArtifact(ctx, allocationID); err == nil {
		digest, digestErr := recipe.SHA256()
		if digestErr != nil || digest != existing.RecipeSHA256 {
			return store.BatesArtifact{}, store.ErrBatesReservationConflict
		}
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.BatesArtifact{}, err
	}
	allocation, pages, err := catalog.BatesPublicationPlan(ctx, allocationID)
	if err != nil {
		return store.BatesArtifact{}, err
	}
	recipeSHA, err := recipe.SHA256()
	if err != nil || recipeSHA != allocation.RecipeSHA256 || recipe.NamespaceID != allocation.NamespaceID ||
		int64(recipe.StartAt) != allocation.StartSequence {
		return store.BatesArtifact{}, store.ErrBatesReservationConflict
	}
	output, result, err := stampBatesPages(ctx, blobs, allocation, pages, recipe)
	if err != nil {
		return store.BatesArtifact{}, err
	}
	recipeJSON, err := canonical.Marshal(recipe)
	if err != nil {
		return store.BatesArtifact{}, fmt.Errorf("encode Bates recipe: %w", err)
	}
	var artifact store.BatesArtifact
	err = blobs.WithMutation(ctx, func() error {
		written, writeErr := blobs.WriteDetailedContext(ctx, bytes.NewReader(output))
		if writeErr != nil {
			return writeErr
		}
		if written.Hash != result.SHA256 || written.Size != result.Size {
			return errors.New("Bates export blob receipt differs from verified output") //nolint:staticcheck // Bates is a proper name.
		}
		encoding, encodeErr := written.EncodingName()
		if encodeErr != nil {
			return encodeErr
		}
		artifact, writeErr = catalog.PublishBatesArtifact(ctx, store.BatesArtifactPublication{
			ArtifactID: allocationID, AllocationID: allocationID, BlobSHA256: written.Hash,
			Size: written.Size, PageCount: result.PageCount, RecipeJSON: recipeJSON, Pages: pages,
		}, store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize, PackEligible: written.PackEligible,
			MD5: written.MD5, Created: written.Created})
		return writeErr
	})
	return artifact, err
}

// stampBatesPages stamps each consecutive same-source group in its own
// supervised worker call, then combines the groups. It fails as soon as the
// retained groups exceed the stamped-output limit.
func stampBatesPages(ctx context.Context, blobs *blob.Store, allocation store.BatesAllocation,
	pages []store.BatesArtifactPage, recipe pdfstamp.Recipe,
) ([]byte, pdfstamp.Result, error) {
	var groups [][]byte
	var total int64
	for first := 0; first < len(pages); {
		last := first + 1
		for last < len(pages) && pages[last].SourceBlobSHA256 == pages[first].SourceBlobSHA256 &&
			pages[last].OccurrenceID == pages[first].OccurrenceID {
			last++
		}
		labels := make([]pdfstamp.PageLabel, last-first)
		for index := first; index < last; index++ {
			labels[index-first] = pdfstamp.PageLabel{SourcePage: pages[index].SourcePage, Label: pages[index].Label}
		}
		groupRecipe := recipe
		groupRecipe.StartAt = int(allocation.StartSequence) + first
		stamped, err := stampBatesGroup(ctx, blobs, pages[first].SourceBlobSHA256, labels, groupRecipe)
		if err != nil {
			return nil, pdfstamp.Result{}, err
		}
		total += int64(len(stamped))
		if total > pdfstamp.MaxOutputBytes {
			return nil, pdfstamp.Result{}, fmt.Errorf("%w: stamped pages exceed %d bytes",
				pdfstamp.ErrStampEngineFailure, pdfstamp.MaxOutputBytes)
		}
		groups = append(groups, stamped)
		first = last
	}
	allLabels := make([]pdfstamp.PageLabel, len(pages))
	for index, page := range pages {
		allLabels[index] = pdfstamp.PageLabel{SourcePage: page.SourcePage, Label: page.Label}
	}
	workerContext, cancel := context.WithTimeout(ctx, batesWorkerTimeout)
	defer cancel()
	var output bytes.Buffer
	result, err := pdfstamp.CombineStampedSupervised(workerContext, groups, allLabels, &output)
	if err != nil {
		return nil, pdfstamp.Result{}, err
	}
	if result.PageCount != len(pages) {
		return nil, pdfstamp.Result{}, store.ErrBatesPageCountMismatch
	}
	return output.Bytes(), result, nil
}

func stampBatesGroup(ctx context.Context, blobs *blob.Store, sourceSHA256 string,
	labels []pdfstamp.PageLabel, recipe pdfstamp.Recipe,
) ([]byte, error) {
	workerContext, cancel := context.WithTimeout(ctx, batesWorkerTimeout)
	defer cancel()
	reader, _, err := blobs.OpenSeekableContext(workerContext, sourceSHA256)
	if err != nil {
		return nil, err
	}
	var stamped bytes.Buffer
	result, stampErr := pdfstamp.StampSelectedSupervised(workerContext, reader, labels, recipe, &stamped)
	closeErr := reader.Close()
	if stampErr != nil || closeErr != nil {
		return nil, errors.Join(stampErr, closeErr)
	}
	if result.PageCount != len(labels) {
		return nil, store.ErrBatesPageCountMismatch
	}
	return stamped.Bytes(), nil
}

// ReadBatesExport returns retained artifact bytes after the blob store
// re-hashes them against the catalog digest and size. The supervised worker
// verified the PDF's pages and labels before publication, so the daemon does
// not parse the PDF again here.
func ReadBatesExport(ctx context.Context, catalog *store.Store, blobs *blob.Store, allocationID string) ([]byte, store.BatesArtifact, error) {
	artifact, err := catalog.BatesArtifact(ctx, allocationID)
	if err != nil {
		return nil, store.BatesArtifact{}, err
	}
	reader, size, err := blobs.OpenStreamContext(ctx, artifact.BlobSHA256)
	if err != nil {
		return nil, store.BatesArtifact{}, err
	}
	if size != artifact.Size {
		return nil, store.BatesArtifact{}, errors.Join(errors.New("Bates artifact size differs from catalog"), reader.Close()) //nolint:staticcheck // Bates is a proper name.
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, size+1))
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil || int64(len(data)) != size || !reader.Verified() {
		return nil, store.BatesArtifact{}, errors.Join(err, errors.New("Bates artifact bytes failed verification")) //nolint:staticcheck // Bates is a proper name.
	}
	return data, artifact, nil
}
