package pdfproduction

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"math/big"
	"os"
	"slices"
	"sort"
	"unicode"
	"unicode/utf8"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/enums"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/responses"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
)

// NativeGlyph is one PDFium character observation. Bounds are PDF user-space
// points quantized to point/10000. A nonempty GapReason means the character was
// not accepted as visible native text and must never become a text atom.
type NativeGlyph struct {
	Text      string
	Bounds    [4]int64 // left, bottom, right, top
	GapReason string
}

type NativeTextPage struct {
	Number int
	Glyphs []NativeGlyph
	// NonTextBounds conservatively marks painted image/path/shading/form
	// regions that may contain visible text unavailable to native extraction.
	NonTextBounds [][4]int64
}

// InspectNativeText freezes and hashes source once, then inspects individual
// characters. It deliberately does not use GetPageText: that API includes
// generated and out-of-crop characters and obscures object visibility.
func InspectNativeText(ctx context.Context, source Source, frames []document.PageFrameV1) (_ []NativeTextPage, resultErr error) {
	if len(frames) == 0 || len(frames) > maxPageCount {
		return nil, errors.New("invalid native text page inventory")
	}
	recipe := defaultRecipe()
	staged, err := stageSource(ctx, source, recipe.MaxStagingBytes)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, staged.Close(), os.Remove(staged.Name())) }()
	engine, err := NewPDFium(recipe)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, engine.Close()) }()
	e, ok := engine.(*pdfiumEngine)
	if !ok {
		return nil, errors.New("qualified native text engine has unexpected type")
	}
	result := make([]NativeTextPage, len(frames))
	for index, frame := range frames {
		if frame.Page != index+1 || document.ValidatePageFrameV1(frame) != nil || frame.InputUnits != "point/10000" {
			return nil, errors.New("native text pages are not contiguous")
		}
	}
	var totalCharacters int
	err = e.withInstance(ctx, func(instance pdfium.Pdfium) (operationErr error) {
		doc, err := instance.OpenDocument(&requests.OpenDocument{FileReader: io.NewSectionReader(staged, 0, source.Size), FileReaderSize: source.Size})
		if err != nil {
			return fmt.Errorf("open native PDF: %w", err)
		}
		defer func() {
			_, closeErr := instance.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
			operationErr = errors.Join(operationErr, closeErr)
		}()
		count, err := instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
		if err != nil || count.PageCount != len(frames) {
			return errors.New("native PDF page inventory mismatch")
		}
		for index, frame := range frames {
			if err := ctx.Err(); err != nil {
				return err
			}
			result[index], err = inspectLoadedNativePage(ctx, instance, doc.Document, index, frame, maxTextBytes-totalCharacters)
			if err != nil {
				return fmt.Errorf("inspect native PDF page %d: %w", frame.Page, err)
			}
			totalCharacters += len(result[index].Glyphs)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func inspectLoadedNativePage(ctx context.Context, instance pdfium.Pdfium, documentRef references.FPDF_DOCUMENT, index int, frame document.PageFrameV1, remaining int) (result NativeTextPage, resultErr error) {
	loaded, err := instance.FPDF_LoadPage(&requests.FPDF_LoadPage{Document: documentRef, Index: index})
	if err != nil {
		return result, fmt.Errorf("load native PDF page: %w", err)
	}
	defer func() {
		_, closeErr := instance.FPDF_ClosePage(&requests.FPDF_ClosePage{Page: loaded.Page})
		resultErr = errors.Join(resultErr, closeErr)
	}()
	ref := requests.Page{ByReference: &loaded.Page}
	if err := checkNativePageFrame(instance, ref, frame); err != nil {
		return result, err
	}
	return inspectNativePage(ctx, instance, documentRef, ref, frame, remaining)
}

func checkNativePageFrame(instance pdfium.Pdfium, ref requests.Page, frame document.PageFrameV1) error {
	media, mediaErr := instance.FPDFPage_GetMediaBox(&requests.FPDFPage_GetMediaBox{Page: ref})
	crop, cropErr := instance.FPDFPage_GetCropBox(&requests.FPDFPage_GetCropBox{Page: ref})
	// PDFium's transform API reports only direct page-dictionary boxes. When
	// both are inherited/implicit, its independently resolved bounding box is
	// authoritative only for the equal MediaBox/CropBox case.
	if media == nil || crop == nil || mediaErr != nil || cropErr != nil {
		bounds, boundsErr := instance.FPDF_GetPageBoundingBox(&requests.FPDF_GetPageBoundingBox{Page: ref})
		if boundsErr != nil || bounds == nil || frame.MediaBox != frame.CropBox {
			return errors.New("native PDF MediaBox or CropBox is unavailable")
		}
		actual, err := quantizeBounds(float64(bounds.Rect.Left), float64(bounds.Rect.Bottom), float64(bounds.Rect.Right), float64(bounds.Rect.Top))
		if err != nil {
			return err
		}
		if actual != frame.CropBox {
			return errors.New("native PDF bounding box differs from frame")
		}
		media = &responses.FPDFPage_GetMediaBox{Left: bounds.Rect.Left, Bottom: bounds.Rect.Bottom, Right: bounds.Rect.Right, Top: bounds.Rect.Top}
		crop = &responses.FPDFPage_GetCropBox{Left: bounds.Rect.Left, Bottom: bounds.Rect.Bottom, Right: bounds.Rect.Right, Top: bounds.Rect.Top}
	}
	rotation, err := instance.FPDFPage_GetRotation(&requests.FPDFPage_GetRotation{Page: ref})
	if err != nil {
		return fmt.Errorf("read native PDF rotation: %w", err)
	}
	actualMedia, err := quantizeBounds(float64(media.Left), float64(media.Bottom), float64(media.Right), float64(media.Top))
	if err != nil {
		return err
	}
	actualCrop, err := quantizeBounds(float64(crop.Left), float64(crop.Bottom), float64(crop.Right), float64(crop.Top))
	if err != nil {
		return err
	}
	if actualMedia != frame.MediaBox || actualCrop != frame.CropBox || int(rotation.PageRotation)*90 != frame.Rotation {
		return errors.New("native PDF MediaBox, CropBox, or rotation differs from frame")
	}
	size, err := instance.GetPageSize(&requests.GetPageSize{Page: ref})
	if err != nil || math.Abs(size.Width-physicalPoints(frame.Width)) > .001 || math.Abs(size.Height-physicalPoints(frame.Height)) > .001 {
		return errors.New("native PDF physical page differs from frame")
	}
	return nil
}

type nativeGlyphObservation struct {
	NativeGlyph

	object    references.FPDF_PAGEOBJECT
	objectKey nativeObjectKey
}

type nativeObjectKey [10]uint32

func inspectNativePage(ctx context.Context, instance pdfium.Pdfium, documentRef references.FPDF_DOCUMENT, page requests.Page, frame document.PageFrameV1, remaining int) (result NativeTextPage, resultErr error) {
	loaded, err := instance.FPDFText_LoadPage(&requests.FPDFText_LoadPage{Page: page})
	if err != nil {
		return result, fmt.Errorf("load native PDF text page: %w", err)
	}
	defer func() {
		_, closeErr := instance.FPDFText_ClosePage(&requests.FPDFText_ClosePage{TextPage: loaded.TextPage})
		resultErr = errors.Join(resultErr, closeErr)
	}()
	count, err := instance.FPDFText_CountChars(&requests.FPDFText_CountChars{TextPage: loaded.TextPage})
	if err != nil || count.Count < 0 || count.Count > remaining {
		return result, errors.New("native PDF text count is invalid or excessive")
	}
	result.Number = frame.Page
	actualText := map[references.FPDF_PAGEOBJECT]bool{}
	observed := make([]nativeGlyphObservation, 0, count.Count)
	for index := range count.Count {
		if index&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return result, err
			}
		}
		glyph, object, key, err := inspectNativeGlyph(instance, loaded.TextPage, index, actualText)
		if err != nil {
			return result, err
		}
		observed = append(observed, nativeGlyphObservation{NativeGlyph: glyph, object: object, objectKey: key})
	}
	var nonText [][4]int64
	if err := reconcileNativeVisibility(ctx, instance, documentRef, page, frame, observed, &nonText); err != nil {
		return result, err
	}
	result.NonTextBounds = nonText
	for _, glyph := range observed {
		result.Glyphs = append(result.Glyphs, glyph.NativeGlyph)
	}
	return result, nil
}

func inspectNativeGlyph(instance pdfium.Pdfium, textPage references.FPDF_TEXTPAGE, index int, actualText map[references.FPDF_PAGEOBJECT]bool) (NativeGlyph, references.FPDF_PAGEOBJECT, nativeObjectKey, error) {
	box, err := instance.FPDFText_GetCharBox(&requests.FPDFText_GetCharBox{TextPage: textPage, Index: index})
	if err != nil {
		return NativeGlyph{}, "", nativeObjectKey{}, fmt.Errorf("read native character box: %w", err)
	}
	bounds, err := quantizeBounds(box.Left, box.Bottom, box.Right, box.Top)
	if err != nil {
		return NativeGlyph{}, "", nativeObjectKey{}, err
	}
	g := NativeGlyph{Bounds: bounds}
	generated, err := instance.FPDFText_IsGenerated(&requests.FPDFText_IsGenerated{TextPage: textPage, Index: index})
	if err != nil {
		return g, "", nativeObjectKey{}, fmt.Errorf("inspect generated native character: %w", err)
	}
	mapping, err := instance.FPDFText_HasUnicodeMapError(&requests.FPDFText_HasUnicodeMapError{TextPage: textPage, Index: index})
	if err != nil {
		return g, "", nativeObjectKey{}, fmt.Errorf("inspect native Unicode mapping: %w", err)
	}
	value, err := instance.FPDFText_GetUnicode(&requests.FPDFText_GetUnicode{TextPage: textPage, Index: index})
	if err != nil {
		return g, "", nativeObjectKey{}, fmt.Errorf("read native character Unicode: %w", err)
	}
	object, err := instance.FPDFText_GetTextObject(&requests.FPDFText_GetTextObject{TextPage: textPage, Index: index})
	if err != nil {
		// PDFium reports missing object ownership as an error for generated
		// characters. Absence is an explicit gap, never accepted text.
		g.GapReason = "missing_text_object"
		if whitespace, ok := nativeWhitespaceRune(value.Unicode); ok {
			g.Text = whitespace
		}
		return g, "", nativeObjectKey{}, nil
	}
	if object.TextObject == "" {
		g.GapReason = "missing_text_object"
		if whitespace, ok := nativeWhitespaceRune(value.Unicode); ok {
			g.Text = whitespace
		}
		return g, "", nativeObjectKey{}, nil
	}
	key, err := nativeTextObjectKey(instance, object.TextObject)
	if err != nil {
		return g, object.TextObject, nativeObjectKey{}, err
	}
	if generated.IsGenerated {
		g.GapReason = "generated_character"
		if whitespace, ok := nativeWhitespaceRune(value.Unicode); ok {
			g.Text = whitespace
		}
		return g, object.TextObject, key, nil
	}
	if mapping.HasUnicodeMapError || value.Unicode == 0 || value.Unicode > utf8.MaxRune || value.Unicode >= 0xd800 && value.Unicode <= 0xdfff {
		g.GapReason = "unicode_mapping_error"
		return g, object.TextObject, key, nil
	}
	if present, ok := actualText[object.TextObject]; ok && present {
		g.GapReason = "actual_text_disagreement"
		return g, object.TextObject, key, nil
	}
	present, err := objectHasActualText(instance, object.TextObject)
	if err != nil {
		return g, object.TextObject, key, err
	}
	actualText[object.TextObject] = present
	if present {
		g.GapReason = "actual_text_disagreement"
		return g, object.TextObject, key, nil
	}
	mode, err := instance.FPDFTextObj_GetTextRenderMode(&requests.FPDFTextObj_GetTextRenderMode{PageObject: object.TextObject})
	if err != nil {
		return g, object.TextObject, key, fmt.Errorf("read native text rendering mode: %w", err)
	}
	if mode.TextRenderMode == enums.FPDF_TEXTRENDERMODE_INVISIBLE || mode.TextRenderMode == enums.FPDF_TEXTRENDERMODE_CLIP || mode.TextRenderMode == enums.FPDF_TEXTRENDERMODE_UNKNOWN {
		g.GapReason = "hidden_text"
		return g, object.TextObject, key, nil
	}
	fill, err := instance.FPDFText_GetFillColor(&requests.FPDFText_GetFillColor{TextPage: textPage, Index: index})
	if err != nil {
		return g, object.TextObject, key, fmt.Errorf("read native text fill color: %w", err)
	}
	stroke, err := instance.FPDFText_GetStrokeColor(&requests.FPDFText_GetStrokeColor{TextPage: textPage, Index: index})
	if err != nil {
		return g, object.TextObject, key, fmt.Errorf("read native text stroke color: %w", err)
	}
	fillActive := mode.TextRenderMode == 0 || mode.TextRenderMode == 2 || mode.TextRenderMode == 4 || mode.TextRenderMode == 6
	strokeActive := mode.TextRenderMode == 1 || mode.TextRenderMode == 2 || mode.TextRenderMode == 5 || mode.TextRenderMode == 6
	if (!fillActive || fill.A == 0) && (!strokeActive || stroke.A == 0) {
		g.GapReason = "transparent_text"
		return g, object.TextObject, key, nil
	}
	g.Text = string(rune(value.Unicode))
	return g, object.TextObject, key, nil
}

func validNativeWhitespace(value uint) bool {
	return value <= utf8.MaxRune && value != 0 && (value < 0xd800 || value > 0xdfff) && unicode.IsSpace(rune(value))
}

func nativeWhitespaceRune(value uint) (string, bool) {
	if !validNativeWhitespace(value) {
		return "", false
	}
	return string(rune(value)), true //nolint:gosec // validNativeWhitespace proves a valid Unicode scalar.
}

func nativeTextObjectKey(instance pdfium.Pdfium, object references.FPDF_PAGEOBJECT) (nativeObjectKey, error) {
	matrix, err := instance.FPDFPageObj_GetMatrix(&requests.FPDFPageObj_GetMatrix{PageObject: object})
	if err != nil {
		return nativeObjectKey{}, fmt.Errorf("read native text object matrix: %w", err)
	}
	bounds, err := instance.FPDFPageObj_GetBounds(&requests.FPDFPageObj_GetBounds{PageObject: object})
	if err != nil {
		return nativeObjectKey{}, fmt.Errorf("read native text object bounds: %w", err)
	}
	return nativeObjectKey{
		math.Float32bits(matrix.Matrix.A), math.Float32bits(matrix.Matrix.B), math.Float32bits(matrix.Matrix.C), math.Float32bits(matrix.Matrix.D), math.Float32bits(matrix.Matrix.E), math.Float32bits(matrix.Matrix.F),
		math.Float32bits(bounds.Left), math.Float32bits(bounds.Bottom), math.Float32bits(bounds.Right), math.Float32bits(bounds.Top),
	}, nil
}

// reconcileNativeVisibility proves that each otherwise acceptable character
// contributes pixels to the final rendered page. Removing one complete text
// object preserves every later paint operation, so a covered or clipped glyph
// produces no pixel delta and is retained only as an explicit gap.
func reconcileNativeVisibility(ctx context.Context, instance pdfium.Pdfium, documentRef references.FPDF_DOCUMENT, page requests.Page, frame document.PageFrameV1, glyphs []nativeGlyphObservation, nonText *[][4]int64) error {
	type objectGroup struct {
		object  references.FPDF_PAGEOBJECT
		indexes []int
	}
	groups := map[nativeObjectKey]objectGroup{}
	for index := range glyphs {
		if glyphs[index].GapReason == "" && glyphs[index].Text != "" && glyphs[index].object != "" {
			group := groups[glyphs[index].objectKey]
			if group.object == "" {
				group.object = glyphs[index].object
			}
			group.indexes = append(group.indexes, index)
			_, ok, err := nativePhysicalBounds(frame, glyphs[index].Bounds)
			if err != nil {
				return err
			}
			if !ok {
				glyphs[index].GapReason = "outside_crop_box"
				glyphs[index].Text = ""
			}
			groups[glyphs[index].objectKey] = group
		}
	}
	pageObjects, painted, err := nativePageObjects(ctx, instance, page)
	if err != nil {
		return err
	}
	annotations, err := instance.FPDFPage_GetAnnotCount(&requests.FPDFPage_GetAnnotCount{Page: page})
	if err != nil {
		return fmt.Errorf("inspect native page annotations: %w", err)
	}
	if annotations.Count < 0 {
		return errors.New("native page annotation count is invalid")
	}
	if annotations.Count > 0 {
		// Annotation and widget appearance streams are included in the rendered
		// pixels but absent from FPDFPage_CountObjects. Treat their possible
		// visible text as an explicit full-page coverage gap.
		painted = append(painted, frame.CropBox)
	}
	*nonText = painted
	for key, group := range groups {
		object, ok := pageObjects[key]
		if !ok || object == "" {
			for _, index := range group.indexes {
				glyphs[index].GapReason = "ambiguous_or_nested_text_object"
				glyphs[index].Text = ""
			}
			delete(groups, key)
			continue
		}
		group.object = object
		groups[key] = group
	}
	keys := make([]nativeObjectKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		for index := range keys[i] {
			if keys[i][index] != keys[j][index] {
				return keys[i][index] < keys[j][index]
			}
		}
		return false
	})
	baseline, baselineCleanup, err := renderNativePage(instance, documentRef, page, frame)
	if err != nil {
		return err
	}
	defer baselineCleanup()
	type changedObject struct {
		object references.FPDF_PAGEOBJECT
		mode   enums.FPDF_TEXT_RENDERMODE
	}
	disabled := make([]changedObject, 0, len(keys))
	restore := func() error {
		var restoreErr error
		for _, d := range slices.Backward(disabled) {
			_, err := instance.FPDFTextObj_SetTextRenderMode(&requests.FPDFTextObj_SetTextRenderMode{PageObject: d.object, TextRenderMode: d.mode})
			restoreErr = errors.Join(restoreErr, err)
		}
		disabled = disabled[:0]
		return restoreErr
	}
	restored := false
	defer func() {
		if !restored {
			_ = restore()
		}
	}()
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		object := groups[key].object
		mode, err := instance.FPDFTextObj_GetTextRenderMode(&requests.FPDFTextObj_GetTextRenderMode{PageObject: object})
		if err != nil {
			return fmt.Errorf("read native text mode for visibility proof: %w", err)
		}
		if _, err := instance.FPDFTextObj_SetTextRenderMode(&requests.FPDFTextObj_SetTextRenderMode{PageObject: object, TextRenderMode: enums.FPDF_TEXTRENDERMODE_INVISIBLE}); err != nil {
			return fmt.Errorf("disable native text object for visibility proof: %w", err)
		}
		disabled = append(disabled, changedObject{object: object, mode: mode.TextRenderMode})
	}
	noText, noTextCleanup, err := renderNativePage(instance, documentRef, page, frame)
	if err != nil {
		return err
	}
	defer noTextCleanup()
	if err := restore(); err != nil {
		return fmt.Errorf("restore native text objects after visibility proof: %w", err)
	}
	restored = true
	restoredImage, restoredCleanup, err := renderNativePage(instance, documentRef, page, frame)
	if err != nil {
		return err
	}
	defer restoredCleanup()
	if equal, err := nativeImagesEqual(ctx, baseline, restoredImage); err != nil {
		return err
	} else if !equal {
		return errors.New("native page did not restore exactly after visibility proof")
	}
	glyphRects := make([]image.Rectangle, 0, len(glyphs))
	for index := range glyphs {
		if glyphs[index].GapReason != "" || glyphs[index].Text == "" {
			continue
		}
		bounds, ok, boundsErr := nativePhysicalBounds(frame, glyphs[index].Bounds)
		if boundsErr != nil {
			return boundsErr
		}
		if ok {
			glyphRects = append(glyphRects, physicalPixelRect(baseline.Bounds(), frame, bounds))
		}
	}
	ambiguity, err := nativeGlyphAmbiguity(ctx, baseline.Bounds(), glyphRects)
	if err != nil {
		return err
	}
	visits := int64(0)
	for _, key := range keys {
		group := groups[key]
		for _, index := range group.indexes {
			box, ok, err := nativePhysicalBounds(frame, glyphs[index].Bounds)
			if err != nil {
				return err
			}
			rect := physicalPixelRect(baseline.Bounds(), frame, box)
			visible, visibilityErr := nativeGlyphHasUniqueDelta(ctx, baseline, noText, rect, ambiguity, &visits)
			if visibilityErr != nil {
				return visibilityErr
			}
			if !ok || !visible {
				glyphs[index].GapReason = "no_unambiguous_rendered_pixel_contribution"
				if !nativeWhitespaceText(glyphs[index].Text) {
					glyphs[index].Text = ""
				}
			}
		}
	}
	return nil
}

