package pdfstamp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/font"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/fault"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

const maxStampOutputBytes int64 = 512 << 20

const watermarkArtifact = "/Artifact <</Subtype /Watermark /Type /Pagination >>BDC"

// Keep stamping independent of the host's pdfcpu config and font directory.
func stampConfiguration() *model.Configuration {
	return &model.Configuration{
		Reader15: true, ValidationMode: model.ValidationRelaxed, Offline: true,
		Eol: types.EolLF, WriteObjectStream: true, WriteXRefStream: true,
		Optimize: true, OptimizeBeforeWriting: true, OptimizeResourceDicts: true,
		Cmd: model.ADDWATERMARKS, Limits: model.DefaultResourceLimits(),
	}
}

var ErrStampEngineFailure = errors.New("stamp_engine_failure: the PDF engine could not stamp a page")

type PageLabel struct {
	SourcePage int
	Label      string
}

type Result struct {
	SHA256    string
	Size      int64
	PageCount int
	Pages     []PageLabel
}

type limitedStampWriter struct {
	Writer    io.Writer
	Remaining int64
}

func (w *limitedStampWriter) Write(value []byte) (int, error) {
	if int64(len(value)) > w.Remaining {
		return 0, ErrStampEngineFailure
	}
	written, err := w.Writer.Write(value)
	w.Remaining -= int64(written)
	if err == nil && written != len(value) {
		err = io.ErrShortWrite
	}
	return written, err
}

