package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/kataevidence"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

const kataOriginalMaxBytes = 16 << 20

type kataArchive struct {
	deps Deps
	gate *OperationGate
	// Each operation caches only its last original; later calls verify sources again.
	originals map[kataOriginalKey]kataOriginal
}

func (a kataArchive) VaultUID() string { return a.deps.Store.VaultID() }

func (a kataArchive) Prepare(ctx context.Context, selector kataevidence.Selector) (kataevidence.Evidence, error) {
	// Exact rendition reads take the processing service's own capture gate.
	if selector.Citation != nil {
		if selector.Path != "" {
			node, err := a.deps.Store.NodeByPath(ctx, selector.Path)
			if err != nil {
				return kataevidence.Evidence{}, err
			}
			selector.NodeID = node.ID
		}
		return a.prepareRendition(ctx, selector)
	}
	return a.prepareOriginal(ctx, selector, "")
}

// A saved hash verifies an existing citation; new citations also need MIME admission.
func (a kataArchive) prepareOriginal(ctx context.Context, selector kataevidence.Selector, savedHash string) (kataevidence.Evidence, error) {
	var result kataevidence.Evidence
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
		if savedHash != "" && savedHash != node.BlobHash {
			return kataevidence.ErrChanged
		}
		if node.Size < 0 || node.Size > kataOriginalMaxBytes {
			return fmt.Errorf("%w: document exceeds the 16 MiB original-text citation limit", kataevidence.ErrUnsupportedSource)
		}
		if savedHash == "" && !store.SupportsTextExtractionMIME(node.MimeType) {
			return fmt.Errorf("%w: document needs a retained text rendition; supply an exact text citation", kataevidence.ErrUnsupportedSource)
		}
		text, err := a.originalText(ctx, node)
		if err != nil {
			return err
		}
		runeCount := utf8.RuneCountInString(text)
		if selector.StartRune < 0 || selector.EndRune <= selector.StartRune || selector.EndRune > runeCount || selector.EndRune-selector.StartRune > kataevidence.MaxChars {
			return fmt.Errorf("%w: end_rune must be within the selected text and at most 1000 code points after start_rune", kataevidence.ErrInvalid)
		}
		// A returned excerpt must not keep an evicted original's backing bytes alive.
		quote := strings.Clone(sliceKataRunes(text, selector.StartRune, selector.EndRune))
		if selector.Quote != "" && selector.Quote != quote {
			return fmt.Errorf("%w: quote does not match the selected range", kataevidence.ErrInvalid)
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
		return kataevidence.Evidence{}, fmt.Errorf("%w: quote does not match the selected range", kataevidence.ErrInvalid)
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
	ref := kataevidence.Reference{Version: 1, Kind: "docbank", Rendition: &c}
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
	var selector kataevidence.Selector
	var vault, hash string
	if c := ref.Rendition; c != nil {
		vault, hash = c.VaultUID.String(), c.ContentSHA256
		selector = kataevidence.Selector{NodeID: c.NodeID, ExpectedVersion: c.ContentVersionID.String(), StartRune: c.Start, EndRune: c.End, Citation: c}
	} else {
		p := ref.Docbank
		vault, hash = p.VaultUID, p.ContentSHA256
		selector = kataevidence.Selector{NodeID: p.NodeID, ExpectedVersion: p.ContentVersionID, StartRune: p.StartRune, EndRune: p.EndRune}
	}
	if vault != a.deps.Store.VaultID() {
		result.State = "unreachable"
		return result, nil
	}

	node, err := a.deps.Store.NodeByID(ctx, selector.NodeID)
	if errors.Is(err, store.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if node.TrashedAt != nil || node.IsDir() {
		return result, nil
	}
	if node.BlobHash != hash || ref.Rendition != nil && node.CurrentVersionID != selector.ExpectedVersion {
		result.State = "changed"
		return result, nil
	}
	// Identical original bytes remain verifiable after a version-only rewrite.
	// Pin the current version so verification still detects a concurrent replacement.
	if ref.Docbank != nil {
		selector.ExpectedVersion = node.CurrentVersionID
	}
	var evidence kataevidence.Evidence
	if ref.Docbank != nil {
		evidence, err = a.prepareOriginal(ctx, selector, hash)
	} else {
		evidence, err = a.prepareRendition(ctx, selector)
	}
	switch {
	case err == nil && evidence.Excerpt == quote:
		result.State = "available"
		result.Evidence = kataevidence.NewEvidence(ref, evidence.Excerpt, evidence.Label)
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

type kataOriginal struct {
	text string
	err  error
}

type kataOriginalKey struct {
	nodeID int64
	hash   string
}

func (a kataArchive) originalText(ctx context.Context, node store.Node) (string, error) {
	key := kataOriginalKey{nodeID: node.ID, hash: node.BlobHash}
	if original, ok := a.originals[key]; ok {
		return original.text, original.err
	}
	clear(a.originals)
	text, err := a.readOriginal(ctx, node)
	a.originals[key] = kataOriginal{text: text, err: err}
	return text, err
}

func (a kataArchive) readOriginal(ctx context.Context, node store.Node) (string, error) {
	stream, size, err := a.deps.Blobs.OpenStreamContext(ctx, node.BlobHash)
	if err != nil {
		return "", processing.CitationIntegrityError(err)
	}
	defer func() { _ = stream.Close() }()
	if size != node.Size {
		return "", document.ErrCitationIntegrity
	}
	data, err := io.ReadAll(io.LimitReader(stream, kataOriginalMaxBytes+1))
	if err != nil {
		return "", processing.CitationIntegrityError(err)
	}
	if !stream.Verified() || int64(len(data)) != node.Size {
		return "", document.ErrCitationIntegrity
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%w: document original is not UTF-8 text", kataevidence.ErrInvalid)
	}
	return string(data), nil
}
