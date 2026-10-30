package processing

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"
	"uuid"

	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/internal/canonical"
)

// CreateRedactionPreview renders once. publish fences publication against browser
// session revocation; API-key callers may leave it nil. The directory belongs to
// the operation until that fence succeeds.
func (s *Service) CreateRedactionPreview(ctx context.Context, owner string, request RedactionRenderRequest,
	runtime *pagerender.Runtime, publish func(func() error) error,
) (RedactionPreview, error) {
	return s.createRedactionPreview(ctx, owner, request, runtime, publish,
		nativeRedactionControls{limits: defaultNativeRedactionLimits, retainPages: true})
}

func (s *Service) createRedactionPreview(ctx context.Context, owner string, request RedactionRenderRequest,
	runtime *pagerender.Runtime, publish func(func() error) error, controls nativeRedactionControls,
) (result RedactionPreview, resultErr error) {
	if owner == "" || request.AllocationID != uuid.Nil() || !canonical.IsSHA256Hex(request.ReviewSHA256) {
		return result, ErrRedactionInvalidRequest
	}
	if err := ValidateRedactionMasks(request.Masks); err != nil {
		return result, err
	}
	if s == nil || runtime == nil || !pagerender.SupportedPlatform() {
		return result, ErrRedactionPreparationUnavailable
	}
	e, err := s.reserveRedactionPreview(owner)
	if err != nil {
		return result, err
	}
	workCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(e.ctx, cancel)
	defer func() {
		stop()
		cancel()
		s.redactionPreviews.mu.Lock()
		defer s.redactionPreviews.mu.Unlock()
		if !e.ready {
			e.retired = true
		}
		e.pins-- // Creation owns the entry until every failure path has cleaned up.
		resultErr = errors.Join(resultErr, s.cleanRedactionPreviewLocked())
	}()
	op, err := s.beginNativePDFOperation(workCtx, "preview")
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, op.Close()) }()
	admitted, pdf, err := s.prepareRedactionRender(op.ctx, op, request, runtime, controls)
	if err != nil {
		return result, err
	}
	defer clear(admitted.data)
	archive, manifest, offset, err := assembleRedactionPreview(op.ctx, op.directory, pdf, admitted, controls)
	if err != nil {
		return result, err
	}
	manifest.ID = e.manifest.ID
	manifest.ExpiresAt = s.clock().UTC().Add(redactionPreviewLifetime)
	encoded, err := canonical.Marshal(manifest)
	if err != nil {
		return result, err
	}
	if len(encoded) > 256<<10 {
		return result, ErrRedactionPreparationLimit
	}
	commit := func() error {
		s.redactionPreviews.mu.Lock()
		defer s.redactionPreviews.mu.Unlock()
		if err := op.ctx.Err(); err != nil {
			return err
		}
		if e.retired || e.ctx.Err() != nil {
			return ErrRedactionPreviewUnavailable
		}
		e.manifest, e.archivePath, e.textOffset = manifest, archive, offset
		for _, page := range pdf.pages {
			e.pagePaths = append(e.pagePaths, page.path)
		}
		e.directory, op.directory = op.directory, ""
		e.ready = true
		e.timer = time.AfterFunc(redactionPreviewLifetime, func() {
			s.redactionPreviews.mu.Lock()
			defer s.redactionPreviews.mu.Unlock()
			if s.redactionPreviews.entry != e {
				return
			}
			e.retired = true
			if err := s.cleanRedactionPreviewLocked(); err != nil {
				slog.Error("removing expired redaction preview", "error", err)
			}
		})
		return nil
	}
	if publish == nil {
		err = commit()
	} else {
		err = publish(commit)
	}
	if err != nil {
		return result, err
	}
	return manifest, nil
}

func (s *Service) reserveRedactionPreview(owner string) (*redactionPreviewEntry, error) {
	s.redactionPreviews.mu.Lock()
	defer s.redactionPreviews.mu.Unlock()
	if err := s.expireRedactionPreviewLocked(); err != nil {
		return nil, err
	}
	if s.redactionPreviews.entry != nil {
		return nil, ErrRedactionPreviewBusy
	}
	ctx, finish, err := s.beginRun(s.lifecycle)
	if err != nil {
		return nil, ErrRedactionPreparationUnavailable
	}
	ctx, cancel := context.WithCancel(ctx)
	var id [24]byte
	_, _ = rand.Read(id[:])
	e := &redactionPreviewEntry{owner: owner, ctx: ctx, cancel: cancel, finish: finish, pins: 1,
		manifest: RedactionPreview{ID: hex.EncodeToString(id[:])}}
	s.redactionPreviews.entry = e
	return e, nil
}

func assembleRedactionPreview(ctx context.Context, dir string, pdf renderedNativePDF,
	admitted admittedRedaction, controls nativeRedactionControls,
) (path string, manifest RedactionPreview, textOffset int64, resultErr error) {
	defer func() {
		if pdf.file != nil {
			resultErr = errors.Join(resultErr, pdf.Close())
		}
	}()
	archive, err := os.CreateTemp(dir, "redaction-*.zip")
	if err != nil {
		return "", manifest, 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, archive.Close()) }()
	result, err := writeRedactionArchive(ctx, archive, pdf, admitted, controls.limits.archive)
	if err != nil {
		return "", manifest, 0, err
	}
	index, err := zip.NewReader(archive, result.Size)
	if err != nil {
		return "", manifest, 0, fmt.Errorf("indexing preview ZIP: %w", err)
	}
	textOffset, err = index.File[1].DataOffset()
	if err != nil {
		return "", manifest, 0, fmt.Errorf("locating preview text: %w", err)
	}
	manifest = RedactionPreview{Contract: "native-pdf-redaction-preview/v1", Archive: result}
	start := 0
	for _, p := range pdf.pages {
		page := p.artifact.Page
		manifest.Pages = append(manifest.Pages, RedactionPreviewPage{Number: page.Number,
			FrameSHA256: page.FrameSHA256, Width: page.Width, Height: page.Height,
			PixelWidth: p.pixelWidth, PixelHeight: p.pixelHeight,
			PNG:       RedactionFileDigest{Size: p.artifact.PNGSize, SHA256: p.artifact.PNGSHA256},
			TextStart: start, TextEnd: start + len(p.text)})
		start += len(p.text) + 1
	}
	// Close before unlink for Windows; the deferred Close is replaced with a
	// no-op by clearing ownership below.
	if err := pdf.file.Close(); err != nil {
		return "", manifest, 0, err
	}
	if err := os.Remove(pdf.file.Name()); err != nil {
		return "", manifest, 0, err
	}
	pdf.file = nil
	return archive.Name(), manifest, textOffset, nil
}
