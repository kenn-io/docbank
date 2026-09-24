package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/safefileio"
)

// ProductionDraftPreviewStage owns private verified bytes until a download
// ticket consumes them. Close removes both files, including after a failed
// ticket publication.
type ProductionDraftPreviewStage struct {
	SetID              string
	Revision           int64
	ETag               int64
	MemberID           string
	Page               int
	PreviewInputSHA256 string
	ResolvedSHA256     string
	Image              *production.VerifiedProductionFile
	ImageSHA256        string
	ImageSize          int64
	Text               *production.VerifiedProductionFile
	TextSHA256         string
	TextSize           int64
}

func (s *ProductionDraftPreviewStage) Close() error {
	if s == nil {
		return nil
	}
	return errors.Join(s.Image.Close(), s.Text.Close())
}

// PrepareProductionDraftPreview joins catalog selection, complete source-byte
// verification and rendering before exposing a candidate image or text file.
// The caller passes the daemon's private, startup-swept staging directory.
func PrepareProductionDraftPreview(ctx context.Context, vault *store.Store,
	opener store.RenditionBlobReader, stageDir, setID string, revision, etag int64,
	memberID string, page int, engine pdfproduction.Engine) (result *ProductionDraftPreviewStage, resultErr error) {
	if ctx == nil || vault == nil || opener == nil || engine == nil ||
		stageDir == "" || !filepath.IsAbs(stageDir) {
		return nil, store.ErrInvalidProduction
	}
	if err := safefileio.EnsurePrivateDir(stageDir); err != nil {
		return nil, fmt.Errorf("securing production preview staging: %w", err)
	}
	input, err := vault.OpenProductionDraftPreviewSource(ctx, setID, revision, etag, memberID, opener)
	if err != nil {
		return nil, err
	}
	preview, err := production.RenderUnnumberedProductionPreviewPageInDir(ctx, input.PDF,
		input.Member, page, input.Recipe, engine, stageDir)
	if err != nil {
		return nil, err
	}
	result = &ProductionDraftPreviewStage{SetID: setID, Revision: revision, ETag: etag,
		MemberID: memberID, Page: page, PreviewInputSHA256: input.PreviewInputSHA256,
		ResolvedSHA256: preview.ResolvedSHA256, Image: preview.File,
		ImageSHA256: preview.PNGSHA256, ImageSize: preview.PNGSize,
		TextSHA256: preview.TextSHA256, TextSize: int64(len(preview.Text))}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, result.Close())
			result = nil
		}
	}()
	textFile, err := os.CreateTemp(stageDir, ".production-preview-*.txt")
	if err != nil {
		return result, err
	}
	result.Text = &production.VerifiedProductionFile{File: textFile}
	for remaining := preview.Text; len(remaining) > 0; {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		chunk := remaining[:min(len(remaining), 64<<10)]
		n, err := textFile.Write(chunk)
		if err != nil || n != len(chunk) {
			return result, errors.Join(io.ErrShortWrite, err)
		}
		remaining = remaining[n:]
	}
	if _, err := textFile.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	hasher := sha256.New()
	n, err := io.Copy(hasher, textFile)
	if err != nil || n != result.TextSize || hex.EncodeToString(hasher.Sum(nil)) != result.TextSHA256 {
		return result, errors.Join(production.ErrJobConflict, err)
	}
	if _, err := textFile.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}
