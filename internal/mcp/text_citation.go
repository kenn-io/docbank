package mcp

import (
	"context"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/daemonconn"
)

var textCitationTool = toolDefinition{
	name: "resolve_text_citation", title: "Resolve text citation",
	description: "Reopen an exact Unicode range from retained sanitized Markdown, including " +
		"historical versions. Verifies the full rendition before returning text. " +
		"The reference retains no evidence; trash, prune or purge can make it unavailable.",
	schemas: textCitationSchemas, idempotent: true,
}

type textCitationOutput struct {
	privateCache
	document.ResolvedTextCitation
}

func resolveTextCitation(
	ctx context.Context, lease *daemonLease, raw []byte,
) (textCitationOutput, error) {
	var citation document.TextCitation
	if err := decodeReadArguments(raw, &citation); err != nil {
		return textCitationOutput{}, err
	}
	if err := document.ValidateTextCitation(citation); err != nil {
		return textCitationOutput{}, invalidToolArgumentsError()
	}
	result, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (
		document.ResolvedTextCitation, error,
	) {
		return c.ResolveTextCitation(ctx, citation)
	})
	if err != nil {
		return textCitationOutput{}, err
	}
	return textCitationOutput{privateCache: newPrivateCache(), ResolvedTextCitation: result}, nil
}
