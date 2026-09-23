// Package pdfproduction renders untrusted PDFs in a capability-free WASM runtime
// and writes fresh PDFs from sanitized page artifacts. Physical coordinates
// are integer inch/10000; font sizes are explicitly named millipoints.
package pdfproduction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"os"
	"time"

	"github.com/shirou/gopsutil/v4/process"

	"go.kenn.io/docbank/document/redaction"
)

type Source struct {
	Reader io.ReaderAt
	Size   int64
	SHA256 string
}
type Raster struct {
	burned *burnProof
	Pixels *image.NRGBA
	Page   redaction.Page
	DPI    int
}
type PageArtifact struct {
	masks                                                       []redaction.Box
	bound                                                       bool
	Page                                                        redaction.Page
	PNGSHA256, ResolvedSHA256, EndorsementsSHA256, LayoutSHA256 string
	PNGSize                                                     int64
	OpenPNG                                                     func() (io.ReadCloser, error)
	Runs                                                        []redaction.Run
	Layout                                                      redaction.PageLayout
	Endorsements                                                []redaction.Endorsement
}
type PageSequence interface {
	Next(ctx context.Context) (PageArtifact, error)
}
type VerificationInput struct {
	Pages                            []redaction.Page
	NextText                         func(context.Context) (int, []byte, error)
	NextPageArtifact                 func(context.Context) (PageArtifact, error)
	ResolvedSHA256                   []string
	NextEndorsements                 func(context.Context) (redaction.PageLayout, []redaction.Endorsement, error)
	EndorsementsSHA256, LayoutSHA256 []string
}
type Engine interface {
	Render(ctx context.Context, source Source, page redaction.Page, recipe redaction.Recipe) (Raster, error)
	Close() error
}

const maxPayloadBytes int64 = 50 << 30

// The pinned WASM ABI uses i32 for FPDF_FILEACCESS_Create's file length.
// Keep this distinct from the aggregate export payload budget.
const maxPDFFileBytes int64 = math.MaxUint32
const maxPageCount = 100000
const wasmSHA256 = "f651270c675cac90702b762f4b95d2b34e365cdb374065e40af016f4f0f304ea"
const fontSHA256 = "c2f3b4d463500a2ddcd3849cded1fceeb9fd6d1c32e6cbecd568453ba50fc68f"
const writerVersion = "docbank-fresh-objects/v2"

func defaultRecipe() redaction.Recipe {
	return redaction.Recipe{Contract: "raster-redaction/v1", RendererSHA256: wasmSHA256,
		WriterVersion: writerVersion, FontSHA256: fontSHA256, DPI: 300, PaddingPixels: 2, MaxPixels: 40_000_000, MaxAxis: 16384,
		WASMMemoryBytes: 512 << 20, PageTimeoutSeconds: 60, QualifiedPeakRSSBytes: 1 << 30, MaxStagingBytes: 100 << 30}
}

// QualifiedRecipe returns the pinned local production recipe.
func QualifiedRecipe() redaction.Recipe { return defaultRecipe() }

// QualifiedRecipeForDPI returns one immutable catalog recipe. Both supported
// resolutions bind every renderer, writer and font dependency; callers cannot
// supply or omit those identities independently.
func QualifiedRecipeForDPI(dpi int) (redaction.Recipe, error) {
	if dpi != 300 && dpi != 600 {
		return redaction.Recipe{}, errors.New("PDF production DPI is not qualified")
	}
	recipe := defaultRecipe()
	recipe.DPI = dpi
	return recipe, nil
}

