package pdfproduction

import (
	"context"
	"io"
	"slices"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/pagerender"
)

func nativeFontDomains(ctx context.Context, runtime *pagerender.Runtime, staged io.ReaderAt,
	source Source, frames []document.PageFrameV1) ([]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if runtime == nil || source.Size > document.MaxPageSourceBytes {
		return nil, nil
	}
	identity := frames[0].Source
	if identity.SHA256 != source.SHA256 || identity.Size != source.Size {
		return nil, nil
	}
	data, err := io.ReadAll(io.NewSectionReader(staged, 0, source.Size))
	if err != nil {
		return nil, err
	}
	defer clear(data)
	result, err := runtime.Inspect(ctx, data, identity, "application/pdf")
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || !slices.Equal(result.Frames, frames) {
		return nil, nil
	}
	domains := make([]bool, len(frames))
	for i, domain := range result.FontDomains {
		domains[i] = *domain.SimpleFontsOnly
	}
	return domains, nil
}

func (input nativeTextInput) fontDomains(ctx context.Context, staged io.ReaderAt) ([]bool, error) {
	if input.inspection == nil {
		return nativeFontDomains(ctx, input.runtime, staged, input.source, input.frames)
	}
	inspection := *input.inspection
	if inspection.Source.SHA256 != input.source.SHA256 || inspection.Source.Size != input.source.Size {
		return nil, pagerender.ErrInvalidOutput
	}
	if err := pagerender.ValidateInspection(inspection, inspection.Source, "application/pdf"); err != nil {
		return nil, err
	}
	domains := make([]bool, len(inspection.FontDomains))
	for i, domain := range inspection.FontDomains {
		domains[i] = *domain.SimpleFontsOnly
	}
	return domains, nil
}