// Stamp performs the qualified synchronous PDF transformation. Daemon and export
// callers must run it in their bounded, supervised worker process.
// Pages with annotations must be flattened before stamping: viewers draw their
// appearances above page content, where they can cover the label.
// Tagged PDFs are rejected because moving source content into a form would
// disconnect its accessibility structure from the page.
func Stamp(ctx context.Context, source io.ReadSeeker, labels []PageLabel, recipe Recipe, output io.Writer) (Result, error) {
	var zero Result
	if ctx == nil {
		return zero, stampFailure("validate context", errors.New("nil context"))
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if source == nil || output == nil {
		return zero, stampFailure("validate streams", errors.New("nil source or destination"))
	}
	recipe = recipe.normalized()
	if err := recipe.Validate(); err != nil {
		return zero, stampFailure("validate recipe", err)
	}

	pdfContext, dimensions, err := inspectSource(source, recipe.Restamp)
	if err != nil {
		return zero, err
	}
	pageCount := pdfContext.PageCount
	if err := validateLabels(labels, recipe, pageCount, dimensions); err != nil {
		return zero, err
	}
	watermarks, err := buildWatermarks(labels, recipe)
	if err != nil {
		return zero, err
	}
	if err := applyWatermarks(pdfContext, watermarks); err != nil {
		return zero, stampFailure("stamp PDF", err)
	}

	var staged bytes.Buffer
	bounded := &limitedStampWriter{Writer: &staged, Remaining: maxStampOutputBytes}
	if err := api.WriteContext(pdfContext, bounded); err != nil {
		return zero, stampFailure("stamp PDF", err)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	verifiedCount, err := api.PageCount(bytes.NewReader(staged.Bytes()), stampConfiguration())
	if err != nil {
		return zero, stampFailure("verify output page count", err)
	}
	if verifiedCount != pageCount {
		return zero, stampFailure("verify output page count", fmt.Errorf("got %d pages, want %d", verifiedCount, pageCount))
	}
	if err := verifyStampedLabels(staged.Bytes(), labels); err != nil {
		return zero, err
	}

	written, err := output.Write(staged.Bytes())
	if err != nil {
		return zero, stampFailure("write verified output", err)
	}
	if written != staged.Len() {
		return zero, stampFailure("write verified output", io.ErrShortWrite)
	}
	digest := sha256.Sum256(staged.Bytes())
	return Result{
		SHA256: hex.EncodeToString(digest[:]), Size: int64(staged.Len()), PageCount: pageCount,
		Pages: slices.Clone(labels),
	}, nil
}

// Let pdfcpu lay out the stamp in displayed page coordinates, then transform
// only that artifact back into source coordinates. pdfcpu's own rotation
// normalization rewrites source content and mishandles inherited rotation and
// offset crop boxes. Preserve the original boxes and rotation, including absence
// of a leaf entry when the value is inherited.
func applyWatermarks(ctx *model.Context, watermarks map[int]*model.Watermark) (err error) {
	defer fault.Catch(&err)
	originals := make([]types.Dict, ctx.PageCount)
	matrices := make([][6]float64, ctx.PageCount)
	for i := range ctx.PageCount {
		page, _, attrs, err := ctx.PageDict(i+1, false)
		if err != nil {
			return fmt.Errorf("prepare page %d: %w", i+1, err)
		}
		box := attrs.CropBox
		if box == nil {
			box = attrs.MediaBox
		}
		rotation := (attrs.Rotate%360 + 360) % 360
		w, h := box.Width(), box.Height()
		matrix := [6]float64{1, 0, 0, 1, box.LL.X, box.LL.Y}
		switch rotation {
		case 90:
			w, h = h, w
			matrix = [6]float64{0, 1, -1, 0, box.UR.X, box.LL.Y}
		case 180:
			matrix = [6]float64{-1, 0, 0, -1, box.UR.X, box.UR.Y}
		case 270:
			w, h = h, w
			matrix = [6]float64{0, -1, 1, 0, box.LL.X, box.UR.Y}
		case 0:
		default:
			return fmt.Errorf("page %d rotation is not a multiple of 90", i+1)
		}
		matrices[i] = matrix
		originals[i] = types.Dict{"MediaBox": page["MediaBox"], "CropBox": page["CropBox"], "Rotate": page["Rotate"]}
		page.Update("MediaBox", types.NewNumberArray(0, 0, w, h))
		page.Update("CropBox", types.NewNumberArray(0, 0, w, h))
		page.Update("Rotate", types.Integer(0))
		// pdfcpu edits content streams in place. Give every page its own
		// stream, including blank pages, so shared source streams stay intact.
		content, err := sourcePageContent(ctx, page, i+1)
		if err != nil {
			return err
		}
		if err := setPageContent(ctx, page, content); err != nil {
			return err
		}
		resources := maps.Clone(attrs.Resources)
		if resources == nil {
			resources = types.NewDict()
		}
		// Restamping removes and replaces entries in these dictionaries.
		// Resolve indirect dictionaries so pages cannot change each other's stamps.
		for _, key := range []string{"XObject", "ExtGState"} {
			if object, exists := resources[key]; exists {
				dict, err := ctx.DereferenceDict(object)
				if err != nil {
					return fmt.Errorf("read page %d %s: %w", i+1, key, err)
				}
				resources[key] = dict.Clone()
			}
		}
		page.Update("Resources", resources)
	}
	if err := pdfcpu.AddWatermarksMap(ctx, watermarks); err != nil {
		return fmt.Errorf("add watermarks: %w", err)
	}
	for i, original := range originals {
		page, _, _, err := ctx.PageDict(i+1, false)
		if err != nil {
			return fmt.Errorf("restore page %d: %w", i+1, err)
		}
		for key, value := range original {
			if value == nil {
				page.Delete(key)
			} else {
				page.Update(key, value)
			}
		}
		content, start, form, err := stampedForm(ctx, i+1)
		if err != nil {
			return err
		}
		// A Bates label must remain visible even if the source's first layer
		// defaults OFF. Flatten only the new form, preserving source layers.
		form.Delete("OC")
		sourceName, err := isolateSourceContent(ctx, i+1, content[:start])
		if err != nil {
			return err
		}
		m := matrices[i]
		prefix := fmt.Sprintf("q /%s Do Q %s q %f %f %f %f %f %f cm ", sourceName, watermarkArtifact, m[0], m[1], m[2], m[3], m[4], m[5])
		content = append([]byte(prefix), content[start+len(watermarkArtifact)+3:]...)
		if err := setPageContent(ctx, page, content); err != nil {
			return err
		}
	}
	return nil
}

// A form invocation has its own graphics-state stack. Merely surrounding source
// operators with q/Q lets an unmatched source Q pop the stamp's saved state.
func isolateSourceContent(ctx *model.Context, pageNumber int, content []byte) (string, error) {
	_, _, attrs, err := ctx.PageDict(pageNumber, false)
	if err != nil {
		return "", fmt.Errorf("read source page %d resources: %w", pageNumber, err)
	}
	form, err := ctx.NewStreamDictForBuf(content)
	if err != nil {
		return "", fmt.Errorf("create source form: %w", err)
	}
	form.InsertName("Type", "XObject")
	form.InsertName("Subtype", "Form")
	form.Insert("BBox", attrs.MediaBox.Array())
	form.Insert("Resources", attrs.Resources.Clone())
	if err := form.Encode(); err != nil {
		return "", fmt.Errorf("encode source form: %w", err)
	}
	ref, err := ctx.IndRefForNewObject(*form)
	if err != nil {
		return "", fmt.Errorf("store source form: %w", err)
	}
	xObjects, err := ctx.DereferenceDict(attrs.Resources["XObject"])
	if err != nil {
		return "", fmt.Errorf("read source XObjects: %w", err)
	}
	name := xObjects.NewIDForPrefix("Source", 0)
	xObjects.Insert(name, *ref)
	return name, nil
}

func setPageContent(ctx *model.Context, page types.Dict, content []byte) error {
	stream, err := ctx.NewStreamDictForBuf(content)
	if err != nil {
		return fmt.Errorf("create page content: %w", err)
	}
	if err := stream.Encode(); err != nil {
		return fmt.Errorf("encode page content: %w", err)
	}
	ref, err := ctx.IndRefForNewObject(*stream)
	if err != nil {
		return fmt.Errorf("store page content: %w", err)
	}
	page.Update("Contents", *ref)
	return nil
}

func sourcePageContent(ctx *model.Context, page types.Dict, pageNumber int) ([]byte, error) {
	object, err := ctx.Dereference(page["Contents"])
	if err != nil {
		return nil, fmt.Errorf("read page %d streams: %w", pageNumber, err)
	}
	streams, ok := object.(types.Array)
	if !ok {
		streams = types.Array{object}
	}
	var content bytes.Buffer
	for _, stream := range streams {
		decoded, err := ctx.PageContent(types.Dict{"Contents": stream}, pageNumber)
		if err != nil && !errors.Is(err, model.ErrNoContent) {
			return nil, fmt.Errorf("decode page %d stream: %w", pageNumber, err)
		}
		content.Write(decoded)
		// PDF stream arrays may split only between lexical tokens (ISO 32000-1,
		// Table 30). Preserve those boundaries and terminate trailing comments.
		content.WriteByte('\n')
	}
	return content.Bytes(), nil
}

// Read the form drawn by pdfcpu's final artifact. Source XObjects can be large
// images or contain identical text; neither says anything about the new stamp.
func stampedForm(ctx *model.Context, pageNumber int) ([]byte, int, *types.StreamDict, error) {
	page, _, attrs, err := ctx.PageDict(pageNumber, false)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read page: %w", err)
	}
	content, err := ctx.PageContent(page, pageNumber)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read page content: %w", err)
	}
	start := bytes.LastIndex(content, []byte(watermarkArtifact))
	if start < 0 {
		return nil, 0, nil, errors.New("missing final watermark artifact")
	}
	fields := strings.Fields(string(content[start+len(watermarkArtifact):]))
	n := len(fields)
	if n < 5 || fields[0] != "q" || fields[n-3] != "Do" || fields[n-2] != "Q" || fields[n-1] != "EMC" || !strings.HasPrefix(fields[n-4], "/Fm") {
		return nil, 0, nil, errors.New("unexpected final watermark artifact")
	}
	xObjects, err := ctx.DereferenceDict(attrs.Resources["XObject"])
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read page XObjects: %w", err)
	}
	form, _, err := ctx.DereferenceStreamDict(xObjects[strings.TrimPrefix(fields[n-4], "/")])
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read stamp form: %w", err)
	}
	if form == nil || form.Subtype() == nil || *form.Subtype() != "Form" {
		return nil, 0, nil, errors.New("watermark does not draw a form")
	}
	return content, start, form, nil
}

