package mcp

import (
	"context"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/grounding"
)

var checkAnswerTool = toolDefinition{
	name: "check_answer", title: "Check answer figures",
	description: "Resolve citations and compare whole answer figures by exact spelling with any cited text. A match confirms occurrence only. Send plain text with at most 512 figures and 512 [n] markers.",
	schemas:     checkAnswerSchemas, idempotent: true,
}

type checkAnswerInput struct {
	Answer    string                  `json:"answer"`
	Citations []document.TextCitation `json:"citations"`
}
type checkAnswerOutput struct {
	privateCache
	grounding.Report
}

func checkAnswer(ctx context.Context, lease *daemonLease, raw []byte) (checkAnswerOutput, error) {
	var input checkAnswerInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return checkAnswerOutput{}, err
	}
	for _, citation := range input.Citations {
		if document.ValidateTextCitation(citation) != nil {
			return checkAnswerOutput{}, invalidToolArgumentsError()
		}
	}
	answer, err := grounding.Prepare(input.Answer)
	if err != nil {
		return checkAnswerOutput{}, invalidToolArgumentsError()
	}
	resolved, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) ([]grounding.Citation, error) {
		result := make([]grounding.Citation, 0, len(input.Citations))
		for _, citation := range input.Citations {
			var entry grounding.Citation
			quote, err := c.ResolveTextCitation(ctx, citation)
			if err != nil {
				code, _ := stableDomainError(err)
				if code != "citation_unavailable" {
					return nil, err
				}
				entry.Err = code
			} else {
				entry.Text, entry.TextSHA256 = quote.Text, quote.TextSHA256
			}
			result = append(result, entry)
		}
		return result, nil
	})
	if err != nil {
		return checkAnswerOutput{}, err
	}
	return checkAnswerOutput{privateCache: newPrivateCache(), Report: answer.Check(resolved)}, nil
}
