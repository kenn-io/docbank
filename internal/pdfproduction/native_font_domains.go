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
