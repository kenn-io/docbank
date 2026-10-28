package processing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"uuid"

	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
)

func (s *Service) prepareRedactionRender(ctx context.Context, op *nativePDFOperation,
	request RedactionRenderRequest, runtime *pagerender.Runtime, controls nativeRedactionControls,
) (admitted admittedRedaction, pdf renderedNativePDF, resultErr error) {
	defer func() {
		if resultErr != nil {
			clear(admitted.data)
		}
	}()
	resolve := RedactionResolveRequest{Preparation: request.Preparation, Masks: request.Masks}
	var reservation *NumberedReservation
	if request.AllocationID != uuid.Nil() {
		review, err := BuildRedactionReview(request.Preparation, request.Masks, request.Preparation.Selection.Revision)
		if err != nil {
			return admitted, pdf, err
		}
		if review.SHA256 != request.ReviewSHA256 {
			return admitted, pdf, ErrRedactionReviewChanged
		}
		a, err := s.catalog.BatesAllocation(ctx, request.AllocationID.String())
		if errors.Is(err, store.ErrNotFound) {
			return admitted, pdf, ErrNumberedAllocationNotFound
		}
		if err != nil {
			return admitted, pdf, err
		}
		ns, err := s.catalog.BatesNamespace(ctx, a.NamespaceID, "", "", 0)
		if errors.Is(err, store.ErrNotFound) {
			return admitted, pdf, ErrNumberedNamespaceNotFound
		}
		if err != nil {
			return admitted, pdf, err
		}
		recipe, err := BuildNumberedRecipe(uuid.MustParse(s.catalog.VaultID()), ns, a.StartSequence, review)
		if err != nil {
			return admitted, pdf, err
		}
		digest, err := recipe.SHA256()
		if err != nil {
			return admitted, pdf, err
		}
		if digest != a.RecipeSHA256 {
			return admitted, pdf, ErrRedactionReviewChanged
		}
		r, err := s.numberedReservation(ctx, recipe, a)
		if err != nil {
			return admitted, pdf, err
		}
		reservation = &r
		admitted.review, admitted.revision = review, request.Preparation.Selection.Revision
		admitted.presentation, err = recipe.Presentation()
		if err != nil {
			return admitted, pdf, err
		}
		artifact, err := s.catalog.BatesArtifact(ctx, a.AllocationID)
		if err == nil {
			pdf, err = s.stageNumberedArtifact(ctx, op.directory, r, artifact)
			if err != nil {
				return admitted, pdf, err
			}
			admitted.numbered, err = NumberedArtifactDelivery(r, artifact)
			if err != nil {
				return admitted, renderedNativePDF{}, errors.Join(err, pdf.Close())
			}
			return admitted, pdf, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return admitted, pdf, err
		}
	}
	if runtime == nil || !pagerender.SupportedPlatform() {
		return admitted, pdf, ErrRedactionPreparationUnavailable
	}
	var err error
	if reservation == nil {
		admitted, err = s.freshRedactionReview(ctx, op, resolve, runtime)
	} else {
		admitted.data, err = s.captureNumberedSource(ctx, *reservation)
		if err == nil {
			admitted, err = s.reviewCapturedPDF(ctx, op, resolve, runtime, admitted)
		}
	}
	if err != nil {
		return admitted, pdf, err
	}
	if admitted.review.SHA256 != request.ReviewSHA256 {
		return admitted, pdf, ErrRedactionReviewChanged
	}
	pdf, err = renderRedactionPDF(ctx, op.directory, admitted, controls)
	if err != nil {
		return admitted, pdf, err
	}
	if reservation != nil {
		artifact, err := s.publishNumberedPDF(ctx, *reservation, pdf)
		if err != nil {
			return admitted, renderedNativePDF{}, errors.Join(err, pdf.Close())
		}
		// A concurrent publisher's first artifact is authoritative even if a later
		// renderer made another byte representation of the same reviewed plan.
		if artifact.BlobSHA256 != pdf.digest.SHA256 || artifact.Size != pdf.digest.Size {
			if err := pdf.Close(); err != nil {
				return admitted, renderedNativePDF{}, err
			}
			pdf, err = s.stageNumberedArtifact(ctx, op.directory, *reservation, artifact)
			if err != nil {
				return admitted, pdf, err
			}
		}
		admitted.numbered, err = NumberedArtifactDelivery(*reservation, artifact)
		if err != nil {
			return admitted, renderedNativePDF{}, errors.Join(err, pdf.Close())
		}
	}
	return admitted, pdf, nil
}

