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

const batesExportTimeout = 2 * time.Minute

// PublishBatesExport runs one bounded Bates PDF transformation and publishes
// its verified artifact. Exact retries reuse the immutable artifact.
func PublishBatesExport(ctx context.Context, catalog *store.Store, blobs *blob.Store, allocationID string, recipe pdfstamp.Recipe) (store.BatesArtifact, error) {
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
	workerContext, cancel := context.WithTimeout(ctx, batesExportTimeout)
	defer cancel()
	var groups [][]byte
	for first := 0; first < len(pages); {
		last := first + 1
		for last < len(pages) && pages[last].SourceBlobSHA256 == pages[first].SourceBlobSHA256 &&
			pages[last].OccurrenceID == pages[first].OccurrenceID {
			last++
		}
		reader, size, openErr := blobs.OpenSeekableContext(workerContext, pages[first].SourceBlobSHA256)
		if openErr != nil {
			return store.BatesArtifact{}, openErr
		}
		labels := make([]pdfstamp.PageLabel, last-first)
		for index := first; index < last; index++ {
			labels[index-first] = pdfstamp.PageLabel{SourcePage: pages[index].SourcePage, Label: pages[index].Label}
		}
		groupRecipe := recipe
		groupRecipe.StartAt = int(allocation.StartSequence) + first
		var stamped bytes.Buffer
		result, stampErr := pdfstamp.StampSelectedSupervised(workerContext, reader, labels, groupRecipe, &stamped)
		closeErr := reader.Close()
		if stampErr != nil || closeErr != nil {
			return store.BatesArtifact{}, errors.Join(stampErr, closeErr)
		}
		if size < 1 || result.PageCount != len(labels) {
			return store.BatesArtifact{}, store.ErrBatesPageCountMismatch
		}
		groups = append(groups, stamped.Bytes())
		first = last
	}
	allLabels := make([]pdfstamp.PageLabel, len(pages))
	for index, page := range pages {
		allLabels[index] = pdfstamp.PageLabel{SourcePage: page.SourcePage, Label: page.Label}
	}
	var output bytes.Buffer
	result, err := pdfstamp.CombineStampedSupervised(workerContext, groups, allLabels, &output)
	if err != nil || result.PageCount != len(pages) {
		return store.BatesArtifact{}, errors.Join(err, store.ErrBatesPageCountMismatch)
	}
	recipeJSON, err := canonical.Marshal(recipe)
	if err != nil {
		return store.BatesArtifact{}, fmt.Errorf("encode Bates recipe: %w", err)
	}
	var artifact store.BatesArtifact
	err = blobs.WithMutation(workerContext, func() error {
		written, writeErr := blobs.WriteDetailedContext(workerContext, bytes.NewReader(output.Bytes()))
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
		artifact, writeErr = catalog.PublishBatesArtifact(workerContext, store.BatesArtifactPublication{
			ArtifactID: allocationID, AllocationID: allocationID, BlobSHA256: written.Hash,
			Size: written.Size, PageCount: result.PageCount, RecipeJSON: recipeJSON, Pages: pages,
		}, store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize, PackEligible: written.PackEligible,
			MD5: written.MD5, Created: written.Created})
		return writeErr
	})
	return artifact, err
}

// ReadBatesExport fully re-hashes and re-parses retained bytes against the
// durable page receipts before returning a download.
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
	labels := make([]pdfstamp.PageLabel, len(artifact.Pages))
	for index, page := range artifact.Pages {
		labels[index] = pdfstamp.PageLabel{SourcePage: page.SourcePage, Label: page.Label}
	}
	verified, err := pdfstamp.VerifyStamped(ctx, data, labels)
	if err != nil || verified.SHA256 != artifact.BlobSHA256 || verified.Size != artifact.Size || verified.PageCount != artifact.PageCount {
		return nil, store.BatesArtifact{}, errors.Join(err, errors.New("Bates artifact PDF differs from manifest")) //nolint:staticcheck // Bates is a proper name.
	}
	return data, artifact, nil
}
