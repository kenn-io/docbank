package mcp

import (
	"context"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/grounding"
)

var findConflictsTool = toolDefinition{
	name: "find_conflicts", title: "Find differing figures",
	description: "Compare money, percentages, and durations in lexical search excerpts with otherwise identical sentence text. Returns disagreements as leads to inspect, with source dates and evidence IDs. Skips ambiguous or incomplete sentences and name-only evidence; never chooses a winner.",
	schemas:     findConflictsSchemas,
}

type conflictPassage struct {
	Rank             int      `json:"rank"`
	NodeID           int64    `json:"node_id"`
	ContentVersionID string   `json:"content_version_id"`
	Path             string   `json:"path"`
	ModifiedAt       string   `json:"modified_at"`
	EvidenceIDs      []string `json:"evidence_ids"`
	Compared         bool     `json:"compared"`
}

type findConflictsOutput struct {
	privateCache
	grounding.Comparison
	VaultID   string            `json:"vault_id,omitempty"`
	Truncated bool              `json:"truncated"`
	Passages  []conflictPassage `json:"passages"`
}

func findConflicts(ctx context.Context, session documentReadSession, raw []byte) (findConflictsOutput, []*sdkmcp.ResourceLink, error) {
	var input searchDocumentsInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return findConflictsOutput{}, nil, err
	}
	input.Mode = "lexical"
	result, err := documentRead(ctx, session, func(ctx context.Context, c documentClient) (documentSearchResponse, error) {
		return runDocumentSearch(ctx, c, input, true)
	})
	if err != nil {
		return findConflictsOutput{}, nil, err
	}
	texts := make([]string, len(result.report.Results))
	for i, hit := range result.report.Results {
		if answerContentExcerpt(hit) {
			texts[i] = hit.Excerpt
		}
	}
	out := findConflictsOutput{privateCache: newPrivateCache(), Comparison: grounding.Compare(texts),
		VaultID: result.vaultID, Truncated: result.report.Truncated,
		Passages: make([]conflictPassage, len(texts))}
	for i, hit := range result.report.Results {
		ids, err := encodeSearchEvidenceIDs(hit.Evidence)
		if err != nil {
			return findConflictsOutput{}, nil, err
		}
		out.Passages[i] = conflictPassage{Rank: hit.Rank, NodeID: hit.NodeID, ContentVersionID: hit.ContentVersionID,
			Path: hit.Path, ModifiedAt: result.documents[i].ModifiedAt, EvidenceIDs: ids, Compared: out.Compared[i]}
	}
	return out, documentResourceLinks(result.vaultID, result.documents), nil
}

func findConflictsSchemas() (schema, schema) {
	input, _ := searchDocumentsSchemas()
	properties := input["properties"].(schema)
	delete(properties, "explain")
	delete(properties, "rerank")
	delete(properties, "mode")
	delete(properties, "binding_id")
	figure := objectSchema(schema{
		"passage": integerSchema(1, 100), "text": stringSchema(512), "value": stringSchema(512), "quote": stringSchema(512),
	}, "passage", "text", "value", "quote")
	conflict := objectSchema(schema{
		"kind": enumSchema("money", "percent", "duration"), "unit": stringSchema(512),
		"figures": arraySchema(figure, 0),
	}, "kind", "unit", "figures")
	passage := objectSchema(schema{
		"rank": integerSchema(1, 100), "node_id": integerSchema(1, 0), "content_version_id": uuidSchema(),
		"path":        schema{"type": "string", "minLength": 1, "maxLength": maxPathCharacters, "pattern": "^/"},
		"modified_at": dateTimeSchema(), "evidence_ids": arraySchema(stringSchema(1024), 64), "compared": booleanSchema(),
	}, "rank", "node_id", "content_version_id", "path", "modified_at", "evidence_ids", "compared")
	output := rootObjectSchema(withPrivateCache(schema{
		"status": enumSchema("CONFLICTING", "NO_CONFLICT", "NOT_COMPARABLE"), "vault_id": uuidSchema(),
		"truncated": booleanSchema(),
		"passages":  arraySchema(passage, 100), "conflicts": arraySchema(conflict, 0),
	}), cacheRequired("status", "truncated", "passages", "conflicts")...)
	return input, output
}
