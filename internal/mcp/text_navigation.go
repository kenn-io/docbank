package mcp

import (
	"context"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/daemonconn"
)

var textOutlineTool = toolDefinition{
	name: "get_text_outline", title: "Read retained text outline",
	description: "Read headings and whole-section bounds in exact retained Markdown. " +
		"Use citation range [0,1) when you have no particular quote. " +
		"Plaintext # comments can become Markdown headings; this is not source-language structure. " +
		"Each page verifies and parses the rendition again; navigation is limited to 4 MiB.",
	schemas: textOutlineSchemas, idempotent: true,
}

var textSectionTool = toolDefinition{
	name: "read_text_section", title: "Read retained text section",
	description: "Read an exact Markdown section, including subsections, " +
		"using bounds from get_text_outline. " +
		"Offsets are code points relative to the section; " +
		"advance to next_offset until complete is true. " +
		"Partial text stays partial if a later read fails. Each page rechecks retained evidence. " +
		"Plaintext # comments may be stored headings, not source-language sections.",
	schemas: textSectionSchemas, idempotent: true,
}

type textOutlineOutput struct {
	privateCache
	document.TextOutlinePage
}

type textSectionOutput struct {
	privateCache
	document.TextSectionPage
}

func getTextOutline(
	ctx context.Context, lease *daemonLease, raw []byte,
) (textOutlineOutput, error) {
	var request document.TextOutlineRequest
	if err := decodeReadArguments(raw, &request); err != nil {
		return textOutlineOutput{}, err
	}
	if err := document.ValidateTextOutlineRequest(request); err != nil {
		return textOutlineOutput{}, invalidToolArgumentsError()
	}
	result, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (
		document.TextOutlinePage, error,
	) {
		return c.GetTextOutline(ctx, request)
	})
	if err != nil {
		return textOutlineOutput{}, err
	}
	return textOutlineOutput{privateCache: newPrivateCache(), TextOutlinePage: result}, nil
}

func readTextSection(
	ctx context.Context, lease *daemonLease, raw []byte,
) (textSectionOutput, error) {
	var request document.TextSectionRequest
	if err := decodeReadArguments(raw, &request); err != nil {
		return textSectionOutput{}, err
	}
	if err := document.ValidateTextSectionRequest(request); err != nil {
		return textSectionOutput{}, invalidToolArgumentsError()
	}
	result, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (
		document.TextSectionPage, error,
	) {
		return c.ReadTextSection(ctx, request)
	})
	if err != nil {
		return textSectionOutput{}, err
	}
	return textSectionOutput{privateCache: newPrivateCache(), TextSectionPage: result}, nil
}
