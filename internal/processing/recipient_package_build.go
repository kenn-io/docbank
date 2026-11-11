package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"
	"uuid"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/store"
)

// BuildRecipientPackage packages an existing numbered delivery; it never renders
// or publishes an allocation. publish fences the handoff against owner revocation.
func (s *Service) BuildRecipientPackage(ctx context.Context, owner string, request RedactionRenderRequest,
	publish func(func() error) error,
) (result PreparedRecipientPackage, resultErr error) {
	if owner == "" || request.AllocationID == uuid.Nil() || !canonical.IsSHA256Hex(request.ReviewSHA256) {
		return result, ErrRecipientPackageInvalid
	}
	if s == nil {
		return result, ErrRedactionPreparationUnavailable
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	e, err := s.reserveRecipientPackage(owner)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(e.ctx, cancel)
	defer func() {
		stop()
		cancel()
		s.recipientPackages.mu.Lock()
		defer s.recipientPackages.mu.Unlock()
		if !e.ready {
			e.retired = true
		}
		e.pins--
		resultErr = errors.Join(resultErr, s.cleanRecipientPackageLocked())
		if resultErr != nil && s.lifecycle.Err() != nil {
			resultErr = errors.Join(resultErr, ErrRedactionPreparationUnavailable)
		}
	}()
	inputs, err := s.admitRecipientPackage(ctx, request)
	if err != nil {
		return result, err
	}
	op, err := s.beginNativePDFOperation(ctx, "recipient package")
	if err != nil {
		return result, err
	}
	s.recipientPackages.mu.Lock()
	e.directory = op.directory
	s.recipientPackages.mu.Unlock()
	defer func() {
		op.directory = "" // The one-entry store owns failed cleanup as well as ready files.
		resultErr = errors.Join(resultErr, op.Close())
	}()
	pdf, err := s.stageNumberedArtifact(op.ctx, op.directory, inputs.reservation, inputs.artifact)
	if err != nil {
		return result, err
	}
	defer func() {
		if pdf.file != nil {
			resultErr = errors.Join(resultErr, pdf.Close())
		}
	}()
	presentation, err := inputs.reservation.Recipe.Presentation()
	if err != nil {
		return result, err
	}
	if err := pdfproduction.VerifySaved(op.ctx, pdf.file, pdf.digest.Size,
		inputs.review.Resolved, inputs.review.Recipe, presentation); err != nil {
		return result, err
	}
	archive, err := os.CreateTemp(op.directory, "recipient-*.zip")
	if err != nil {
		return result, err
	}
	s.recipientPackages.mu.Lock()
	e.file = archive
	s.recipientPackages.mu.Unlock()
	hash := sha256.New()
	pdfPath := pdf.file.Name()
	if _, err := pdf.file.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	receipt, err := WriteRecipientPackage(op.ctx, io.MultiWriter(archive, hash),
		[]RecipientPackageDocument{inputs.document}, func(context.Context, int) (io.ReadCloser, []byte, error) {
			file := pdf.file
			pdf.file = nil // The writer owns and closes this exact verified handle.
			return file, inputs.text, nil
		})
	if err != nil {
		return result, err
	}
	if err := os.Remove(pdfPath); err != nil {
		return result, err
	}
	files, err := archive.Stat()
	if err != nil {
		return result, err
	}
	receipt.Documents = []RecipientPackageDocument{inputs.document}
	if err = VerifyRecipientPackage(op.ctx, archive, files.Size(), receipt); err != nil {
		return result, err
	}
	result = PreparedRecipientPackage{ID: e.result.ID, AllocationID: request.AllocationID,
		Receipt: receipt, Size: files.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)),
		ExpiresAt: s.clock().UTC().Add(recipientPackageLifetime)}
	commit := func() error {
		s.recipientPackages.mu.Lock()
		defer s.recipientPackages.mu.Unlock()
		if err := op.ctx.Err(); err != nil {
			return err
		}
		if e.retired || e.ctx.Err() != nil {
			return ErrRecipientPackageUnavailable
		}
		e.result, e.ready = result, true
		e.timer = time.AfterFunc(recipientPackageLifetime, func() {
			s.recipientPackages.mu.Lock()
			defer s.recipientPackages.mu.Unlock()
			if s.recipientPackages.entry != e {
				return
			}
			e.retired = true
			if err := s.cleanRecipientPackageLocked(); err != nil {
				slog.Error("removing expired recipient package", "error", err)
			}
		})
		return nil
	}
	if publish == nil {
		err = commit()
	} else {
		err = publish(commit)
	}
	return result, err
}

type recipientPackageInputs struct {
	reservation NumberedReservation
	artifact    store.BatesArtifact
	review      RedactionReview
	document    RecipientPackageDocument
	text        []byte
}

func (s *Service) admitRecipientPackage(ctx context.Context, request RedactionRenderRequest) (result recipientPackageInputs, err error) {
	a, err := s.catalog.BatesAllocation(ctx, request.AllocationID.String())
	if errors.Is(err, store.ErrNotFound) {
		return result, ErrNumberedAllocationNotFound
	}
	if err != nil {
		return result, err
	}
	artifact, err := s.catalog.BatesArtifact(ctx, a.AllocationID)
	if errors.Is(err, store.ErrNotFound) {
		return result, ErrRecipientPackageNotPublished
	}
	if err != nil {
		return result, err
	}
	ns, err := s.catalog.BatesNamespace(ctx, a.NamespaceID, "", "", 0)
	if err != nil {
		return result, err
	}
	r, err := CheckBatesArtifact(artifact, a, ns)
	if err != nil {
		return result, err
	}
	if r == nil {
		return result, fmt.Errorf("%w: select a completed numbered redaction", ErrRecipientPackageInvalid)
	}
	if err := r.Recipe.Qualify(); err != nil {
		return result, err
	}
	review, err := MatchRedactionReview(request.Preparation, request.Masks,
		request.Preparation.Selection.Revision, request.ReviewSHA256)
	if err != nil {
		return result, err
	}
	if err := r.Recipe.MatchReview(review); err != nil {
		return result, err
	}
	textSize := int64(review.Resolved.PageCount)
	for _, run := range review.Resolved.Runs {
		textSize += int64(len(run.Text))
		if textSize > MaxRecipientPackageTextBytes {
			return result, ErrRecipientPackageLimit
		}
	}
	text, err := redaction.Text(review.Resolved)
	if err != nil {
		return result, err
	}
	labels := r.Recipe.Labels()
	document := RecipientPackageDocument{Ordinal: 1, DocumentID: labels[0], AllocationID: a.AllocationID,
		NamespaceID: a.NamespaceID, ArtifactManifestSHA256: artifact.ManifestSHA256,
		RecipeSHA256: artifact.RecipeSHA256, ReviewSHA256: review.SHA256,
		PageCount: artifact.PageCount, Labels: labels,
		PDF:  RecipientPackageFile{Path: "VOL001/PDF/000001.pdf", Size: artifact.Size, SHA256: artifact.BlobSHA256},
		Text: recipientFile("VOL001/TEXT/000001.txt", text)}
	if err := ValidateRecipientDocuments([]RecipientPackageDocument{document}); err != nil {
		return result, fmt.Errorf("%w: %v", ErrRecipientPackageLimit, err)
	}
	return recipientPackageInputs{reservation: *r, artifact: artifact, review: review, document: document, text: text}, nil
}
