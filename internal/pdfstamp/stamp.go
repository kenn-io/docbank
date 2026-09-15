package pdfstamp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/font"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

const maxStampOutputBytes int64 = 512 << 20

var ErrStampEngineFailure = errors.New("stamp_engine_failure: the PDF engine could not stamp a page")

type PageLabel struct {
	SourcePage int
	Label      string
}

type Result struct {
	SHA256    string
	Size      int64
	PageCount int
	Pages     []PageLabel
}

type limitedStampWriter struct {
	Writer    io.Writer
	Remaining int64
}

func (w *limitedStampWriter) Write(value []byte) (int, error) {
	if int64(len(value)) > w.Remaining {
		return 0, ErrStampEngineFailure
	}
	written, err := w.Writer.Write(value)
	w.Remaining -= int64(written)
	if err == nil && written != len(value) {
		err = io.ErrShortWrite
	}
	return written, err
}

// Stamp performs the qualified synchronous PDF transformation. Daemon and export
// callers must run it in their bounded, supervised worker process.
func Stamp(ctx context.Context, source io.ReadSeeker, labels []PageLabel, recipe Recipe, output io.Writer) (Result, error) {
	var zero Result
	if ctx == nil {
		return zero, stampFailure("validate context", errors.New("nil context"))
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if source == nil || output == nil {
		return zero, stampFailure("validate streams", errors.New("nil source or destination"))
	}
	recipe = recipe.normalized()
	if err := recipe.Validate(); err != nil {
		return zero, stampFailure("validate recipe", err)
	}

	pageCount, dimensions, err := inspectSource(source, recipe.Restamp)
	if err != nil {
		return zero, err
	}
	if err := validateLabels(labels, recipe, pageCount, dimensions); err != nil {
		return zero, err
	}
	watermarks, err := buildWatermarks(labels, recipe)
	if err != nil {
		return zero, err
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return zero, stampFailure("rewind source", err)
	}

	var staged bytes.Buffer
	bounded := &limitedStampWriter{Writer: &staged, Remaining: maxStampOutputBytes}
	if err := api.AddWatermarksMap(source, bounded, watermarks, nil); err != nil {
		return zero, stampFailure("stamp PDF", err)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	verifiedCount, err := api.PageCount(bytes.NewReader(staged.Bytes()), nil)
	if err != nil {
		return zero, stampFailure("verify output page count", err)
	}
	if verifiedCount != pageCount {
		return zero, stampFailure("verify output page count", fmt.Errorf("got %d pages, want %d", verifiedCount, pageCount))
	}
	if err := verifyStampedLabels(staged.Bytes(), labels); err != nil {
		return zero, err
	}

	written, err := output.Write(staged.Bytes())
	if err != nil {
		return zero, stampFailure("write verified output", err)
	}
	if written != staged.Len() {
		return zero, stampFailure("write verified output", io.ErrShortWrite)
	}
	digest := sha256.Sum256(staged.Bytes())
	return Result{
		SHA256: hex.EncodeToString(digest[:]), Size: int64(staged.Len()), PageCount: pageCount,
		Pages: slices.Clone(labels),
	}, nil
}

func verifyStampedLabels(pdf []byte, labels []PageLabel) error {
	pdfContext, err := api.ReadContext(bytes.NewReader(pdf), model.NewDefaultConfiguration())
	if err != nil {
		return stampFailure("verify output labels", err)
	}
	if err := api.ValidateContext(pdfContext); err != nil {
		return stampFailure("verify output labels", err)
	}
	if pdfContext.PageCount != len(labels) {
		return stampFailure("verify output labels", errors.New("page and label counts differ"))
	}
	for _, label := range labels {
		pageDict, _, attributes, err := pdfContext.PageDict(label.SourcePage, false)
		if err != nil {
			return stampFailure("verify output labels", fmt.Errorf("read page %d: %w", label.SourcePage, err))
		}
		pageContent, err := pdfContext.PageContent(pageDict, label.SourcePage)
		if err != nil {
			return stampFailure("verify output labels", fmt.Errorf("read page %d content: %w", label.SourcePage, err))
		}
		if !bytes.Contains(pageContent, []byte("/Artifact <</Subtype /Watermark /Type /Pagination >>BDC")) {
			return stampFailure("verify output labels", fmt.Errorf("page %d has no watermark artifact", label.SourcePage))
		}
		xObjects, err := pdfContext.DereferenceDict(attributes.Resources["XObject"])
		if err != nil {
			return stampFailure("verify output labels", fmt.Errorf("read page %d XObjects: %w", label.SourcePage, err))
		}
		escaped, err := types.Escape(label.Label)
		if err != nil {
			return stampFailure("verify output labels", fmt.Errorf("encode page %d label: %w", label.SourcePage, err))
		}
		labelToken := []byte("(" + *escaped + ") Tj")
		matches := 0
		for name, object := range xObjects {
			if !bytes.Contains(pageContent, []byte("/"+name+" Do")) {
				continue
			}
			stream, _, err := pdfContext.DereferenceStreamDict(object)
			if err != nil {
				return stampFailure("verify output labels", fmt.Errorf("read page %d XObject %s: %w", label.SourcePage, name, err))
			}
			if stream == nil {
				continue
			}
			if err := stream.DecodeWithLimit(4 << 20); err != nil {
				return stampFailure("verify output labels", fmt.Errorf("decode page %d XObject %s: %w", label.SourcePage, name, err))
			}
			if bytes.Contains(stream.Content, labelToken) {
				matches++
			}
		}
		if matches != 1 {
			return stampFailure("verify output labels", fmt.Errorf("page %d contains %d copies of its declared label", label.SourcePage, matches))
		}
	}
	return nil
}

func inspectSource(source io.ReadSeeker, allowRestamp bool) (int, []types.Dim, error) {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return 0, nil, stampFailure("rewind source", err)
	}
	pdfContext, err := api.ReadContext(source, model.NewDefaultConfiguration())
	if err != nil {
		return 0, nil, stampFailure("read source PDF", err)
	}
	if err := api.ValidateContext(pdfContext); err != nil {
		return 0, nil, stampFailure("validate source PDF", err)
	}
	count := pdfContext.PageCount
	if count < 1 {
		return 0, nil, stampFailure("read source page count", errors.New("PDF has no pages"))
	}
	boundaries, err := pdfContext.PageBoundaries(nil)
	if err != nil {
		return 0, nil, stampFailure("read source page boundaries", err)
	}
	if len(boundaries) != count {
		return 0, nil, stampFailure("read source page boundaries", errors.New("page boundary count mismatch"))
	}
	dimensions := make([]types.Dim, count)
	for index, boundary := range boundaries {
		cropBox := boundary.CropBox()
		if cropBox == nil {
			return 0, nil, stampFailure("read source page boundaries", fmt.Errorf("page %d has no effective CropBox", index+1))
		}
		dimensions[index] = cropBox.Dimensions()
		if boundary.Rot%180 != 0 {
			dimensions[index].Width, dimensions[index].Height = dimensions[index].Height, dimensions[index].Width
		}
	}
	if !allowRestamp {
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			return 0, nil, stampFailure("rewind source", err)
		}
		hasWatermarks, err := api.HasWatermarks(source, nil)
		if err != nil {
			return 0, nil, stampFailure("inspect source watermarks", err)
		}
		if hasWatermarks {
			return 0, nil, stampFailure("inspect source watermarks", errors.New("source already contains a watermark"))
		}
	}
	return count, dimensions, nil
}

