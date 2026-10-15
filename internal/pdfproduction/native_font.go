package pdfproduction

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
)

type whitespaceProof uint8

const (
	whitespaceUnsupported whitespaceProof = iota
	whitespaceBlank
	whitespaceUnproven
)

const (
	maxFontBatches      = 256
	maxFontObservations = 8192
	maxFontRenders      = 2048
)

var errFontUnproven = errors.New("font whitespace proof incomplete")

type nativeFontProof struct {
	instance         pdfium.Pdfium
	document         references.FPDF_DOCUMENT
	programs         map[string][]byte
	started          time.Time
	batches, renders int
}

func newNativeFontProof(instance pdfium.Pdfium,
	document references.FPDF_DOCUMENT) *nativeFontProof {
	return &nativeFontProof{instance: instance, document: document,
		programs: make(map[string][]byte), started: time.Now()}
}

func (p *nativeFontProof) available(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if time.Since(p.started) >= 10*time.Second {
		return errFontUnproven
	}
	return nil
}

func (p *nativeFontProof) check(ctx context.Context,
	object references.FPDF_PAGEOBJECT) (whitespaceProof, error) {
	if err := ctx.Err(); err != nil {
		return whitespaceUnsupported, err
	}
	font, err := p.instance.FPDFTextObj_GetFont(&requests.FPDFTextObj_GetFont{PageObject: object})
	if err != nil {
		return whitespaceUnsupported, fmt.Errorf("read source font: %w", err)
	}
	matched, err := p.qualifiedProgram(font.Font)
	if err != nil || !matched {
		return whitespaceUnsupported, err
	}
	if err = p.available(ctx); err == nil && p.batches < maxFontBatches {
		p.batches++
		var codes []uint32
		codes, err = p.whitespaceCodes(ctx, font.Font)
		if err == nil && len(codes) > 0 {
			var blank bool
			blank, err = p.blankCodes(ctx, font.Font, codes)
			if err == nil && blank {
				return whitespaceBlank, nil
			}
		}
	}
	if err == errFontUnproven { //nolint:errorlint // Joined cleanup failures must remain errors.
		err = nil
	}
	return whitespaceUnproven, err
}

func (p *nativeFontProof) qualifiedProgram(font references.FPDF_FONT) (bool, error) {
	actual, err := p.instance.FPDFFont_GetFontData(&requests.FPDFFont_GetFontData{Font: font})
	if err != nil {
		return false, fmt.Errorf("read font program: %w", err)
	}
	if len(actual.FontData) == 0 || len(actual.FontData) > 64<<10 {
		return false, nil
	}
	hash := sha256.Sum256(actual.FontData)
	digest := hex.EncodeToString(hash[:])
	name, known := qualifiedFontPrograms[digest]
	if !known {
		return false, nil
	}
	expected, loaded := p.programs[name]
	if !loaded {
		expected, err = p.loadProgram(name)
		if err != nil {
			return false, err
		}
		p.programs[name] = expected
	}
	return bytes.Equal(actual.FontData, expected), nil
}

func (p *nativeFontProof) loadProgram(name string) (data []byte, retErr error) {
	font, err := p.instance.FPDFText_LoadStandardFont(&requests.FPDFText_LoadStandardFont{Document: p.document, Font: name})
	if err != nil {
		return nil, fmt.Errorf("load qualified font: %w", err)
	}
	defer func() {
		_, err := p.instance.FPDFFont_Close(&requests.FPDFFont_Close{Font: font.Font})
		retErr = errors.Join(retErr, err)
	}()
	program, err := p.instance.FPDFFont_GetFontData(&requests.FPDFFont_GetFontData{Font: font.Font})
	if err != nil {
		return nil, fmt.Errorf("read font program: %w", err)
	}
	return program.FontData, nil
}

// Every nonempty glyph in these pinned programs was qualified at the probe geometry.
var qualifiedFontPrograms = map[string]string{
	"b6c8fe53f134b8b6d4578cd2d544df4cee9624c4efa8d51a560fb40ea296101b": "Courier",
	"f1b7159702973f54fc86254ea38bcf3712b2a736eb3a0995751e9f4bb45ad603": "Courier-Bold",
	"5007faf8320fa1fcc08b8894b356ad976f60b992ba29ee5272578bbdebaf3876": "Courier-Oblique",
	"8a000945843bd31add06aee63bf9fd41b7578b4f3242a4b7cd46349ad9d24d4d": "Courier-BoldOblique",
	"a02339e95d20db14cbdfc1c6baf359cd36a091d5434925c3a4040f60e1a1af9e": "Helvetica",
	"7bdfa36de1d5757dcfaf8b8a4be95c6e41c40fa70dbed9d78bf979a7b9959054": "Helvetica-Bold",
	"7751cbefeda351708dae8c4a88db043411bc39ab2ae2097685da845d4d16f307": "Helvetica-Oblique",
	"555ce45924338e2e4b32821910221ce85da691fd59a9723c4c971a7c5627fc8b": "Helvetica-BoldOblique",
	"4f57d2b9d884af8f907bf22df6019b52d86cbf6214fdbd02b1ae05472a543f35": "Times-Roman",
	"0bdf4b04e964139818d51eda03d566ba999fc3ba2421b1c6c51f9dc969022e80": "Times-Bold",
	"610ae0687198045c4db5d1a6650fd5e92536631706382eb8b72e38df578d0ae9": "Times-Italic",
	"a406cac82583bf98175cb62c87ed5e95c45fbb34eece63a10b0a13e793cb2e10": "Times-BoldItalic",
}