func verifyStampedLabels(pdf []byte, labels []PageLabel) error {
	return verifyStampedLabelsReader(bytes.NewReader(pdf), labels)
}

func verifyStampedLabelsReader(source io.ReadSeeker, labels []PageLabel) error {
	pdfContext, err := api.ReadContext(source, stampConfiguration())
	if err != nil {
		return stampFailure("verify output labels", err)
	}
	if err := api.ValidateContext(pdfContext); err != nil {
		return stampFailure("verify output labels", err)
	}
	if pdfContext.PageCount != len(labels) {
		return stampFailure("verify output labels", errors.New("page and label counts differ"))
	}
	for _, label := range labels {
		_, _, form, err := stampedForm(pdfContext, label.SourcePage)
		if err != nil {
			return stampFailure("verify output labels", fmt.Errorf("page %d: %w", label.SourcePage, err))
		}
		if _, optional := form.Find("OC"); optional {
			return stampFailure("verify output labels", fmt.Errorf("page %d stamp depends on optional content", label.SourcePage))
		}
		if err := form.DecodeWithLimit(4 << 20); err != nil {
			return stampFailure("verify output labels", fmt.Errorf("decode page %d stamp: %w", label.SourcePage, err))
		}
		escaped, err := types.Escape(label.Label)
		if err != nil {
			return stampFailure("verify output labels", fmt.Errorf("encode page %d label: %w", label.SourcePage, err))
		}
		labelToken := []byte("(" + *escaped + ") Tj")
		matches := bytes.Count(form.Content, labelToken)
		if matches != 1 {
			return stampFailure("verify output labels", fmt.Errorf("page %d contains %d copies of its declared label", label.SourcePage, matches))
		}
	}
	return nil
}

