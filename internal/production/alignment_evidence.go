package production

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"io"
	"math/big"
	"sort"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
)

type PageEvidence struct {
	Image        document.PageImageV1 `json:"image"`
	ImageReceipt []byte               `json:"image_receipt"`
}

type EvidenceReceipt struct {
	Contract             string         `json:"contract"`
	SHA256               string         `json:"sha256"`
	EvidenceSHA256       string         `json:"evidence_sha256"`
	PDFSHA256            string         `json:"pdf_sha256"`
	AuthorizationSHA256  string         `json:"authorization_sha256"`
	AuthorizationReceipt []byte         `json:"authorization_receipt"`
	Pages                []PageEvidence `json:"pages"`
}

func CanonicalEvidenceReceipt(value EvidenceReceipt) (string, error) {
	identity := struct {
		Contract             string         `json:"contract"`
		EvidenceSHA256       string         `json:"evidence_sha256"`
		PDFSHA256            string         `json:"pdf_sha256"`
		AuthorizationSHA256  string         `json:"authorization_sha256"`
		AuthorizationReceipt []byte         `json:"authorization_receipt"`
		Pages                []PageEvidence `json:"pages"`
	}{value.Contract, value.EvidenceSHA256, value.PDFSHA256, value.AuthorizationSHA256, value.AuthorizationReceipt, value.Pages}
	return canonicalDigest(identity)
}

func validateEvidenceReceipt(ctx context.Context, receipt EvidenceReceipt, input AlignmentInput, evidenceSHA string) error {
	digestValue, err := CanonicalEvidenceReceipt(receipt)
	if err != nil || receipt.Contract != "alignment-evidence/v1" || digestValue != receipt.SHA256 || receipt.EvidenceSHA256 != evidenceSHA || receipt.PDFSHA256 != input.PDF.SHA256 || !digest(receipt.AuthorizationSHA256) || hashData(receipt.AuthorizationReceipt) != receipt.AuthorizationSHA256 || len(receipt.Pages) != len(input.Pages) || input.OpenPageImage == nil {
		return &Problem{Code: problemBoundEvidenceMismatch}
	}
	for index, page := range receipt.Pages {
		frame := input.Pages[index]
		_, frameSHA, err := document.MarshalPageFrameV1(frame)
		expectedReceipt, receiptSHA, encodeErr := document.MarshalPageImageV1(page.Image)
		if err != nil || encodeErr != nil || !bytes.Equal(expectedReceipt, page.ImageReceipt) || receiptSHA != hashData(page.ImageReceipt) || document.ValidatePageImageV1(page.Image) != nil || page.Image.Page != index+1 || page.Image.Source != frame.Source || page.Image.FrameSHA256 != frameSHA {
			return &Problem{Code: problemBoundEvidenceMismatch}
		}
		reader, openErr := input.OpenPageImage(ctx, page.Image)
		if openErr != nil || reader == nil {
			return &Problem{Code: problemBoundEvidenceMismatch}
		}
		payload, readErr := io.ReadAll(io.LimitReader(reader, page.Image.Size+1))
		readErr = errors.Join(readErr, reader.Close())
		if readErr != nil || int64(len(payload)) != page.Image.Size || hashData(payload) != page.Image.SHA256 {
			return &Problem{Code: problemBoundEvidenceMismatch}
		}
		config, pngErr := png.DecodeConfig(bytes.NewReader(payload))
		if pngErr != nil || int64(config.Width) != page.Image.Width || int64(config.Height) != page.Image.Height {
			return &Problem{Code: problemBoundEvidenceMismatch}
		}
	}
	return nil
}

func alignEvidence(frames []document.PageFrameV1, evidence document.NormalizedEvidenceV1, pages []redaction.Page, pdfSHA, evidenceSHA string) (redaction.TextMap, error) {
	if evidence.Completeness != document.EvidenceComplete || len(frames) == 0 || len(evidence.Units) != len(frames) {
		return redaction.TextMap{}, mappingError("retained OCR does not cover the complete page inventory")
	}
	for index, unit := range evidence.Units {
		page := int64(index + 1)
		if unit.Locator.Kind != document.EvidenceLocatorPage ||
			unit.Locator.IndexOrigin != document.EvidenceIndexOriginOne ||
			unit.Locator.Start != page || unit.Locator.End != page {
			return redaction.TextMap{}, mappingError("retained OCR page locators do not exactly cover the page inventory")
		}
	}
	result := mapIdentity("aligned-text/v1", pdfSHA, evidenceSHA, "", pages)
	pageIndex := 0
	result.Pages[0].Span.Start = 0
	for unitIndex, unit := range evidence.Units {
		unitPage := evidencePage(unit, len(frames))
		if unitPage < pageIndex {
			return redaction.TextMap{}, mappingError("evidence page order moves backward")
		}
		for pageIndex < unitPage {
			result.Pages[pageIndex].Span.End = int64(len(result.Text))
			pageIndex++
			result.Pages[pageIndex].Span.Start = int64(len(result.Text))
		}
		start := int64(len(result.Text))
		result.Text += unit.Text
		end := int64(len(result.Text))
		result.Pages[pageIndex].Span.End = end
		boundaries := runeByteBoundaries(unit.Text)
		coverage := make([]uint8, len(boundaries)-1)
		for _, region := range unit.Regions {
			if region.Geometry == nil || region.TextRange.Start < 0 || region.TextRange.End <= region.TextRange.Start || region.TextRange.End >= len(boundaries) {
				continue
			}
			boxes, err := evidenceBoxes(frames[unitPage], pages[unitPage], *region.Geometry)
			if err != nil {
				return redaction.TextMap{}, err
			}
			if len(boxes) == 0 {
				continue
			}
			span := redaction.Span{Start: start + int64(boundaries[region.TextRange.Start]), End: start + int64(boundaries[region.TextRange.End])}
			// Geometry is the smallest evidence-backed cluster. Never quote-match
			// it against other equal strings.
			result.Atoms = append(result.Atoms, redaction.Atom{Span: span, Boxes: boxes})
			for runeIndex := region.TextRange.Start; runeIndex < region.TextRange.End; runeIndex++ {
				coverage[runeIndex]++
				if coverage[runeIndex] > 1 {
					return redaction.TextMap{}, mappingError(fmt.Sprintf("evidence unit %d has overlapping mapped regions", unitIndex))
				}
			}
		}
		for runeIndex, character := range []rune(unit.Text) {
			if !unicode.IsSpace(character) && coverage[runeIndex] != 1 {
				return redaction.TextMap{}, mappingError(fmt.Sprintf("evidence unit %d has incomplete mapped geometry", unitIndex))
			}
		}
	}
	for pageIndex < len(result.Pages)-1 {
		result.Pages[pageIndex].Span.End = int64(len(result.Text))
		pageIndex++
		result.Pages[pageIndex].Span = redaction.Span{Start: int64(len(result.Text)), End: int64(len(result.Text))}
	}
	return result, nil
}

