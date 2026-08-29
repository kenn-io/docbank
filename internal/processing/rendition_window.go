package processing

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/pack"
)

var (
	ErrInvalidRenditionWindow   = errors.New("rendition text window is invalid")
	ErrInvalidRenditionEncoding = errors.New("rendition text is not valid UTF-8")
)

type RenditionWindowRequest struct {
	VaultUID         string
	NodeID           int64
	ContentVersionID string
	AttachmentID     string
	Offset           int
	MaxChars         int
}

type RenditionTextWindow struct {
	VaultUID           string
	NodeID             int64
	ContentVersionID   string
	AttachmentID       string
	BuildID            string
	ProfileFingerprint string
	Text               string
	MediaType          string
	Checksum           string
	RequestedOffset    int
	ActualStart        int
	ActualEnd          int
	NextOffset         int
	EOF                bool
	ResponseBytes      int
}

// RenditionTextWindow reads one bounded Unicode window only after the daemon
// has revalidated the exact current/live rendition tuple.
func (service *Service) RenditionTextWindow(
	ctx context.Context, request RenditionWindowRequest,
) (RenditionTextWindow, error) {
	if service == nil || service.catalog == nil || service.blobs == nil ||
		request.VaultUID != service.catalog.VaultID() || request.NodeID < 1 ||
		request.ContentVersionID == "" || request.AttachmentID == "" || request.Offset < 0 {
		return RenditionTextWindow{}, store.ErrNotFound
	}
	if request.MaxChars == 0 {
		request.MaxChars = 8_000
	}
	if request.MaxChars < 1 || request.MaxChars > 16_000 {
		return RenditionTextWindow{}, ErrInvalidRenditionWindow
	}
	view, err := service.catalog.ActiveRenditionByAttachment(ctx, request.AttachmentID)
	if err != nil {
		return RenditionTextWindow{}, err
	}
	version, err := service.catalog.ContentVersionByID(ctx, view.Attachment.ContentVersionID)
	if err != nil {
		return RenditionTextWindow{}, err
	}
	node, err := service.catalog.NodeByID(ctx, version.NodeID)
	if err != nil {
		return RenditionTextWindow{}, err
	}
	if node.ID != request.NodeID || version.ID != request.ContentVersionID ||
		view.Attachment.ID != request.AttachmentID || node.CurrentVersionID != version.ID || node.TrashedAt != nil {
		return RenditionTextWindow{}, store.ErrNotFound
	}
	var artifact store.RenditionArtifactRecord
	for _, candidate := range view.Build.Artifacts {
		if candidate.Role == "sanitized_markdown" {
			artifact = candidate
			break
		}
	}
	if artifact.ID == "" || artifact.Size < 0 {
		return RenditionTextWindow{}, store.ErrNotFound
	}
	text, actualEnd, eof, err := readRenditionBlobWindow(
		ctx, service.blobs, artifact.BlobHash, artifact.Size, request.Offset, request.MaxChars,
	)
	if err != nil {
		return RenditionTextWindow{}, err
	}
	return RenditionTextWindow{
		VaultUID: service.catalog.VaultID(), NodeID: node.ID, ContentVersionID: version.ID,
		AttachmentID: view.Attachment.ID, BuildID: view.Build.ID,
		ProfileFingerprint: view.Attachment.Profile.Fingerprint,
		Text:               text, MediaType: "text/markdown", Checksum: artifact.BlobHash,
		RequestedOffset: request.Offset, ActualStart: request.Offset, ActualEnd: actualEnd,
		NextOffset: actualEnd, EOF: eof, ResponseBytes: len(text),
	}, nil
}

func readRenditionBlobWindow(
	ctx context.Context, blobs verifiedBlobReader, hash string, expectedSize int64, offset, maxChars int,
) (string, int, bool, error) {
	stream, size, err := blobs.OpenStreamContext(ctx, hash)
	if err != nil {
		return "", 0, false, err
	}
	if size != expectedSize {
		closeErr := discardIncompleteVerification(stream.Close())
		return "", 0, false, errors.Join(
			errors.New("rendition blob size disagrees with catalog authority"), closeErr,
		)
	}
	text, actualEnd, eof, readErr := readUnicodeRenditionWindow(ctx, stream, offset, maxChars)
	closeErr := discardIncompleteVerification(stream.Close())
	if err := errors.Join(readErr, closeErr); err != nil {
		return "", 0, false, err
	}
	return text, actualEnd, eof, nil
}

// A bounded window intentionally closes a verified stream before whole-blob
// verification reaches EOF. Drop only that exact expected sentinel; retain
// any joined release, cancellation, or integrity failure.
func discardIncompleteVerification(err error) error {
	// Equality is intentional: a joined error may also carry a cleanup or
	// integrity failure that must survive removal of the expected sentinel.
	if err == nil || err == pack.ErrVerificationIncomplete { //nolint:errorlint
		return nil
	}
	type multiUnwrapper interface{ Unwrap() []error }
	joined, ok := err.(multiUnwrapper)
	if !ok {
		return err
	}
	remaining := make([]error, 0, len(joined.Unwrap()))
	for _, child := range joined.Unwrap() {
		if child = discardIncompleteVerification(child); child != nil {
			remaining = append(remaining, child)
		}
	}
	return errors.Join(remaining...)
}

func readUnicodeRenditionWindow(
	ctx context.Context, source io.Reader, offset, maxChars int,
) (string, int, bool, error) {
	if source == nil || offset < 0 || maxChars < 1 || maxChars > 16_000 {
		return "", 0, false, ErrInvalidRenditionWindow
	}
	reader := bufio.NewReaderSize(source, 4096)
	readRune := func() (rune, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		char, size, err := reader.ReadRune()
		if err != nil {
			return 0, fmt.Errorf("reading rendition text: %w", err)
		}
		if char == utf8.RuneError && size == 1 {
			return 0, ErrInvalidRenditionEncoding
		}
		return char, nil
	}
	for range offset {
		if _, err := readRune(); err != nil {
			if errors.Is(err, io.EOF) {
				return "", 0, false, fmt.Errorf("%w: offset exceeds rendition length", ErrInvalidRenditionWindow)
			}
			return "", 0, false, err
		}
	}
	var result strings.Builder
	result.Grow(maxChars)
	actualEnd := offset
	for range maxChars {
		char, err := readRune()
		if errors.Is(err, io.EOF) {
			return result.String(), actualEnd, true, nil
		}
		if err != nil {
			return "", 0, false, err
		}
		result.WriteRune(char)
		actualEnd++
	}
	_, err := readRune()
	if errors.Is(err, io.EOF) {
		return result.String(), actualEnd, true, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return result.String(), actualEnd, false, nil
}
