package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/processing"
)

type PassageResolveRequest struct {
	Ref      document.PassageRefV1 `json:"ref"`
	MaxBytes int                   `json:"max_bytes,omitzero" minimum:"1" maximum:"262144" default:"32768"`
}

type PassageResolution struct {
	Availability  string                      `json:"availability" enum:"available"`
	Freshness     string                      `json:"freshness" enum:"current,historical"`
	PassageID     string                      `json:"passage_id" pattern:"^[0-9a-f]{64}$"`
	Ref           document.PassageRefV1       `json:"ref"`
	Text          string                      `json:"text"`
	SectionPath   []string                    `json:"section_path"`
	SourceLocator *document.EvidenceLocatorV1 `json:"source_locator,omitempty"`
	SourcePath    string                      `json:"source_path"`
}

type PassageOutlineRequest struct {
	Ref document.PassageRefV1 `json:"ref"`
}

type PassageOutline struct {
	BodySHA256       string                  `json:"body_sha256" pattern:"^[0-9a-f]{64}$"`
	RenditionBuildID string                  `json:"rendition_build_id" pattern:"^[0-9a-f]{64}$"`
	Sections         []PassageOutlineSection `json:"sections"`
}

type PassageOutlineSection struct {
	Key                string                      `json:"key" pattern:"^[0-9a-f]{64}$"`
	Title              string                      `json:"title" maxLength:"8192"`
	Level              int                         `json:"level" minimum:"0" maximum:"6"`
	Occurrence         int                         `json:"occurrence" minimum:"1"`
	ByteStart          int                         `json:"byte_start" minimum:"0"`
	ByteEnd            int                         `json:"byte_end" minimum:"0"`
	OwnByteEnd         int                         `json:"own_byte_end" minimum:"0"`
	EstimatedUTF8Bytes int                         `json:"estimated_utf8_bytes" minimum:"0"`
	EstimatedRunes     int                         `json:"estimated_runes" minimum:"0"`
	ChildCount         int                         `json:"child_count" minimum:"0"`
	Preamble           bool                        `json:"preamble"`
	SourceLocator      *document.EvidenceLocatorV1 `json:"source_locator,omitempty"`
	Children           []PassageOutlineSection     `json:"children"`
}

type PassageReadSectionRequest struct {
	Ref             document.PassageRefV1 `json:"ref"`
	NavigationKey   string                `json:"navigation_key" pattern:"^[0-9a-f]{64}$"`
	IncludeChildren bool                  `json:"include_children"`
	MaxBytes        int                   `json:"max_bytes,omitzero" minimum:"1" maximum:"262144" default:"32768"`
	Continuation    string                `json:"continuation,omitzero" maxLength:"4096"`
}

type PassageSectionSelection struct {
	Key             string                      `json:"key" pattern:"^[0-9a-f]{64}$"`
	Title           string                      `json:"title" maxLength:"8192"`
	Level           int                         `json:"level" minimum:"0" maximum:"6"`
	ByteStart       int                         `json:"byte_start" minimum:"0"`
	ByteEnd         int                         `json:"byte_end" minimum:"0"`
	IncludeChildren bool                        `json:"include_children"`
	SourceLocator   *document.EvidenceLocatorV1 `json:"source_locator,omitempty"`
}

type PassageSectionPage struct {
	BodySHA256       string                  `json:"body_sha256" pattern:"^[0-9a-f]{64}$"`
	RenditionBuildID string                  `json:"rendition_build_id" pattern:"^[0-9a-f]{64}$"`
	Section          PassageSectionSelection `json:"section"`
	Text             string                  `json:"text"`
	Ref              *document.PassageRefV1  `json:"ref,omitempty"`
	PageStart        int                     `json:"page_start" minimum:"0"`
	PageEnd          int                     `json:"page_end" minimum:"0"`
	Complete         bool                    `json:"complete"`
	Continuation     string                  `json:"continuation,omitzero"`
}

func registerPassageRoutes(api huma.API, d Deps) {
	type resolveOutput struct{ Body PassageResolution }
	huma.Register(api, huma.Operation{OperationID: "resolvePassage", Method: http.MethodPost,
		Path: "/api/v1/passages/resolve", Summary: "Resolve one exact retained Markdown passage",
		MaxBodyBytes: 16 << 10}, func(ctx context.Context, input *struct {
		Body PassageResolveRequest
	}) (*resolveOutput, error) {
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		resolved, err := d.Processing.ResolvePassage(ctx, processing.PassageResolveRequest{
			Ref: input.Body.Ref, MaxBytes: input.Body.MaxBytes,
		})
		if err != nil {
			return nil, passageResolveError(err)
		}
		return &resolveOutput{Body: passageResolutionFromProcessing(resolved)}, nil
	})

	type outlineOutput struct{ Body PassageOutline }
	huma.Register(api, huma.Operation{OperationID: "outlineDocument", Method: http.MethodPost,
		Path: "/api/v1/documents/outline", Summary: "Outline one exact retained Markdown rendition",
		MaxBodyBytes: 16 << 10}, func(ctx context.Context, input *struct {
		Body PassageOutlineRequest
	}) (*outlineOutput, error) {
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		outline, err := d.Processing.PassageOutline(ctx, processing.OutlineRequest{Ref: input.Body.Ref})
		if err != nil {
			return nil, passageResolveError(err)
		}
		return &outlineOutput{Body: passageOutlineFromProcessing(outline)}, nil
	})

	type sectionOutput struct{ Body PassageSectionPage }
	huma.Register(api, huma.Operation{OperationID: "readPassageSection", Method: http.MethodPost,
		Path: "/api/v1/passages/read-section", Summary: "Read a bounded page from one exact section",
		MaxBodyBytes: 16 << 10}, func(ctx context.Context, input *struct {
		Body PassageReadSectionRequest
	}) (*sectionOutput, error) {
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		page, err := d.Processing.ReadPassageSection(ctx, processing.SectionReadRequest{
			Ref: input.Body.Ref, NavigationKey: input.Body.NavigationKey,
			IncludeChildren: input.Body.IncludeChildren, MaxBytes: input.Body.MaxBytes,
			Continuation: input.Body.Continuation,
		})
		if err != nil {
			return nil, passageResolveError(err)
		}
		return &sectionOutput{Body: passageSectionPageFromProcessing(page)}, nil
	})
}

