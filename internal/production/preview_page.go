package production

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"os"
	"strings"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
)

const maxProductionPreviewPNGBytes = 32 << 20
const maxProductionPreviewTextBytes = 16 << 20

// ProductionPreviewPage owns one private unnumbered PNG and its matching
// sanitized text. Close File after a verified ticket copies or consumes it.
type ProductionPreviewPage struct {
	File           *VerifiedProductionFile
	Page           int
	ResolvedSHA256 string
	PNGSHA256      string
	PNGSize        int64
	Text           []byte
	TextSHA256     string
	Layout         redaction.PageLayout
	Endorsements   []redaction.Endorsement
}

type productionPreviewContextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w productionPreviewContextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}

// RenderUnnumberedProductionPreviewPage uses the qualified production burn
// and endorsement path, but never accepts assigned numbers or publishes bytes.
// The caller must obtain source and prepared member from catalog authority.
func RenderUnnumberedProductionPreviewPage(ctx context.Context, source PinnedProductionPDF,
	member documentproduction.PreparedMember, pageNumber int, recipe redaction.Recipe,
	engine pdfproduction.Engine) (result *ProductionPreviewPage, resultErr error) {
	closeRejectedSource := func(err error) (*ProductionPreviewPage, error) {
		if source.Stream != nil {
			err = errors.Join(err, source.Stream.Close())
		}
		return nil, err
	}
	if ctx == nil || engine == nil || pageNumber < 1 || source.Stream == nil ||
		source.PDFSHA256 != member.Member.PDFSHA256 || source.Size != member.Member.PDFSize ||
		member.ResolvedSHA256 != member.Resolved.SHA256 || member.Member.ID == "" {
		return closeRejectedSource(ErrJobConflict)
	}
	if err := ctx.Err(); err != nil {
		return closeRejectedSource(err)
	}
	qualified, err := pdfproduction.QualifiedRecipeForDPI(recipe.DPI)
	recipeRaw, marshalErr := canonical.Marshal(recipe)
	if err != nil || marshalErr != nil || recipe != qualified ||
		hashProductionStageBytes(recipeRaw) != member.Resolved.RecipeSHA256 {
		return closeRejectedSource(ErrJobConflict)
	}
	endorsed, err := PlanEndorsementPages(member.Member.ID, member.Resolved.Pages,
		member.Resolved, nil, recipe)
	if err != nil || pageNumber > len(endorsed) || endorsed[pageNumber-1].Layout.Source.Number != pageNumber {
		return closeRejectedSource(errors.Join(ErrJobConflict, err))
	}
	page := endorsed[pageNumber-1]
	text, err := productionPreviewPageText(member.Resolved, pageNumber)
	if err != nil {
		return closeRejectedSource(err)
	}
	spool, err := SpoolProductionPDF(ctx, source, min(recipe.MaxStagingBytes, int64(math.MaxUint32)))
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, spool.Close())
		if resultErr != nil && result != nil {
			resultErr = errors.Join(resultErr, result.File.Close())
			result = nil
		}
	}()
	raster, err := engine.Render(ctx, pdfproduction.Source{Reader: spool.File,
		Size: source.Size, SHA256: source.PDFSHA256}, page.Layout.Source, recipe)
	if err != nil {
		return nil, err
	}
	masks := make([]redaction.Box, 0)
	for _, box := range member.Resolved.RedactBoxes {
		if box.Page == pageNumber {
			masks = append(masks, box)
		}
	}
	burned, err := pdfproduction.Burn(raster, masks, recipe)
	if err != nil {
		return nil, err
	}
	final, err := pdfproduction.Endorse(burned, page.Layout, page.Endorsements, recipe)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp("", "docbank-production-preview-*.png")
	if err != nil {
		return nil, err
	}
	result = &ProductionPreviewPage{File: &VerifiedProductionFile{File: file},
		Page: pageNumber, ResolvedSHA256: member.Resolved.SHA256,
		Text: text, TextSHA256: hashProductionStageBytes(text),
		Layout: page.Layout, Endorsements: page.Endorsements}
	limit := min(recipe.MaxStagingBytes, int64(maxProductionPreviewPNGBytes))
	writer := &boundedPageWriter{writer: productionPreviewContextWriter{ctx: ctx, writer: file}, limit: limit}
	if err := png.Encode(writer, final.Pixels); err != nil || writer.wrote < 1 {
		return result, errors.Join(ErrJobConflict, err)
	}
	result.PNGSize = writer.wrote
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	hasher := sha256.New()
	n, err := io.Copy(hasher, file)
	if err != nil || n != result.PNGSize {
		return result, errors.Join(ErrJobConflict, err)
	}
	result.PNGSHA256 = hex.EncodeToString(hasher.Sum(nil))
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	decoded, err := png.Decode(file)
	if err != nil || decoded.Bounds() != final.Pixels.Bounds() {
		return result, errors.Join(ErrJobConflict, err)
	}
	if !productionPreviewPixelsEqual(decoded, final.Pixels) {
		return result, ErrJobConflict
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	return result, nil
}

func productionPreviewPixelsEqual(decoded image.Image, expected *image.NRGBA) bool {
	if decoded.Bounds() != expected.Bounds() {
		return false
	}
	switch value := decoded.(type) {
	case *image.NRGBA:
		if value.Stride == expected.Stride {
			return bytes.Equal(value.Pix, expected.Pix)
		}
	case *image.RGBA:
		if value.Stride == expected.Stride && expected.Opaque() {
			return bytes.Equal(value.Pix, expected.Pix)
		}
	}
	for y := expected.Bounds().Min.Y; y < expected.Bounds().Max.Y; y++ {
		for x := expected.Bounds().Min.X; x < expected.Bounds().Max.X; x++ {
			if color.NRGBAModel.Convert(decoded.At(x, y)) != expected.NRGBAAt(x, y) {
				return false
			}
		}
	}
	return true
}

func productionPreviewPageText(resolved redaction.Resolved, page int) ([]byte, error) {
	var output strings.Builder
	for _, run := range resolved.Runs {
		if run.Page != page {
			continue
		}
		if len(run.Text) > maxProductionPreviewTextBytes-output.Len() {
			return nil, ErrJobConflict
		}
		output.WriteString(run.Text)
	}
	return []byte(output.String()), nil
}