func nativeWhitespaceText(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if !unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func nativePageObjects(ctx context.Context, instance pdfium.Pdfium, page requests.Page) (map[nativeObjectKey]references.FPDF_PAGEOBJECT, [][4]int64, error) {
	count, err := instance.FPDFPage_CountObjects(&requests.FPDFPage_CountObjects{Page: page})
	if err != nil || count.Count < 0 || count.Count > maxTextBytes {
		return nil, nil, errors.New("native page object count is invalid or excessive")
	}
	result := make(map[nativeObjectKey]references.FPDF_PAGEOBJECT)
	duplicates := make(map[nativeObjectKey]bool)
	var nonText [][4]int64
	for index := range count.Count {
		if index&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}
		object, err := instance.FPDFPage_GetObject(&requests.FPDFPage_GetObject{Page: page, Index: index})
		if err != nil {
			return nil, nil, fmt.Errorf("read native page object: %w", err)
		}
		kind, err := instance.FPDFPageObj_GetType(&requests.FPDFPageObj_GetType{PageObject: object.PageObject})
		if err != nil {
			return nil, nil, fmt.Errorf("read native page object type: %w", err)
		}
		if kind.Type != enums.FPDF_PAGEOBJ_TEXT {
			if kind.Type == enums.FPDF_PAGEOBJ_UNKNOWN {
				return nil, nil, errors.New("native page contains an unknown painted object")
			}
			bounds, boundsErr := instance.FPDFPageObj_GetBounds(&requests.FPDFPageObj_GetBounds{PageObject: object.PageObject})
			if boundsErr != nil {
				return nil, nil, fmt.Errorf("read non-text page object bounds: %w", boundsErr)
			}
			quantized, boundsErr := quantizeBounds(float64(bounds.Left), float64(bounds.Bottom), float64(bounds.Right), float64(bounds.Top))
			if boundsErr != nil {
				return nil, nil, boundsErr
			}
			nonText = append(nonText, quantized)
			continue
		}
		key, err := nativeTextObjectKey(instance, object.PageObject)
		if err != nil {
			return nil, nil, err
		}
		if _, exists := result[key]; exists {
			duplicates[key] = true
		}
		result[key] = object.PageObject
	}
	for key := range duplicates {
		delete(result, key)
	}
	return result, nonText, nil
}