func passageResolutionFromProcessing(value processing.PassageResolution) PassageResolution {
	var locator *document.EvidenceLocatorV1
	if value.SourceLocator != nil {
		copied := *value.SourceLocator
		locator = &copied
	}
	return PassageResolution{Availability: value.Availability, Freshness: value.Freshness,
		PassageID: value.PassageID, Ref: value.Ref, Text: value.Text,
		SectionPath:   append([]string(nil), value.SectionPath...),
		SourceLocator: locator, SourcePath: value.SourcePath}
}

func passageOutlineFromProcessing(value processing.PassageOutline) PassageOutline {
	sections := make([]PassageOutlineSection, len(value.Sections))
	for index := range value.Sections {
		sections[index] = passageOutlineSectionFromProcessing(value.Sections[index])
	}
	return PassageOutline{BodySHA256: value.BodySHA256,
		RenditionBuildID: value.RenditionBuildID, Sections: sections}
}

func passageOutlineSectionFromProcessing(value processing.OutlineSection) PassageOutlineSection {
	children := make([]PassageOutlineSection, len(value.Children))
	for index := range value.Children {
		children[index] = passageOutlineSectionFromProcessing(value.Children[index])
	}
	var locator *document.EvidenceLocatorV1
	if value.SourceLocator != nil {
		copied := *value.SourceLocator
		locator = &copied
	}
	return PassageOutlineSection{Key: value.Key, Title: value.Title, Level: value.Level,
		Occurrence: value.Occurrence, ByteStart: value.ByteStart, ByteEnd: value.ByteEnd,
		OwnByteEnd: value.OwnByteEnd, EstimatedUTF8Bytes: value.EstimatedUTF8Bytes,
		EstimatedRunes: value.EstimatedRunes, ChildCount: value.ChildCount,
		Preamble: value.Preamble, SourceLocator: locator, Children: children}
}

func passageSectionPageFromProcessing(value processing.SectionReadResult) PassageSectionPage {
	selection := PassageSectionSelection{Key: value.Section.Key, Title: value.Section.Title,
		Level: value.Section.Level, ByteStart: value.Section.ByteStart, ByteEnd: value.Section.ByteEnd,
		IncludeChildren: value.Section.IncludeChildren}
	if value.Section.SourceLocator != nil {
		copied := *value.Section.SourceLocator
		selection.SourceLocator = &copied
	}
	var ref *document.PassageRefV1
	if value.Ref != nil {
		copied := *value.Ref
		ref = &copied
	}
	return PassageSectionPage{BodySHA256: value.BodySHA256,
		RenditionBuildID: value.RenditionBuildID, Section: selection, Text: value.Text,
		Ref: ref, PageStart: value.PageStart, PageEnd: value.PageEnd,
		Complete: value.Complete, Continuation: value.Continuation}
}

func passageResolveError(err error) error {
	switch {
	case errors.Is(err, processing.ErrSectionNotFound):
		return NewError(http.StatusNotFound, "section_unavailable",
			"The exact authorized section is not available.")
	case errors.Is(err, processing.ErrSectionContinuation):
		return NewError(http.StatusConflict, "section_changed",
			"The section continuation no longer matches the exact authorized section.")
	case errors.Is(err, processing.ErrSectionBudget):
		return NewError(http.StatusUnprocessableEntity, "section_budget_too_small",
			"The response budget is too small for a section page and its metadata.")
	case errors.Is(err, processing.ErrOutlineTooLarge):
		return NewError(http.StatusUnprocessableEntity, "outline_too_large",
			"The document outline exceeds the bounded response limit.")
	case errors.Is(err, processing.ErrPassageInvalid):
		return NewError(http.StatusUnprocessableEntity, "invalid_passage",
			"The passage reference or read budget is invalid.")
	case errors.Is(err, processing.ErrPassageUnauthorized),
		errors.Is(err, processing.ErrPassageUnavailable):
		// Deliberately merge authorization and retention failures so the route
		// does not disclose whether a hidden source exists.
		return NewError(http.StatusNotFound, "passage_unavailable",
			"The exact authorized passage is not available.")
	case errors.Is(err, processing.ErrPassageCorrupt):
		return NewError(http.StatusInternalServerError, "passage_corrupt",
			"The retained passage failed identity verification.")
	default:
		return NewError(http.StatusInternalServerError, "internal",
			"The passage could not be resolved.")
	}
}
