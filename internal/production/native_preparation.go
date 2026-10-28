package production

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
)

const MaxNativeObservationBytes int64 = 8 << 20

var ErrNativePreparationLimit = errors.New("native PDF preparation exceeds its bounds")

type NativePDFEvidence struct {
	Contract               string                         `json:"contract"`
	Source                 document.PageSource            `json:"source"`
	PageRuntimeFingerprint string                         `json:"page_runtime_fingerprint"`
	RendererSHA256         string                         `json:"renderer_sha256"`
	Pages                  []pdfproduction.NativeTextPage `json:"pages"`
}

type NativePreparation struct {
	Frames         []document.PageFrameV1 `json:"frames"`
	Evidence       NativePDFEvidence      `json:"evidence"`
	EvidenceSHA256 string                 `json:"evidence_sha256"`
	Map            redaction.TextMap      `json:"map"`
}

type NativePreparationInput struct {
	PDF                    pdfproduction.Source
	Inspection             pagerender.Inspection
	PageRuntimeFingerprint string
}

func PrepareNativePDF(ctx context.Context, input NativePreparationInput) (NativePreparation, error) {
	if input.Inspection.PageCount > document.MaxRenderPages {
		return NativePreparation{}, ErrNativePreparationLimit
	}
	pages, err := pdfproduction.InspectNativeTextFromInspection(ctx, pdfproduction.NativeInspectionInput{
		Source: input.PDF, Inspection: input.Inspection, MaxObservationBytes: MaxNativeObservationBytes,
	})
	if err != nil {
		return NativePreparation{}, err
	}
	if err := ctx.Err(); err != nil {
		return NativePreparation{}, err
	}
	evidence := NativePDFEvidence{Contract: "native-pdf-evidence/v1", Source: input.Inspection.Source,
		PageRuntimeFingerprint: input.PageRuntimeFingerprint,
		RendererSHA256:         pdfproduction.QualifiedRecipe().RendererSHA256, Pages: pages}
	result, err := assembleNativePreparation(input.Inspection.Frames, evidence)
	if err != nil {
		return NativePreparation{}, err
	}
	if err := ctx.Err(); err != nil {
		return NativePreparation{}, err
	}
	return result, nil
}

func assembleNativePreparation(frames []document.PageFrameV1, evidence NativePDFEvidence) (NativePreparation, error) {
	if _, exceeded, err := canonical.BoundedSize(evidence, MaxNativeObservationBytes); err != nil {
		return NativePreparation{}, err
	} else if exceeded {
		return NativePreparation{}, ErrNativePreparationLimit
	}
	pages, err := alignmentPageFrames(frames, evidence.Source)
	if err != nil {
		return NativePreparation{}, err
	}
	evidenceSHA, err := canonicalDigest(evidence)
	if err != nil {
		return NativePreparation{}, err
	}
	mapping, err := alignNative(frames, evidence.Pages, pages, evidenceSHA, evidence.Source.SHA256)
	if err != nil {
		return NativePreparation{}, err
	}
	mapping, err = finishAlignment(mapping)
	if err != nil {
		return NativePreparation{}, err
	}
	result := NativePreparation{Frames: frames, Evidence: evidence, EvidenceSHA256: evidenceSHA, Map: mapping}
	if err := ValidateNativePreparation(result); err != nil {
		return NativePreparation{}, err
	}
	return result, nil
}

// ValidateNativePreparation checks response consistency, not the source's pixels.
func ValidateNativePreparation(result NativePreparation) error {
	evidence := result.Evidence
	if evidence.RendererSHA256 != pdfproduction.QualifiedRecipe().RendererSHA256 && canonical.IsSHA256Hex(evidence.RendererSHA256) {
		return pdfproduction.ErrVerifierVersion
	}
	if evidence.Contract != "native-pdf-evidence/v1" || evidence.Source.Validate() != nil ||
		!canonical.IsSHA256Hex(evidence.PageRuntimeFingerprint) ||
		evidence.RendererSHA256 != pdfproduction.QualifiedRecipe().RendererSHA256 ||
		len(result.Frames) == 0 || len(result.Frames) > document.MaxRenderPages ||
		len(result.Frames) != len(evidence.Pages) || len(result.Frames) != len(result.Map.Pages) {
		return errors.New("native preparation identities or page inventory differ")
	}
	if _, exceeded, err := canonical.BoundedSize(evidence, MaxNativeObservationBytes); err != nil {
		return err
	} else if exceeded {
		return ErrNativePreparationLimit
	}
	digest, err := canonicalDigest(evidence)
	if err != nil {
		return err
	}
	if digest != result.EvidenceSHA256 || result.Map.EvidenceSHA256 != digest ||
		result.Map.PDFSHA256 != evidence.Source.SHA256 || len(result.Map.Units) != 0 {
		return errors.New("native preparation evidence binding differs")
	}
	pages, err := alignmentPageFrames(result.Frames, evidence.Source)
	if err != nil {
		return err
	}
	for i, page := range pages {
		actual := result.Map.Pages[i]
		if evidence.Pages[i].Number != i+1 || page.Number != actual.Number ||
			page.FrameSHA256 != actual.FrameSHA256 || page.Width != actual.Width || page.Height != actual.Height {
			return fmt.Errorf("native preparation page %d binding differs", i+1)
		}
	}
	return redaction.ValidateMap(result.Map)
}

// ValidateNativePreparationDerivation checks that submitted geometry follows
// from its observations. Only fresh inspection can authenticate those observations.
func ValidateNativePreparationDerivation(value NativePreparation) error {
	if err := ValidateNativePreparation(value); err != nil {
		return err
	}
	rebuilt, err := assembleNativePreparation(value.Frames, value.Evidence)
	if err != nil {
		return err
	}
	if rebuilt.Map.SHA256 != value.Map.SHA256 {
		return errors.New("native preparation map differs from its observations")
	}
	return nil
}