type physicalBounds struct{ x0, y0, x1, y1 int64 }

func renderNativePage(instance pdfium.Pdfium, documentRef references.FPDF_DOCUMENT, page requests.Page, frame document.PageFrameV1) (image.Image, func(), error) {
	axis := func(physical int64) (int, error) {
		value := new(big.Int).Mul(big.NewInt(physical), big.NewInt(300))
		value.Add(value, big.NewInt(9999)).Quo(value, big.NewInt(10000))
		if !value.IsInt64() || value.Sign() <= 0 || value.Int64() > document.MaxPageAxis {
			return 0, errors.New("native visibility raster axis exceeds bounds")
		}
		return int(value.Int64()), nil
	}
	width, err := axis(frame.Width)
	if err != nil {
		return nil, nil, err
	}
	height, err := axis(frame.Height)
	if err != nil || int64(width) > document.MaxPagePixels/int64(height) {
		return nil, nil, errors.New("native visibility raster exceeds pixel bounds")
	}
	rendered, err := instance.RenderPageInPixels(&requests.RenderPageInPixels{Page: page, Width: width, Height: height, RenderForm: true, Document: &documentRef, RenderFlags: enums.FPDF_RENDER_FLAG_ANNOT})
	if err != nil {
		return nil, nil, fmt.Errorf("render native visibility page: %w", err)
	}
	return rendered.Result.RenderedImage, rendered.Cleanup, nil
}

