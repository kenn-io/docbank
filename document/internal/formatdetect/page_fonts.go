package formatdetect

import (
	"context"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// Only the code domain comes from this parser; PDFium decides what each code paints.
func pageSimpleFontsOnly(ctx *model.Context, resources types.Dict) bool {
	fonts, err := ctx.DereferenceDict(resources["Font"])
	if err != nil {
		return false
	}
	for _, object := range fonts {
		if !simpleFont(ctx, object) {
			return false
		}
	}
	states, err := ctx.DereferenceDict(resources["ExtGState"])
	if err != nil {
		return false
	}
	for _, object := range states {
		state, err := ctx.DereferenceDict(object)
		if err != nil || state == nil {
			return false
		}
		object, present := state["Font"]
		if !present {
			continue
		}
		font, err := ctx.DereferenceArray(object)
		if err != nil || len(font) != 2 || !simpleFont(ctx, font[0]) {
			return false
		}
		if _, err := ctx.DereferenceNumber(font[1]); err != nil {
			return false
		}
	}
	return true
}

func simpleFont(ctx *model.Context, object types.Object) bool {
	font, err := ctx.DereferenceDict(object)
	if err != nil || font == nil {
		return false
	}
	subtype, err := ctx.Dereference(font["Subtype"])
	if err != nil {
		return false
	}
	switch subtype {
	case types.Name("Type1"), types.Name("MMType1"), types.Name("TrueType"):
		return true
	default:
		return false
	}
}

func unvalidatedPageFontDomains(ctx *model.Context, maxPages int) []bool {
	if err := ctx.EnsurePageCount(); err != nil || ctx.PageCount < 1 || ctx.PageCount > maxPages {
		return nil
	}
	domains := make([]bool, ctx.PageCount)
	for page := 1; page <= ctx.PageCount; page++ {
		_, _, attrs, err := ctx.PageDict(context.Background(), page, false)
		if err == nil {
			domains[page-1] = pageSimpleFontsOnly(ctx, attrs.Resources)
		}
	}
	return domains
}