func validateLabels(labels []PageLabel, recipe Recipe, pageCount int, dimensions []types.Dim) error {
	if len(labels) != pageCount {
		return stampFailure("validate page labels", fmt.Errorf("got %d labels for %d pages", len(labels), pageCount))
	}
	maxSequence := int64(1)
	for range recipe.Padding {
		maxSequence *= 10
	}
	maxSequence--
	endSequence := int64(recipe.StartAt) + int64(pageCount) - 1
	if endSequence > maxSequence {
		return stampFailure("validate page labels", errors.New("assigned sequence exceeds recipe padding"))
	}
	for index, label := range labels {
		page := index + 1
		if label.SourcePage != page {
			return stampFailure("validate page labels", fmt.Errorf("label %d targets page %d", index, label.SourcePage))
		}
		expected := fmt.Sprintf("%s%0*d%s", recipe.Prefix, recipe.Padding, recipe.StartAt+index, recipe.Suffix)
		if label.Label != expected {
			return stampFailure("validate page labels", fmt.Errorf("page %d label differs from the assigned recipe sequence", page))
		}
		width, err := font.TextWidthFloat(label.Label, recipe.FontName, float64(recipe.FontSizePoints))
		if err != nil {
			return stampFailure("measure page label", err)
		}
		minimumWidth := width + 2*float64(recipe.MarginPoints)
		minimumHeight := float64(recipe.FontSizePoints) + 2*float64(recipe.MarginPoints)
		if dimensions[index].Width < minimumWidth || dimensions[index].Height < minimumHeight {
			return stampFailure("validate stamp geometry", fmt.Errorf("page %d is too small for its label and margin", page))
		}
	}
	return nil
}

func buildWatermarks(labels []PageLabel, recipe Recipe) (map[int]*model.Watermark, error) {
	position, offsetX, offsetY := pdfcpuPosition(recipe.Position, recipe.MarginPoints)
	description := fmt.Sprintf(
		"fontname:%s, points:%d, position:%s, offset:%d %d, scalefactor:1 abs, color:%s, opacity:%g, rotation:0",
		recipe.FontName, recipe.FontSizePoints, position, offsetX, offsetY, recipe.Color, recipe.Opacity,
	)
	watermarks := make(map[int]*model.Watermark, len(labels))
	for _, label := range labels {
		watermark, err := api.TextWatermark(label.Label, description, true, false, types.POINTS)
		if err != nil {
			return nil, stampFailure("create page watermark", err)
		}
		watermarks[label.SourcePage] = watermark
	}
	return watermarks, nil
}

func pdfcpuPosition(position string, margin int) (string, int, int) {
	positions := map[string]string{
		"top-left": "tl", "top-center": "tc", "top-right": "tr",
		"middle-left": "l", "middle-center": "c", "middle-right": "r",
		"bottom-left": "bl", "bottom-center": "bc", "bottom-right": "br",
	}
	x, y := 0, 0
	if position == "top-left" || position == "middle-left" || position == "bottom-left" {
		x = margin
	}
	if position == "top-right" || position == "middle-right" || position == "bottom-right" {
		x = -margin
	}
	if position == "top-left" || position == "top-center" || position == "top-right" {
		y = -margin
	}
	if position == "bottom-left" || position == "bottom-center" || position == "bottom-right" {
		y = margin
	}
	return positions[position], x, y
}

func stampFailure(operation string, cause error) error {
	return fmt.Errorf("%w: %s: %w", ErrStampEngineFailure, operation, cause)
}