func inspectSource(source io.ReadSeeker, allowRestamp bool) (*model.Context, []types.Dim, error) {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, nil, stampFailure("rewind source", err)
	}
	pdfContext, err := api.ReadValidateAndOptimize(source, stampConfiguration())
	if err != nil {
		return nil, nil, stampFailure("read source PDF", err)
	}
	catalog, err := pdfContext.Catalog()
	if err != nil {
		return nil, nil, stampFailure("inspect source structure", err)
	}
	structure, err := pdfContext.DereferenceDict(catalog["StructTreeRoot"])
	if err != nil {
		return nil, nil, stampFailure("inspect source structure", err)
	}
	if structure != nil {
		return nil, nil, stampFailure("inspect source structure", errors.New("tagged PDFs are not supported: stamping cannot preserve their accessibility structure"))
	}
	count := pdfContext.PageCount
	if count < 1 {
		return nil, nil, stampFailure("read source page count", errors.New("PDF has no pages"))
	}
	boundaries, err := pdfContext.PageBoundaries(nil)
	if err != nil {
		return nil, nil, stampFailure("read source page boundaries", err)
	}
	if len(boundaries) != count {
		return nil, nil, stampFailure("read source page boundaries", errors.New("page boundary count mismatch"))
	}
	dimensions := make([]types.Dim, count)
	for index, boundary := range boundaries {
		page, _, _, err := pdfContext.PageDict(index+1, false)
		if err != nil {
			return nil, nil, stampFailure("inspect source annotations", err)
		}
		annotations, err := pdfContext.DereferenceArray(page["Annots"])
		if err != nil {
			return nil, nil, stampFailure("inspect source annotations", err)
		}
		// Viewers paint annotations after page content. Their appearance can
		// cover a stamp regardless of its page graphics state or layer settings.
		if len(annotations) != 0 {
			return nil, nil, stampFailure("inspect source annotations",
				fmt.Errorf("page %d contains annotations; flatten annotations before stamping", index+1))
		}
		cropBox := boundary.CropBox()
		if cropBox == nil {
			return nil, nil, stampFailure("read source page boundaries", fmt.Errorf("page %d has no effective CropBox", index+1))
		}
		dimensions[index] = cropBox.Dimensions()
		if boundary.Rot%180 != 0 {
			dimensions[index].Width, dimensions[index].Height = dimensions[index].Height, dimensions[index].Width
		}
	}
	if !allowRestamp {
		// pdfcpu recognizes its own artifacts, not arbitrary Bates text from
		// Acrobat or other tools. Such marks remain part of the source content.
		if err := pdfcpu.DetectPageTreeWatermarks(pdfContext); err != nil {
			return nil, nil, stampFailure("inspect source watermarks", err)
		}
		if pdfContext.Watermarked {
			return nil, nil, stampFailure("inspect source watermarks", errors.New("source already contains a pdfcpu watermark"))
		}
	}
	return pdfContext, dimensions, nil
}

