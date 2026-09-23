package production

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"io"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
)

type ImageRenditionInput struct {
	Frame        document.PageFrameV1
	Recipe       document.PageRecipeV1
	Image        document.PageImageV1
	ImageReceipt []byte
	OpenImage    func(context.Context, document.PageImageV1) (io.ReadCloser, error)
}

// ImageRenditionWrapperRecipeSHA256 is the stable identity of the exact
// image-only PDF serializer used for OCR provider uploads.
func ImageRenditionWrapperRecipeSHA256() string {
	return pdfproduction.QualifiedImageWrapperRecipeSHA256()
}

// ValidateImageRenditionFrame accepts the native PNG density matching the
// qualified wrapper, which preserves exact pixels without resampling.
func ValidateImageRenditionFrame(_ context.Context, frame document.PageFrameV1) error {
	if document.ValidatePageFrameV1(frame) != nil || frame.InputUnits != "pixel" || frame.PixelsPerMetreX == 0 || frame.PixelsPerMetreX != frame.PixelsPerMetreY {
		return fmt.Errorf("scan density is unavailable or anisotropic: %w", errUnsupportedRendition)
	}
	// PNG density is an integer number of pixels per metre; round the recipe
	// DPI to that representation (11811 pixels per metre for 300 DPI).
	dpi := int64(pdfproduction.QualifiedRecipe().DPI)
	want := (dpi*5000 + 63) / 127
	if frame.PixelsPerMetreX != want {
		return fmt.Errorf("scan density differs from qualified wrapper: %w", errUnsupportedRendition)
	}
	return validateImageRenditionDimensions(frame, frame.PixelWidth, frame.PixelHeight)
}

func validateImageRenditionDimensions(frame document.PageFrameV1, width, height int64) error {
	dpi := int64(pdfproduction.QualifiedRecipe().DPI)
	if (frame.Width*dpi+9999)/10000 != width || (frame.Height*dpi+9999)/10000 != height {
		return fmt.Errorf("retained pixels do not fit qualified wrapper dimensions: %w", errUnsupportedRendition)
	}
	return nil
}

// RenderImageRendition wraps exact retained page pixels in the qualified fresh
// PDF writer. It neither resamples pixels nor invents physical density.
func RenderImageRendition(ctx context.Context, input ImageRenditionInput) ([]byte, error) {
	return RenderImageSetRendition(ctx, []ImageRenditionInput{input})
}

// RenderImageSetRendition creates the deterministic image-only PDF actually
// presented to an OCR provider. Every page callback reopens the exact retained
// PNG, so construction remains bounded to one page.
func RenderImageSetRendition(ctx context.Context, inputs []ImageRenditionInput) ([]byte, error) {
	return renderImageSetRendition(ctx, inputs, qualifiedTextRenditionLimits.MaxOutputBytes)
}

// RenderImageSetRenditionBounded caps the complete deterministic provider
// upload at the immutable profile's document limit.
func RenderImageSetRenditionBounded(ctx context.Context, inputs []ImageRenditionInput, maxOutputBytes int64) ([]byte, error) {
	return renderImageSetRendition(ctx, inputs, maxOutputBytes)
}

func renderImageSetRendition(ctx context.Context, inputs []ImageRenditionInput, maxOutputBytes int64) ([]byte, error) {
	if len(inputs) == 0 || len(inputs) > document.MaxDocumentPages {
		return nil, &Problem{Code: problemBoundEvidenceMismatch}
	}
	if maxOutputBytes < 1 || maxOutputBytes > qualifiedTextRenditionLimits.MaxOutputBytes {
		return nil, &Problem{Code: problemRenditionLimitExceeded}
	}
	var output bytes.Buffer
	recipe := pdfproduction.QualifiedRecipe()
	recipe.MaxStagingBytes = min(recipe.MaxStagingBytes, maxOutputBytes)
	if err := pdfproduction.WriteRendition(ctx, &output, &imagePageSequence{inputs: inputs}, recipe); err != nil {
		if errors.Is(err, pdfproduction.ErrOutputLimit) {
			return nil, &Problem{Code: problemRenditionLimitExceeded}
		}
		return nil, err
	}
	return output.Bytes(), nil
}

type imagePageSequence struct {
	inputs []ImageRenditionInput
	index  int
}

