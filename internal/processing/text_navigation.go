package processing

import (
	"context"
	"fmt"
	"io"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

// GetTextOutline derives a bounded outline from exact verified retained bytes.
func (service *Service) GetTextOutline(
	ctx context.Context, request document.TextOutlineRequest,
) (document.TextOutlinePage, error) {
	if err := document.ValidateTextOutlineRequest(request); err != nil {
		return document.TextOutlinePage{}, err
	}
	if err := service.navigationSlots.Acquire(ctx, 1); err != nil {
		return document.TextOutlinePage{}, err
	}
	defer service.navigationSlots.Release(1)
	data, err := service.readTextNavigation(ctx, request.Citation)
	if err != nil {
		return document.TextOutlinePage{}, err
	}
	index, err := buildTextSectionIndex(ctx, data, service.textParser())
	if err != nil {
		return document.TextOutlinePage{}, err
	}
	return index.outline(request)
}

// ReadTextSection returns exact code-point pages, including an explicit terminal page.
func (service *Service) ReadTextSection(
	ctx context.Context, request document.TextSectionRequest,
) (document.TextSectionPage, error) {
	if err := document.ValidateTextSectionRequest(request); err != nil {
		return document.TextSectionPage{}, err
	}
	if err := service.navigationSlots.Acquire(ctx, 1); err != nil {
		return document.TextSectionPage{}, err
	}
	defer service.navigationSlots.Release(1)
	data, err := service.readTextNavigation(ctx, request.Citation)
	if err != nil {
		return document.TextSectionPage{}, err
	}
	index, err := buildTextSectionIndex(ctx, data, service.textParser())
	if err != nil {
		return document.TextSectionPage{}, err
	}
	return index.page(ctx, data, request)
}

func (service *Service) readTextNavigation(
	ctx context.Context, citation document.TextCitation,
) ([]byte, error) {
	var data []byte
	err := service.gate.CaptureContext(ctx, func() error {
		artifact, err := service.catalog.RetainedTextCitation(ctx, citation)
		if err != nil {
			return err
		}
		data, err = readTextNavigationBlob(ctx, service.blobs, citation, artifact.Size)
		return err
	})
	return data, err
}

func readTextNavigationBlob(
	ctx context.Context, blobs verifiedBlobReader, citation document.TextCitation, size int64,
) ([]byte, error) {
	if size > document.MaxTextNavigationBytes {
		return nil, document.ErrTextNavigationLimit
	}
	stream, err := openCitationStream(ctx, blobs, citation, size)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: stream},
		document.MaxTextNavigationBytes+1))
	if readErr == nil && (int64(len(data)) != size || !utf8.Valid(data)) {
		readErr = fmt.Errorf("%w: retained Markdown size or UTF-8 differs", document.ErrCitationIntegrity)
	}
	if err := finishCitationStream(ctx, stream, readErr); err != nil {
		return nil, err
	}
	return data, nil
}
