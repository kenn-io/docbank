// Package production builds immutable text-to-page authority for redaction.
package production

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
)

type AlignmentInput struct {
	PDF                 pdfproduction.Source
	Pages               []document.PageFrameV1
	Evidence            document.NormalizedEvidenceV1
	EvidenceSHA256      string
	Units               []redaction.Unit
	EvidenceReceipt     *EvidenceReceipt
	OpenPageImage       func(context.Context, document.PageImageV1) (io.ReadCloser, error)
	NativeTextForbidden bool
}

type Problem struct { //nolint:errname // Problem is the typed alignment failure contract.
	Code string
}

const problemBoundEvidenceMismatch = "bound_evidence_mismatch"

func (p *Problem) Error() string { return p.Code }

func Align(ctx context.Context, input AlignmentInput) (redaction.TextMap, error) {
	if err := ctx.Err(); err != nil {
		return redaction.TextMap{}, err
	}
	evidenceBytes, evidenceSHA, err := document.MarshalNormalizedEvidenceV1(input.Evidence)
	if err != nil || !digest(input.EvidenceSHA256) || evidenceSHA != input.EvidenceSHA256 {
		return redaction.TextMap{}, fmt.Errorf("retained evidence identity mismatch: %w", &Problem{Code: problemBoundEvidenceMismatch})
	}
	_ = evidenceBytes
	pages, err := validateAlignmentAuthority(input)
	if err != nil {
		return redaction.TextMap{}, err
	}
	var result redaction.TextMap
	if input.NativeTextForbidden {
		if input.EvidenceReceipt == nil {
			return result, &Problem{Code: "mapping_incomplete"}
		}
		if err := validateEvidenceReceipt(ctx, *input.EvidenceReceipt, input, evidenceSHA); err != nil {
			return result, err
		}
		result, err = alignEvidence(input.Pages, input.Evidence, pages, input.PDF.SHA256, evidenceSHA)
	} else {
		var inspected []pdfproduction.NativeTextPage
		inspected, err = pdfproduction.InspectNativeText(ctx, input.PDF, input.Pages)
		if err == nil {
			result, err = alignNative(input.Pages, inspected, pages, evidenceSHA, input.PDF.SHA256)
		}
	}
	if err != nil {
		return redaction.TextMap{}, err
	}
	if len(input.Units) != 0 {
		result.Units, err = bindProvidedUnits(result, input.Units)
	} else {
		result.Units, err = semanticUnits(result, input.Pages, input.Evidence, evidenceSHA)
	}
	if err != nil {
		return redaction.TextMap{}, err
	}
	canonicalizeMap(&result)
	_, result.SHA256, err = redaction.CanonicalTextMap(result)
	if err != nil {
		return redaction.TextMap{}, err
	}
	if err := redaction.ValidateMap(result); err != nil {
		return redaction.TextMap{}, fmt.Errorf("validate aligned map: %w", err)
	}
	return result, nil
}

func validateAlignmentAuthority(input AlignmentInput) ([]redaction.Page, error) {
	if input.PDF.Reader == nil || input.PDF.Size < 1 || !digest(input.PDF.SHA256) || len(input.Pages) == 0 {
		return nil, &Problem{Code: "source_binding_mismatch"}
	}
	frames := make([]redaction.Page, len(input.Pages))
	var source document.PageSource
	for index, frame := range input.Pages {
		if err := document.ValidatePageFrameV1(frame); err != nil || frame.Page != index+1 {
			return nil, &Problem{Code: "page_inventory_mismatch"}
		}
		if index == 0 {
			source = frame.Source
		} else if frame.Source != source {
			return nil, &Problem{Code: "page_inventory_mismatch"}
		}
		_, frameSHA, err := document.MarshalPageFrameV1(frame)
		if err != nil {
			return nil, err
		}
		frames[index] = redaction.Page{Number: index + 1, FrameSHA256: frameSHA, Width: frame.Width, Height: frame.Height}
	}
	if source.SHA256 != input.PDF.SHA256 || source.Size != input.PDF.Size {
		return nil, &Problem{Code: "source_binding_mismatch"}
	}
	// Read the complete bounded source independently before native inspection;
	// PDFium will freeze it again for its own isolated view.
	h := sha256.New()
	n, err := io.Copy(h, io.NewSectionReader(input.PDF.Reader, 0, input.PDF.Size))
	if err != nil || n != input.PDF.Size || hex.EncodeToString(h.Sum(nil)) != input.PDF.SHA256 {
		return nil, &Problem{Code: "source_binding_mismatch"}
	}
	return frames, nil
}

func mapIdentity(contract, pdfSHA, evidenceSHA, text string, pages []redaction.Page) redaction.TextMap {
	return redaction.TextMap{Contract: contract, PDFSHA256: pdfSHA, EvidenceSHA256: evidenceSHA, Text: text, Pages: pages, Atoms: []redaction.Atom{}, Units: []redaction.Unit{}, Gaps: []redaction.Gap{}}
}

func canonicalizeMap(value *redaction.TextMap) {
	*value = redaction.NormalizeTextMap(*value)
}

func digest(value string) bool {
	if len(value) != 64 {
		return false
	}
	b, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(b) == value
}

func canonicalDigest(value any) (string, error) {
	b, err := canonical.Marshal(value)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func mappingError(reason string) error {
	return fmt.Errorf("%s: %w", reason, &Problem{Code: "mapping_incomplete"})
}

func identityKind(e document.NormalizedEvidenceV1) string {
	if e.Family == "mail" || e.UnitKind == document.EvidenceUnitMessage {
		return "email_message"
	}
	if e.UnitKind == document.EvidenceUnitSegment || e.Family == "audio" || e.Family == "video" {
		return "transcript_turn"
	}
	return "paragraph"
}

func normalizeVisibleText(s string) string {
	return strings.TrimSuffix(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}

var errUnsupportedRendition = &Problem{Code: "unsupported_rendition"}
