package mcp

import (
	"context"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/grounding"
)

var checkAnswerTool = toolDefinition{
	name: "check_answer", title: "Check answer figures",
	description: "Resolve citations and check whether answer numbers and identifiers occur in their cited text. This does not prove the figures refer to the right subject or that other claims are true. Send plain text with at most 512 figures and 512 marker references.",
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
		cache := make(map[document.TextCitation]grounding.Citation)
		result := make([]grounding.Citation, 0, len(input.Citations))
		for i, citation := range input.Citations {
			entry, ok := cache[citation]
			if !ok {
				quote, err := c.ResolveTextCitation(ctx, citation)
				if err != nil {
					facts, ok := daemonconn.ExtractProblemFacts(err)
					if !ok || !checkAnswerCitationProblem(facts.Code) {
						return nil, err
					}
					entry.Err = facts.Code
				} else {
					entry.Text, entry.TextSHA256 = quote.Text, quote.TextSHA256
				}
				cache[citation] = entry
			}
			entry.Index = i + 1
			result = append(result, entry)
		}
		return result, nil
	})
	if err != nil {
		return checkAnswerOutput{}, err
	}
	return checkAnswerOutput{privateCache: newPrivateCache(), Report: answer.Check(resolved)}, nil
}

func checkAnswerCitationProblem(code string) bool {
	switch code {
	case "citation_unavailable", "invalid_citation_range", "citation_limit", "citation_integrity":
		return true
	default:
		return false
	}
}