func evidencePage(unit document.NormalizedEvidenceUnitV1, count int) int {
	if unit.Locator.Kind == document.EvidenceLocatorPage && unit.Locator.IndexOrigin == document.EvidenceIndexOriginOne && unit.Locator.Start == unit.Locator.End && unit.Locator.Start >= 1 && unit.Locator.Start <= int64(count) {
		return int(unit.Locator.Start - 1)
	}
	return -1
}

func runeByteBoundaries(value string) []int {
	result := make([]int, 0, utf8.RuneCountInString(value)+1)
	for offset := range value {
		result = append(result, offset)
	}
	return append(result, len(value))
}

func evidenceBoxes(frame document.PageFrameV1, page redaction.Page, geometry document.EvidenceGeometryV1) ([]redaction.Box, error) {
	if geometry.Orientation != 0 || geometry.Scale <= 0 || geometry.Width <= 0 || geometry.Height <= 0 || geometry.CoordinateOrigin != document.EvidenceCoordinateTopLeft || geometry.CoordinateSpace != document.EvidenceCoordinatePage {
		return nil, &Problem{Code: "unsupported_evidence_geometry"}
	}
	var result []redaction.Box
	for _, b := range geometry.Boxes {
		if b.Left < 0 || b.Top < 0 || b.Right <= b.Left || b.Bottom <= b.Top || b.Right > geometry.Width || b.Bottom > geometry.Height {
			return nil, &Problem{Code: problemBoundEvidenceMismatch}
		}
		var box redaction.Box
		var err error
		scale := func(value, physical, denominator int64, roundUp bool) (int64, error) {
			product := new(big.Int).Mul(big.NewInt(value), big.NewInt(physical))
			quotient, remainder := new(big.Int), new(big.Int)
			quotient.QuoRem(product, big.NewInt(denominator), remainder)
			if roundUp && remainder.Sign() != 0 {
				quotient.Add(quotient, big.NewInt(1))
			}
			if !quotient.IsInt64() || quotient.Sign() < 0 || quotient.Cmp(big.NewInt(document.MaxPageInteger)) > 0 {
				return 0, &Problem{Code: problemBoundEvidenceMismatch}
			}
			return quotient.Int64(), nil
		}
		makeBox := func(width, height int64) (redaction.Box, error) {
			x0, err := scale(b.Left, page.Width, width, false)
			if err != nil {
				return redaction.Box{}, err
			}
			y0, err := scale(b.Top, page.Height, height, false)
			if err != nil {
				return redaction.Box{}, err
			}
			x1, err := scale(b.Right, page.Width, width, true)
			if err != nil {
				return redaction.Box{}, err
			}
			y1, err := scale(b.Bottom, page.Height, height, true)
			if err != nil {
				return redaction.Box{}, err
			}
			if x1 <= x0 || y1 <= y0 || x1 > page.Width || y1 > page.Height {
				return redaction.Box{}, &Problem{Code: problemBoundEvidenceMismatch}
			}
			return redaction.Box{Page: page.Number, FrameSHA256: page.FrameSHA256, X0: x0, Y0: y0, X1: x1, Y1: y1}, nil
		}
		switch geometry.Unit {
		case document.EvidenceGeometryNormalized:
			box, err = makeBox(geometry.Width, geometry.Height)
		case document.EvidenceGeometryPixel:
			if frame.InputUnits != "pixel" || geometry.Width != frame.PixelWidth || geometry.Height != frame.PixelHeight {
				return nil, &Problem{Code: "unsupported_evidence_geometry"}
			}
			box, err = makeBox(frame.PixelWidth, frame.PixelHeight)
		default:
			return nil, &Problem{Code: "unsupported_evidence_geometry"}
		}
		if err != nil {
			return nil, err
		}
		result = append(result, box)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Y0 != result[j].Y0 {
			return result[i].Y0 < result[j].Y0
		}
		return result[i].X0 < result[j].X0
	})
	return result, nil
}
