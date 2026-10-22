package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image/png"
	"io"
	"os"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
)

type redactionArtifact struct {
	artifact pdfproduction.PageArtifact
	path     string
	text     []byte
}

type redactionArtifactSequence struct {
	pages []redactionArtifact
	index int
}

func (s *redactionArtifactSequence) Next(ctx context.Context) (pdfproduction.PageArtifact, error) {
	if err := ctx.Err(); err != nil {
		return pdfproduction.PageArtifact{}, err
	}
	if s.index == len(s.pages) {
		return pdfproduction.PageArtifact{}, io.EOF
	}
	artifact := s.pages[s.index].artifact
	s.index++
	return artifact, nil
}

func renderRedactionArtifacts(ctx context.Context, dir string, admitted admittedRedaction,
	maxPNG int64,
) (result []redactionArtifact, resultErr error) {
	plan := admitted.review.Resolved
	// Bound text before any writer or image work; form feeds come from Text later.
	textBytes := make([]int, plan.PageCount)
	for _, run := range plan.Runs {
		textBytes[run.Page-1] += len(run.Text)
		if textBytes[run.Page-1] > 1<<20 {
			return nil, ErrRedactionPreparationLimit
		}
	}
	engine, err := pdfproduction.NewPDFium(admitted.review.Recipe)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, engine.Close()) }()
	source := pdfproduction.Source{Reader: bytes.NewReader(admitted.data), Size: int64(len(admitted.data)), SHA256: admitted.review.Selection.Source.SHA256, StagingDirectory: dir}
	for _, page := range plan.Pages {
		artifact, err := renderRedactionPage(ctx, engine, source, admitted.review, page, maxPNG)
		if err != nil {
			return nil, redactionRenderFailure(err)
		}
		maxPNG -= artifact.artifact.PNGSize
		result = append(result, artifact)
	}
	return result, nil
}

func renderRedactionPage(ctx context.Context, engine pdfproduction.Engine, source pdfproduction.Source,
	review RedactionReview, page redaction.Page, maxPNG int64,
) (result redactionArtifact, resultErr error) {
	raster, err := engine.Render(ctx, source, page, review.Recipe)
	if err != nil {
		return result, err
	}
	var boxes []redaction.Box
	for _, box := range review.Resolved.RedactBoxes {
		if box.Page == page.Number {
			boxes = append(boxes, box)
		}
	}
	burned, err := pdfproduction.Burn(raster, boxes, review.Recipe)
	if err != nil {
		return result, err
	}
	file, err := os.CreateTemp(source.StagingDirectory, "page-*.png")
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	hash := sha256.New()
	writer := &redactionBoundedWriter{ctx: ctx, out: io.MultiWriter(file, hash), remaining: maxPNG}
	if err := png.Encode(writer, burned.Pixels); err != nil {
		return result, fmt.Errorf("encoding redaction page: %w", err)
	}
	layout := redaction.PageLayout{Source: page, Output: page}
	layoutBytes, err := canonical.Marshal(layout)
	if err != nil {
		return result, err
	}
	endorsements := []redaction.Endorsement{}
	endorsementBytes, err := canonical.Marshal(endorsements)
	if err != nil {
		return result, err
	}
	var runs []redaction.Run
	var text []byte
	for _, run := range review.Resolved.Runs {
		if run.Page == page.Number {
			runs = append(runs, run)
			text = append(text, run.Text...)
		}
	}
	path := file.Name()
	return redactionArtifact{path: path, text: text, artifact: pdfproduction.PageArtifact{
		Page: page, Layout: layout, LayoutSHA256: sha256HexBytes(layoutBytes), Endorsements: endorsements,
		EndorsementsSHA256: sha256HexBytes(endorsementBytes), ResolvedSHA256: review.Resolved.SHA256, Runs: runs,
		PNGSize: maxPNG - writer.remaining, PNGSHA256: hex.EncodeToString(hash.Sum(nil)),
		OpenPNG: func() (io.ReadCloser, error) { return os.Open(path) },
	}}, nil
}

func redactionVerification(dir string, pages []redactionArtifact) pdfproduction.VerificationInput {
	sequence := &redactionArtifactSequence{pages: pages}
	input := pdfproduction.VerificationInput{StagingDirectory: dir, NextPageArtifact: sequence.Next}
	for _, page := range pages {
		input.Pages = append(input.Pages, page.artifact.Page)
		input.ResolvedSHA256 = append(input.ResolvedSHA256, page.artifact.ResolvedSHA256)
		input.LayoutSHA256 = append(input.LayoutSHA256, page.artifact.LayoutSHA256)
		input.EndorsementsSHA256 = append(input.EndorsementsSHA256, page.artifact.EndorsementsSHA256)
	}
	textIndex, layoutIndex := 0, 0
	input.NextText = func(ctx context.Context) (int, []byte, error) {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		if textIndex == len(pages) {
			return 0, nil, io.EOF
		}
		p := pages[textIndex]
		textIndex++
		return p.artifact.Page.Number, p.text, nil
	}
	input.NextEndorsements = func(ctx context.Context) (redaction.PageLayout, []redaction.Endorsement, error) {
		if err := ctx.Err(); err != nil {
			return redaction.PageLayout{}, nil, err
		}
		if layoutIndex == len(pages) {
			return redaction.PageLayout{}, nil, io.EOF
		}
		p := pages[layoutIndex].artifact
		layoutIndex++
		return p.Layout, p.Endorsements, nil
	}
	return input
}

type redactionBoundedWriter struct {
	ctx       context.Context
	out       io.Writer
	remaining int64
}

func (w *redactionBoundedWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.remaining {
		return 0, ErrRedactionPreparationLimit
	}
	n, err := w.out.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func redactionFileDigest(ctx context.Context, file *os.File) (RedactionFileDigest, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return RedactionFileDigest{}, err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, &contextReader{ctx: ctx, reader: file})
	if err != nil {
		return RedactionFileDigest{}, err
	}
	return RedactionFileDigest{Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}