func (s *imagePageSequence) Next(ctx context.Context) (pdfproduction.PageArtifact, error) {
	if err := ctx.Err(); err != nil {
		return pdfproduction.PageArtifact{}, err
	}
	if s.index == len(s.inputs) {
		return pdfproduction.PageArtifact{}, io.EOF
	}
	// Both validation and the later PNG callback share the writer's page deadline.
	artifact, err := imageRenditionArtifact(ctx, s.inputs[s.index], s.index+1)
	if err != nil {
		return pdfproduction.PageArtifact{}, err
	}
	s.index++
	return artifact, nil
}

func imageRenditionArtifact(ctx context.Context, input ImageRenditionInput, number int) (pdfproduction.PageArtifact, error) {
	if document.ValidatePageFrameV1(input.Frame) != nil {
		return pdfproduction.PageArtifact{}, errUnsupportedRendition
	}
	if input.Frame.InputUnits == "pixel" {
		if err := ValidateImageRenditionFrame(ctx, input.Frame); err != nil {
			return pdfproduction.PageArtifact{}, err
		}
	} else if input.Recipe.DPI != float64(pdfproduction.QualifiedRecipe().DPI) {
		return pdfproduction.PageArtifact{}, errUnsupportedRendition
	}
	receiptBytes, _, err := document.MarshalPageImageV1(input.Image)
	if err != nil || !bytes.Equal(receiptBytes, input.ImageReceipt) || input.OpenImage == nil {
		return pdfproduction.PageArtifact{}, &Problem{Code: problemBoundEvidenceMismatch}
	}
	_, frameSHA, err := document.MarshalPageFrameV1(input.Frame)
	if err != nil || document.ValidatePageImageBinding(input.Image, input.Frame, input.Recipe) != nil {
		return pdfproduction.PageArtifact{}, &Problem{Code: problemBoundEvidenceMismatch}
	}
	if err := validateImageRenditionDimensions(input.Frame, input.Image.Width, input.Image.Height); err != nil {
		return pdfproduction.PageArtifact{}, err
	}
	reader, err := input.OpenImage(ctx, input.Image)
	if err != nil || reader == nil {
		return pdfproduction.PageArtifact{}, &Problem{Code: problemBoundEvidenceMismatch}
	}
	payload, readErr := io.ReadAll(io.LimitReader(reader, input.Image.Size+1))
	readErr = errors.Join(readErr, reader.Close())
	if readErr != nil || int64(len(payload)) != input.Image.Size || hashData(payload) != input.Image.SHA256 {
		return pdfproduction.PageArtifact{}, &Problem{Code: problemBoundEvidenceMismatch}
	}
	config, err := png.DecodeConfig(bytes.NewReader(payload))
	if err != nil || int64(config.Width) != input.Image.Width || int64(config.Height) != input.Image.Height {
		return pdfproduction.PageArtifact{}, &Problem{Code: problemBoundEvidenceMismatch}
	}
	// The wrapper numbers output pages independently of each retained source.
	page := redaction.Page{Number: number, FrameSHA256: frameSHA, Width: input.Frame.Width, Height: input.Frame.Height}
	layout := redaction.PageLayout{Source: page, Output: page}
	layoutBytes, err := canonical.Marshal(layout)
	if err != nil {
		return pdfproduction.PageArtifact{}, err
	}
	endorsements := []redaction.Endorsement{}
	endorsementBytes, err := canonical.Marshal(endorsements)
	if err != nil {
		return pdfproduction.PageArtifact{}, err
	}
	resolvedSHA, err := canonicalDigest(struct {
		Contract, ImageReceiptSHA256 string
	}{"image-rendition/v1", hashData(input.ImageReceipt)})
	if err != nil {
		return pdfproduction.PageArtifact{}, err
	}
	artifact := pdfproduction.PageArtifact{Page: page, PNGSHA256: input.Image.SHA256, PNGSize: input.Image.Size, OpenPNG: func() (io.ReadCloser, error) {
		return input.OpenImage(ctx, input.Image)
	}, ResolvedSHA256: resolvedSHA, Layout: layout, LayoutSHA256: hashData(layoutBytes), Endorsements: endorsements, EndorsementsSHA256: hashData(endorsementBytes), Runs: []redaction.Run{}}
	return artifact, nil
}
