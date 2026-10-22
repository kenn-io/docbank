package processing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
)

type RedactionSender func(context.Context, io.Reader, RedactionArchive) error

type nativeRedactionLimits struct{ png, pdf, archive int64 }

var defaultNativeRedactionLimits = nativeRedactionLimits{png: 1 << 30, pdf: 512 << 20, archive: MaxRedactionArchiveBytes}

type nativeRedactionHooks struct {
	beforeVerify  func(*os.File) error
	afterVerify   func(string) error
	beforePackage func(string) error
}
type nativeRedactionControls struct {
	limits nativeRedactionLimits
	hooks  nativeRedactionHooks
}

func (service *Service) RenderNativePDF(ctx context.Context, request RedactionRenderRequest,
	runtime *pagerender.Runtime, send RedactionSender,
) error {
	return service.renderNativePDF(ctx, request, runtime, send, nativeRedactionControls{limits: defaultNativeRedactionLimits})
}

func (service *Service) renderNativePDF(ctx context.Context, request RedactionRenderRequest,
	runtime *pagerender.Runtime, send RedactionSender, controls nativeRedactionControls,
) (resultErr error) {
	if send == nil || !canonical.IsSHA256Hex(request.ReviewSHA256) {
		return ErrRedactionInvalidRequest
	}
	if err := ValidateRedactionMasks(request.Masks); err != nil {
		return err
	}
	if service == nil || runtime == nil || !pagerender.SupportedPlatform() {
		return ErrRedactionPreparationUnavailable
	}
	op, err := service.beginNativePDFOperation(ctx, "render")
	if err != nil {
		return err
	}
	delivered := false
	finish := func(err error) {
		if delivered {
			if err != nil {
				slog.WarnContext(ctx, "redaction delivered but cleanup failed", "error", err)
			}
		} else {
			resultErr = errors.Join(resultErr, err)
		}
	}
	defer func() { finish(op.Close()) }()
	admitted, err := service.freshRedactionReview(op.ctx, op, RedactionResolveRequest{Preparation: request.Preparation, Masks: request.Masks}, runtime)
	if err != nil {
		return err
	}
	defer clear(admitted.data)
	if admitted.review.SHA256 != request.ReviewSHA256 {
		return ErrRedactionReviewChanged
	}
	pdf, err := renderRedactionPDF(op.ctx, op.directory, admitted, controls)
	if err != nil {
		return err
	}
	defer func() { finish(pdf.Close()) }()
	if controls.hooks.beforePackage != nil {
		if err := controls.hooks.beforePackage(op.directory); err != nil {
			return err
		}
	}
	archive, err := os.CreateTemp(op.directory, "redaction-*.zip")
	if err != nil {
		return err
	}
	defer func() { finish(archive.Close()) }()
	result, err := writeRedactionArchive(op.ctx, archive, pdf, admitted, controls.limits.archive)
	if err != nil {
		return err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := op.ctx.Err(); err != nil {
		return err
	}
	err = send(op.ctx, &contextReader{ctx: op.ctx, reader: archive}, result)
	delivered = err == nil
	return err
}

type renderedNativePDF struct {
	file   *os.File
	digest RedactionFileDigest
}

func (p renderedNativePDF) Close() error { return p.file.Close() }

func renderRedactionPDF(ctx context.Context, dir string, admitted admittedRedaction,
	controls nativeRedactionControls,
) (result renderedNativePDF, resultErr error) {
	pages, err := renderRedactionArtifacts(ctx, dir, admitted, controls.limits.png)
	if err != nil {
		return result, err
	}
	file, err := os.CreateTemp(dir, "redacted-*.pdf")
	if err != nil {
		return result, err
	}
	defer func() {
		if result.file == nil {
			resultErr = errors.Join(resultErr, file.Close())
		}
	}()
	plan := admitted.review.Resolved
	sequence := &redactionArtifactSequence{pages: pages}
	options := pdfproduction.PDFWriteOptions{Recipe: admitted.review.Recipe, StagingDirectory: dir, MaxBytes: controls.limits.pdf}
	if err := pdfproduction.WriteFresh(ctx, file, sequence, plan, options); err != nil {
		return result, redactionRenderFailure(err)
	}
	if controls.hooks.beforeVerify != nil {
		if err := controls.hooks.beforeVerify(file); err != nil {
			return result, err
		}
	}
	digest, err := redactionFileDigest(ctx, file)
	if err != nil {
		return result, err
	}
	verification := redactionVerification(dir, pages)
	verification.PDFSHA256 = digest.SHA256
	if err := pdfproduction.VerifyFresh(ctx, file, digest.Size, verification); err != nil {
		return result, fmt.Errorf("%w: %w", ErrRedactionPreparationIntegrity, err)
	}
	if controls.hooks.afterVerify != nil {
		if err := controls.hooks.afterVerify(dir); err != nil {
			return result, err
		}
	}
	for _, page := range pages {
		if err := os.Remove(page.path); err != nil {
			return result, fmt.Errorf("remove verified page artifact: %w", err)
		}
	}
	return renderedNativePDF{file: file, digest: digest}, nil
}

func redactionRenderFailure(err error) error {
	if errors.Is(err, pdfproduction.ErrOutputLimit) {
		return fmt.Errorf("%w: %w", ErrRedactionPreparationLimit, err)
	}
	return preparationFailure(err)
}
