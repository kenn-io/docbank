package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/kataevidence"
	"go.kenn.io/docbank/internal/store"
)

const kataOriginalMaxBytes = 16 << 20

type kataArchive struct {
	deps Deps
	gate *OperationGate
}

func (a kataArchive) VaultUID() string { return a.deps.Store.VaultID() }

func (a kataArchive) Prepare(ctx context.Context, selector kataevidence.Selector) (kataevidence.Evidence, error) {
	var result kataevidence.Evidence
	// Exact rendition reads take the processing service's own capture gate.
	if selector.Citation != nil {
		if selector.Path != "" {
			node, err := a.deps.Store.NodeByPath(ctx, selector.Path)
			if err != nil {
				return result, err
			}
			selector.NodeID = node.ID
		}
		return a.prepareRendition(ctx, selector)
	}
	err := a.gate.CaptureContext(ctx, func() error {
		var node store.Node
		var err error
		if selector.Path != "" {
			node, err = a.deps.Store.NodeByPath(ctx, selector.Path)
		} else {
			node, err = a.deps.Store.NodeByID(ctx, selector.NodeID)
		}
		if err != nil {
			return err
		}
		if node.IsDir() || node.TrashedAt != nil {
			return kataevidence.ErrUnavailable
		}
		if selector.ExpectedVersion != "" && selector.ExpectedVersion != node.CurrentVersionID {
			return kataevidence.ErrChanged
		}
		if node.Size < 0 || node.Size > kataOriginalMaxBytes {
			return fmt.Errorf("%w: document exceeds the 16 MiB original-text citation limit", kataevidence.ErrUnsupportedSource)
		}
		mediaType, _, mediaErr := mime.ParseMediaType(node.MimeType)
		if mediaErr != nil || !strings.HasPrefix(mediaType, "text/") && mediaType != jsonMediaType {
			return fmt.Errorf("%w: document needs a retained text rendition; supply an exact text citation", kataevidence.ErrUnsupportedSource)
		}
		stream, size, err := a.deps.Blobs.OpenStreamContext(ctx, node.BlobHash)
		if err != nil {
			return err
		}
		defer func() { _ = stream.Close() }()
		if size != node.Size {
			return document.ErrCitationIntegrity
		}
		data, err := io.ReadAll(io.LimitReader(stream, kataOriginalMaxBytes+1))
		if err != nil {
			return err
		}
		if !stream.Verified() || int64(len(data)) != node.Size {
			return document.ErrCitationIntegrity
		}
		if !utf8.Valid(data) {
			return fmt.Errorf("%w: document original is not UTF-8 text", kataevidence.ErrInvalid)
		}
		text := string(data)
		runeCount := utf8.RuneCountInString(text)
		if selector.StartRune < 0 || selector.EndRune <= selector.StartRune || selector.EndRune > runeCount || selector.EndRune-selector.StartRune > kataevidence.MaxChars {
			return fmt.Errorf("%w: --end must be within the selected text and at most 1000 code points after --start", kataevidence.ErrInvalid)
		}
		quote := sliceKataRunes(text, selector.StartRune, selector.EndRune)
		if selector.Quote != "" && selector.Quote != quote {
			return fmt.Errorf("%w: --quote does not match the selected range", kataevidence.ErrInvalid)
		}
		ref := kataevidence.Reference{Version: 1, Kind: "docbank", Docbank: &kataevidence.DocumentReference{
			VaultUID: a.deps.Store.VaultID(), NodeID: node.ID, ContentVersionID: node.CurrentVersionID,
			ContentSHA256: node.BlobHash, StartRune: selector.StartRune, EndRune: selector.EndRune}}
		result = kataevidence.NewEvidence(ref, quote, boundedKataLabel(node.Name))
		return nil
	})
	return result, err
}