func physicalPixelRect(imageBounds image.Rectangle, frame document.PageFrameV1, box physicalBounds) image.Rectangle {
	width, height := int64(imageBounds.Dx()), int64(imageBounds.Dy())
	scale := func(value, pixels, physical int64, ceil bool) int64 {
		product := new(big.Int).Mul(big.NewInt(value), big.NewInt(pixels))
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(product, big.NewInt(physical), remainder)
		if ceil && remainder.Sign() != 0 {
			quotient.Add(quotient, big.NewInt(1))
		}
		return quotient.Int64()
	}
	x0 := max(int64(0), scale(box.x0, width, frame.Width, false)-1)
	y0 := max(int64(0), scale(box.y0, height, frame.Height, false)-1)
	x1 := min(width, scale(box.x1, width, frame.Width, true)+1)
	y1 := min(height, scale(box.y1, height, frame.Height, true)+1)
	return image.Rect(imageBounds.Min.X+int(x0), imageBounds.Min.Y+int(y0), imageBounds.Min.X+int(x1), imageBounds.Min.Y+int(y1)).Intersect(imageBounds)
}

const maxNativeVisibilityVisits int64 = 4 * document.MaxPagePixels

func nativeGlyphAmbiguity(ctx context.Context, bounds image.Rectangle, glyphs []image.Rectangle) ([]uint8, error) {
	counts := make([]uint8, bounds.Dx()*bounds.Dy())
	var visits int64
	for _, rect := range glyphs {
		rect = rect.Intersect(bounds)
		for y := rect.Min.Y; y < rect.Max.Y; y++ {
			if visits&65535 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			for x := rect.Min.X; x < rect.Max.X; x++ {
				visits++
				if visits > maxNativeVisibilityVisits {
					return nil, errors.New("native visibility overlap work exceeds bounds")
				}
				offset := (y-bounds.Min.Y)*bounds.Dx() + x - bounds.Min.X
				if counts[offset] < 2 {
					counts[offset]++
				}
			}
		}
	}
	return counts, nil
}