// QualifiedImageWrapperRecipeSHA256 identifies every output-affecting
// dependency of the deterministic image-only PDF wrapper. Bump
// writerVersion whenever its byte serialization changes.
func QualifiedImageWrapperRecipeSHA256() string {
	value := []byte("image-only-pdf-wrapper/v1\x00" + writerVersion + "\x00" + fontSHA256)
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func validateRecipe(r redaction.Recipe) error {
	if r.WriterVersion != writerVersion || r.RendererSHA256 != wasmSHA256 || r.FontSHA256 != fontSHA256 || r.PaddingPixels != 2 {
		return errors.New("PDF production recipe dependency identity mismatch")
	}
	if r.Contract != "raster-redaction/v1" || r.DPI != 300 && r.DPI != 600 || r.PaddingPixels < 0 || r.PaddingPixels > 16384 ||
		r.MaxPixels <= 0 || r.MaxPixels > 40_000_000 || r.MaxAxis <= 0 || r.MaxAxis > 16384 ||
		r.WASMMemoryBytes <= 0 || r.WASMMemoryBytes > 512<<20 || r.PageTimeoutSeconds <= 0 || r.PageTimeoutSeconds > 60 ||
		r.QualifiedPeakRSSBytes <= 0 || r.QualifiedPeakRSSBytes > 1<<30 || r.MaxStagingBytes <= 0 || r.MaxStagingBytes > 100<<30 {
		return errors.New("PDF production recipe exceeds qualified limits")
	}
	return nil
}

func checkRSS(ctx context.Context, limit int64) error {
	pid := os.Getpid()
	if pid < 0 || pid > math.MaxInt32 || limit < 1 {
		return errors.New("invalid PDF worker accounting limits")
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return fmt.Errorf("open worker memory accounting: %w", err)
	}
	info, err := p.MemoryInfoWithContext(ctx)
	if err != nil {
		return fmt.Errorf("read worker RSS: %w", err)
	}
	if info.RSS > uint64(limit) {
		return errors.New("PDF worker exceeds qualified RSS ceiling")
	}
	return nil
}

// watchRSS covers the whole Go/WASM worker, including decoding, compression,
// font data, compiled code and indexes. A breached ceiling cancels WASM as well
// as the bounded Go streaming loops; accounting failure is fatal.
func watchRSS(parent context.Context, limit int64) (context.Context, func(), error) {
	if err := checkRSS(parent, limit); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancelCause(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := checkRSS(ctx, limit); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(nil); <-done }, nil
}

func dimensions(p redaction.Page, r redaction.Recipe) (int, int, error) {
	if p.Number < 1 || p.Number > maxPageCount || !validHash(p.FrameSHA256) || p.Width <= 0 || p.Height <= 0 ||
		p.Width > (math.MaxInt64-9999)/int64(r.DPI) || p.Height > (math.MaxInt64-9999)/int64(r.DPI) {
		return 0, 0, errors.New("invalid or overflowing PDF page dimensions")
	}
	w, h := (p.Width*int64(r.DPI)+9999)/10000, (p.Height*int64(r.DPI)+9999)/10000
	if w > r.MaxAxis || h > r.MaxAxis || w > r.MaxPixels/h {
		return 0, 0, errors.New("PDF page exceeds axis or pixel limit")
	}
	return int(w), int(h), nil
}

func hashBytes(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func validHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

type frozenPDF struct {
	file   *os.File
	size   int64
	sha256 string
}

// freezeFresh reads the caller exactly once, hashing while copying into a
// private, bounded file. Verification receives only its read-only handle.
// The caller's mutable reader is never consulted again. The later publisher
// must likewise own and publish the exact immutable staged artifact it submits
// to VerifyFresh, bound by size/digest, not reopen a mutable original afterward.
func freezeFresh(ctx context.Context, reader io.ReaderAt, size, budget int64) (snapshot *frozenPDF, result error) {
	if reader == nil || size <= 0 || size > min(maxPDFFileBytes, maxPayloadBytes, budget) {
		return nil, errors.New("invalid final PDF snapshot bounds")
	}
	file, err := os.CreateTemp("", "docbank-verify-*.pdf")
	if err != nil {
		return nil, err
	}
	name := file.Name()
	defer func() {
		if result != nil {
			result = errors.Join(result, os.Remove(name))
		}
	}()
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(file, hash), contextReader{ctx, io.NewSectionReader(reader, 0, size)})
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return nil, err
	}
	if n != size {
		return nil, errors.New("truncated final PDF snapshot")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	readOnly, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return &frozenPDF{file: readOnly, size: n, sha256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func (s *frozenPDF) Close() error { return errors.Join(s.file.Close(), os.Remove(s.file.Name())) }

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func stageSource(ctx context.Context, s Source, budget int64) (file *os.File, result error) {
	if s.Size > maxPDFFileBytes {
		return nil, errors.New("PDF exceeds pinned WASM 32-bit file length limit")
	}
	if s.Reader == nil || s.Size <= 0 || s.Size > maxPayloadBytes || !validHash(s.SHA256) {
		return nil, errors.New("invalid PDF source identity or size")
	}
	if s.Size > budget {
		return nil, errors.New("PDF source exceeds private staging budget")
	}
	file, err := os.CreateTemp("", "docbank-source-*.pdf")
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, file.Close(), os.Remove(file.Name()))
			file = nil
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, h), contextReader{ctx, io.NewSectionReader(s.Reader, 0, s.Size)})
	if err != nil {
		return file, fmt.Errorf("stage and hash PDF source: %w", err)
	}
	if n != s.Size || hex.EncodeToString(h.Sum(nil)) != s.SHA256 {
		return file, errors.New("PDF source hash mismatch")
	}
	return file, nil
}