// NumberedArtifactDelivery validates retained authority and derives its portable receipt.
func NumberedArtifactDelivery(r NumberedReservation, a store.BatesArtifact) (*NumberedDelivery, error) {
	if err := ValidateNumberedReservation(r, nil); err != nil {
		return nil, err
	}
	recipe, err := canonical.Marshal(r.Recipe)
	if err != nil {
		return nil, err
	}
	if a.AllocationID != r.AllocationID.String() || a.ArtifactID != a.AllocationID ||
		a.RecipeSHA256 != r.RecipeSHA256 || sha256HexBytes(a.RecipeJSON) != r.RecipeSHA256 ||
		string(recipe) != string(a.RecipeJSON) || !reflect.DeepEqual(a.Pages, r.Pages) {
		return nil, ErrRedactionPreparationIntegrity
	}
	manifest, err := store.BatesArtifactManifestSHA(a)
	if err != nil {
		return nil, err
	}
	if manifest != a.ManifestSHA256 {
		return nil, ErrRedactionPreparationIntegrity
	}
	r.State = "committed"
	result := &NumberedDelivery{Reservation: r, ArtifactManifestSHA256: a.ManifestSHA256}
	presentation, err := r.Recipe.Presentation()
	if err != nil {
		return nil, err
	}
	for _, p := range presentation {
		layout, err := canonical.Marshal(p.Layout)
		if err != nil {
			return nil, err
		}
		endorsements, err := canonical.Marshal(p.Endorsements)
		if err != nil {
			return nil, err
		}
		result.LayoutSHA256 = append(result.LayoutSHA256, sha256HexBytes(layout))
		result.EndorsementsSHA256 = append(result.EndorsementsSHA256, sha256HexBytes(endorsements))
	}
	return result, nil
}

func (s *Service) publishNumberedPDF(ctx context.Context, r NumberedReservation, pdf renderedNativePDF) (artifact store.BatesArtifact, err error) {
	recipe, err := canonical.Marshal(r.Recipe)
	if err != nil {
		return artifact, err
	}
	err = s.gate.MutateContext(ctx, func() error {
		return s.blobs.WithMutation(ctx, func() error {
			written, err := s.blobs.WriteDetailedContext(ctx, io.NewSectionReader(pdf.file, 0, pdf.digest.Size))
			if err != nil {
				return err
			}
			if written.Size != pdf.digest.Size || written.Hash != pdf.digest.SHA256 {
				return ErrRedactionPreparationIntegrity
			}
			encoding, err := written.EncodingName()
			if err != nil {
				return err
			}
			artifact, err = s.catalog.PublishBatesArtifact(ctx, store.BatesArtifactPublication{
				ArtifactID: r.AllocationID.String(), AllocationID: r.AllocationID.String(), BlobSHA256: written.Hash, Size: written.Size,
				PageCount: r.Recipe.PageCount, RecipeJSON: recipe, Pages: r.Pages}, store.BlobPhysical{
				Encoding: encoding, StoredBytes: written.StoredSize, PackEligible: written.PackEligible, MD5: written.MD5, Created: written.Created})
			return err
		})
	})
	return artifact, err
}

func (s *Service) stageNumberedArtifact(ctx context.Context, dir string, r NumberedReservation,
	artifact store.BatesArtifact,
) (result renderedNativePDF, resultErr error) {
	if _, err := NumberedArtifactDelivery(r, artifact); err != nil {
		return result, err
	}
	if artifact.Size < 1 || artifact.Size > 512<<20 {
		return result, ErrRedactionPreparationIntegrity
	}
	file, err := os.CreateTemp(dir, "retained-*.pdf")
	if err != nil {
		return result, err
	}
	defer func() {
		if result.file == nil {
			resultErr = errors.Join(resultErr, file.Close())
		}
	}()
	err = s.gate.CaptureContext(ctx, func() error {
		reader, size, err := s.blobs.OpenStreamContext(ctx, artifact.BlobSHA256)
		if err != nil {
			return preparationSourceError(err)
		}
		n, readErr := io.Copy(file, io.LimitReader(&contextReader{ctx: ctx, reader: reader}, artifact.Size+1))
		err = errors.Join(readErr, reader.Close())
		if err != nil {
			return preparationSourceError(err)
		}
		if size != artifact.Size || n != size || !reader.Verified() {
			return ErrRedactionPreparationIntegrity
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	digest, err := redactionFileDigest(ctx, file)
	if err != nil {
		return result, err
	}
	if digest.Size != artifact.Size || digest.SHA256 != artifact.BlobSHA256 {
		return result, fmt.Errorf("%w: retained numbered PDF", ErrRedactionPreparationIntegrity)
	}
	return renderedNativePDF{file: file, digest: digest}, nil
}