func nativeGlyphHasUniqueDelta(ctx context.Context, baseline, noText image.Image, glyph image.Rectangle, ambiguity []uint8, visits *int64) (bool, error) {
	bounds := baseline.Bounds()
	for y := glyph.Min.Y; y < glyph.Max.Y; y++ {
		for x := glyph.Min.X; x < glyph.Max.X; x++ {
			(*visits)++
			if *visits&65535 == 0 {
				if err := ctx.Err(); err != nil {
					return false, err
				}
			}
			if *visits > maxNativeVisibilityVisits {
				return false, errors.New("native glyph visibility work exceeds bounds")
			}
			if baseline.At(x, y) == noText.At(x, y) {
				continue
			}
			offset := (y-bounds.Min.Y)*bounds.Dx() + x - bounds.Min.X
			if ambiguity[offset] == 1 {
				return true, nil
			}
		}
	}
	return false, nil
}

func nativeImagesEqual(ctx context.Context, left, right image.Image) (bool, error) {
	if left.Bounds() != right.Bounds() {
		return false, nil
	}
	for y := left.Bounds().Min.Y; y < left.Bounds().Max.Y; y++ {
		if y&31 == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		for x := left.Bounds().Min.X; x < left.Bounds().Max.X; x++ {
			if left.At(x, y) != right.At(x, y) {
				return false, nil
			}
		}
	}
	return true, nil
}