func (a kataArchive) prepareRendition(ctx context.Context, selector kataevidence.Selector) (kataevidence.Evidence, error) {
	c := *selector.Citation
	if c.NodeID != selector.NodeID || c.Start != selector.StartRune || c.End != selector.EndRune || c.End-c.Start > kataevidence.MaxChars {
		return kataevidence.Evidence{}, kataevidence.ErrInvalid
	}
	if a.deps.Processing == nil {
		return kataevidence.Evidence{}, kataevidence.ErrUnavailable
	}
	node, err := a.deps.Store.NodeByID(ctx, selector.NodeID)
	if err != nil {
		return kataevidence.Evidence{}, err
	}
	if node.TrashedAt != nil || node.IsDir() {
		return kataevidence.Evidence{}, kataevidence.ErrUnavailable
	}
	if c.VaultUID.String() != a.deps.Store.VaultID() || c.ContentVersionID.String() != node.CurrentVersionID || c.ContentSHA256 != node.BlobHash ||
		selector.ExpectedVersion != "" && selector.ExpectedVersion != node.CurrentVersionID {
		return kataevidence.Evidence{}, kataevidence.ErrChanged
	}
	resolved, err := a.deps.Processing.ResolveTextCitation(ctx, c)
	if err != nil {
		return kataevidence.Evidence{}, err
	}
	if selector.Quote != "" && selector.Quote != resolved.Text {
		return kataevidence.Evidence{}, fmt.Errorf("%w: --quote does not match the selected range", kataevidence.ErrInvalid)
	}
	// Retention alone does not make a historical rendition the current authority.
	_, active, err := a.deps.Store.CurrentRenditionByAttachment(ctx, c.NodeID, c.ContentVersionID.String(), c.ContentSHA256, c.RenditionAttachmentID)
	if errors.Is(err, store.ErrNotFound) {
		return kataevidence.Evidence{}, kataevidence.ErrChanged
	}
	if err != nil {
		return kataevidence.Evidence{}, err
	}
	if active.Build.ID != c.BuildID || active.Build.MarkdownChecksum != c.RenditionSHA256 {
		return kataevidence.Evidence{}, kataevidence.ErrChanged
	}
	ref := kataevidence.Reference{Version: 1, Kind: "docbank", Docbank: &kataevidence.DocumentReference{
		VaultUID: c.VaultUID.String(), NodeID: c.NodeID, ContentVersionID: c.ContentVersionID.String(), ContentSHA256: c.ContentSHA256,
		RenditionAttachmentID: c.RenditionAttachmentID, BuildID: c.BuildID, RenditionSHA256: c.RenditionSHA256, StartRune: c.Start, EndRune: c.End}}
	return kataevidence.NewEvidence(ref, resolved.Text, boundedKataLabel(node.Name)), nil
}

// sliceKataRunes locates byte boundaries without allocating a rune for every code point.
// The caller validates UTF-8 and the range first.
func sliceKataRunes(text string, start, end int) string {
	startByte, offset := 0, 0
	for i := range text {
		if offset == start {
			startByte = i
		}
		if offset == end {
			return text[startByte:i]
		}
		offset++
	}
	return text[startByte:]
}

func boundedKataLabel(name string) string {
	runes := []rune(strings.ToValidUTF8(name, "�"))
	return string(runes[:min(len(runes), 512)])
}

func (a kataArchive) Resolve(ctx context.Context, ref kataevidence.Reference, quote string) (kataevidence.Resolution, error) {
	result := kataevidence.Resolution{State: "unavailable", Evidence: kataevidence.Evidence{Reference: ref, ContentTrust: "untrusted"}}
	if kataevidence.ValidateReference(ref) != nil {
		result.State = "unsupported"
		return result, nil //nolint:nilerr // Invalid references are reported as unsupported evidence.
	}
	p := ref.Docbank
	if p.VaultUID != a.deps.Store.VaultID() {
		result.State = "unreachable"
		return result, nil
	}
	node, err := a.deps.Store.NodeByID(ctx, p.NodeID)
	if errors.Is(err, store.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if node.TrashedAt != nil || node.IsDir() {
		return result, nil
	}
	if node.CurrentVersionID != p.ContentVersionID || node.BlobHash != p.ContentSHA256 {
		result.State = "changed"
		return result, nil
	}
	selector := kataevidence.Selector{NodeID: p.NodeID, ExpectedVersion: p.ContentVersionID, StartRune: p.StartRune, EndRune: p.EndRune}
	if p.RenditionAttachmentID != "" {
		citation, err := p.TextCitation()
		if err != nil {
			result.State = "unsupported"
			return result, nil //nolint:nilerr // Unsupported citations remain readable alongside other evidence.
		}
		selector.Citation = &citation
	}
	evidence, err := a.Prepare(ctx, selector)
	switch {
	case err == nil && evidence.Excerpt == quote:
		result.State = "available"
		result.Evidence = evidence
	case errors.Is(err, kataevidence.ErrChanged):
		result.State = "changed"
	case errors.Is(err, kataevidence.ErrUnavailable), errors.Is(err, document.ErrCitationUnavailable), errors.Is(err, store.ErrNotFound):
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return result, err
	default:
		result.State = "failed"
	}
	return result, nil
}