func validateLabels(labels []PageLabel, recipe Recipe, pageCount int, dimensions []types.Dim) error {
	if len(labels) != pageCount {
		return stampFailure("validate page labels", fmt.Errorf("got %d labels for %d pages", len(labels), pageCount))
	}
	maxSequence := int64(1)
	for range recipe.Padding {
		maxSequence *= 10
	}
	maxSequence--
	endSequence := int64(recipe.StartAt) + int64(pageCount) - 1
	if endSequence > maxSequence {
		return stampFailure("validate page labels", errors.New("assigned sequence exceeds recipe padding"))
	}
	for index, label := range labels {
		page := index + 1
		if label.SourcePage != page {
			return stampFailure("validate page labels", fmt.Errorf("label %d targets page %d", index, label.SourcePage))
		}
		expected := fmt.Sprintf("%s%0*d%s", recipe.Prefix, recipe.Padding, recipe.StartAt+index, recipe.Suffix)
		if label.Label != expected {
			return stampFailure("validate page labels", fmt.Errorf("page %d label differs from the assigned recipe sequence", page))
		}
		width, err := font.TextWidthFloat(label.Label, recipe.FontName, float64(recipe.FontSizePoints))
		if err != nil {
			return stampFailure("measure page label", err)
		}
		minimumWidth := width + 2*float64(recipe.MarginPoints)
		minimumHeight := float64(recipe.FontSizePoints) + 2*float64(recipe.MarginPoints)
		if dimensions[index].Width < minimumWidth || dimensions[index].Height < minimumHeight {
			return stampFailure("validate stamp geometry", fmt.Errorf("page %d is too small for its label and margin", page))
		}
	}
	return nil
}

func buildWatermarks(labels []PageLabel, recipe Recipe) (map[int]*model.Watermark, error) {
	position, offsetX, offsetY := pdfcpuPosition(recipe.Position, recipe.MarginPoints)
	description := fmt.Sprintf(
		"fontname:%s, points:%d, position:%s, offset:%d %d, scalefactor:1 abs, color:%s, opacity:%g, rotation:0",
		recipe.FontName, recipe.FontSizePoints, position, offsetX, offsetY, recipe.Color, recipe.Opacity,
	)
	watermarks := make(map[int]*model.Watermark, len(labels))
	for _, label := range labels {
		watermark, err := api.TextWatermark(label.Label, description, true, recipe.Restamp, types.POINTS)
		if err != nil {
			return nil, stampFailure("create page watermark", err)
		}
		watermarks[label.SourcePage] = watermark
	}
	return watermarks, nil
}

func pdfcpuPosition(position string, margin int) (string, int, int) {
	positions := map[string]string{
		"top-left": "tl", "top-center": "tc", "top-right": "tr",
		"middle-left": "l", "middle-center": "c", "middle-right": "r",
		"bottom-left": "bl", "bottom-center": "bc", "bottom-right": "br",
	}
	x, y := 0, 0
	if position == "top-left" || position == "middle-left" || position == "bottom-left" {
		x = margin
	}
	if position == "top-right" || position == "middle-right" || position == "bottom-right" {
		x = -margin
	}
	if position == "top-left" || position == "top-center" || position == "top-right" {
		y = -margin
	}
	if position == "bottom-left" || position == "bottom-center" || position == "bottom-right" {
		y = margin
	}
	return positions[position], x, y
}

func stampFailure(operation string, cause error) error {
	return fmt.Errorf("%w: %s: %w", ErrStampEngineFailure, operation, cause)
}