// PhysicalBox applies the crop, page transform, and outward rounding used by
// the native visibility proof, then binds the result to aligned-map authority.
func PhysicalBox(frame document.PageFrameV1, raw [4]int64, page redaction.Page) (redaction.Box, bool, error) {
	bounds, ok, err := nativePhysicalBounds(frame, raw)
	if err != nil || !ok {
		return redaction.Box{}, ok, err
	}
	bounds.x0, bounds.y0 = max(int64(0), bounds.x0), max(int64(0), bounds.y0)
	bounds.x1, bounds.y1 = min(page.Width, bounds.x1), min(page.Height, bounds.y1)
	if bounds.x1 <= bounds.x0 || bounds.y1 <= bounds.y0 {
		return redaction.Box{}, false, nil
	}
	return redaction.Box{Page: page.Number, FrameSHA256: page.FrameSHA256, X0: bounds.x0, Y0: bounds.y0, X1: bounds.x1, Y1: bounds.y1}, true, nil
}

func nativePhysicalBounds(frame document.PageFrameV1, raw [4]int64) (physicalBounds, bool, error) {
	left, bottom := max(raw[0], frame.CropBox[0]), max(raw[1], frame.CropBox[1])
	right, top := min(raw[2], frame.CropBox[2]), min(raw[3], frame.CropBox[3])
	if right <= left || top <= bottom {
		return physicalBounds{}, false, nil
	}
	points := [][2]int64{{left, bottom}, {left, top}, {right, bottom}, {right, top}}
	var minX, minY, maxX, maxY *big.Rat
	for i, point := range points {
		value := func(a, b, c document.PageRational) *big.Rat {
			x := new(big.Rat).Mul(new(big.Rat).SetInt64(point[0]), new(big.Rat).SetFrac64(a.Numerator, a.Denominator))
			y := new(big.Rat).Mul(new(big.Rat).SetInt64(point[1]), new(big.Rat).SetFrac64(b.Numerator, b.Denominator))
			return new(big.Rat).Add(new(big.Rat).Add(x, y), new(big.Rat).SetFrac64(c.Numerator, c.Denominator))
		}
		x, y := value(frame.Transform[0], frame.Transform[2], frame.Transform[4]), value(frame.Transform[1], frame.Transform[3], frame.Transform[5])
		if i == 0 || x.Cmp(minX) < 0 {
			minX = x
		}
		if i == 0 || x.Cmp(maxX) > 0 {
			maxX = x
		}
		if i == 0 || y.Cmp(minY) < 0 {
			minY = y
		}
		if i == 0 || y.Cmp(maxY) > 0 {
			maxY = y
		}
	}
	x0, err := nativeRatRound(minX, false)
	if err != nil {
		return physicalBounds{}, false, err
	}
	y0, err := nativeRatRound(minY, false)
	if err != nil {
		return physicalBounds{}, false, err
	}
	x1, err := nativeRatRound(maxX, true)
	if err != nil {
		return physicalBounds{}, false, err
	}
	y1, err := nativeRatRound(maxY, true)
	if err != nil {
		return physicalBounds{}, false, err
	}
	x0, y0, x1, y1 = max(0, x0), max(0, y0), min(frame.Width, x1), min(frame.Height, y1)
	return physicalBounds{x0, y0, x1, y1}, x1 > x0 && y1 > y0, nil
}

func nativeRatRound(value *big.Rat, ceil bool) (int64, error) {
	q, remainder := new(big.Int), new(big.Int)
	q.QuoRem(value.Num(), value.Denom(), remainder)
	if ceil && value.Sign() > 0 && remainder.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	if !ceil && value.Sign() < 0 && remainder.Sign() != 0 {
		q.Sub(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, errors.New("native visibility box exceeds bounds")
	}
	return q.Int64(), nil
}

func objectHasActualText(instance pdfium.Pdfium, object references.FPDF_PAGEOBJECT) (bool, error) {
	marks, err := instance.FPDFPageObj_CountMarks(&requests.FPDFPageObj_CountMarks{PageObject: object})
	if err != nil {
		return false, fmt.Errorf("count native text marks: %w", err)
	}
	for i := range marks.Count {
		mark, err := instance.FPDFPageObj_GetMark(&requests.FPDFPageObj_GetMark{PageObject: object, Index: uint64(i)})
		if err != nil {
			return false, fmt.Errorf("read native text mark: %w", err)
		}
		params, err := instance.FPDFPageObjMark_CountParams(&requests.FPDFPageObjMark_CountParams{PageObjectMark: mark.Mark})
		if err != nil {
			return false, fmt.Errorf("count native text mark parameters: %w", err)
		}
		for j := range params.Count {
			key, err := instance.FPDFPageObjMark_GetParamKey(&requests.FPDFPageObjMark_GetParamKey{PageObjectMark: mark.Mark, Index: uint64(j)})
			if err != nil {
				return false, fmt.Errorf("read native text mark parameter: %w", err)
			}
			if key.Key == "ActualText" {
				return true, nil
			}
		}
	}
	return false, nil
}

func quantizePoint(value float64) (int64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > math.MaxInt64/10000 {
		return 0, errors.New("native PDF geometry is not bounded and finite")
	}
	return int64(math.Round(value * 10000)), nil
}

func quantizeBounds(values ...float64) ([4]int64, error) {
	if len(values) != 4 {
		return [4]int64{}, errors.New("native PDF box has wrong arity")
	}
	var result [4]int64
	for index, value := range values {
		quantized, err := quantizePoint(value)
		if err != nil {
			return [4]int64{}, err
		}
		result[index] = quantized
	}
	return result, nil
}
