package mcp

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"unicode/utf8"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/grounding"
	"go.kenn.io/docbank/internal/store"
)

var findConflictsTool = toolDefinition{
	name: "find_conflicts", title: "Find differing figures",
	description: "Compare money, percentages, and durations in whole source sentences located by lexical search excerpts with otherwise identical sentence text. Returns disagreements as leads to inspect, with source dates and evidence IDs. Skips ambiguous or incomplete sentences and name-only evidence; never chooses a winner.",
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

func findConflicts(ctx context.Context, lease *daemonLease, raw []byte) (findConflictsOutput, []*sdkmcp.ResourceLink, error) {
	var input searchDocumentsInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return findConflictsOutput{}, nil, err
	}
	input.Mode = "lexical"
	var sources []grounding.Source
	result, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (documentSearchResponse, error) {
		result, err := runDocumentSearch(ctx, daemonDocumentClient{c}, input, true)
		if err != nil {
			return result, err
		}
		sources = make([]grounding.Source, len(result.report.Results))
		for i, hit := range result.report.Results {
			if !answerContentExcerpt(hit) {
				continue
			}
			text := sourceText(ctx, c, hit, result.documents[i])
			if err := ctx.Err(); err != nil {
				return result, err
			}
			needle := strings.Trim(hit.Excerpt, " …")
			if at := strings.Index(text, needle); needle != "" && at >= 0 {
				sources[i] = grounding.Source{Text: text, Start: at, End: at + len(needle)}
			}
		}
		return result, ctx.Err()
	})
	if err != nil {
		return findConflictsOutput{}, nil, err
	}
	out := findConflictsOutput{privateCache: newPrivateCache(), Comparison: grounding.Compare(sources),
		VaultID: result.vaultID, Truncated: result.report.Truncated,
		Passages: make([]conflictPassage, len(sources))}
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

const (
	conflictSourceLimit = 256 << 10
	conflictKindField   = "kind"
)

func sourceText(ctx context.Context, c *daemonconn.Connection, hit api.DocumentSearchResult, summary api.DocumentSummary) string {
	for _, evidence := range hit.Evidence {
		for _, rendition := range summary.ActiveRenditions {
			if evidence.BuildID == "" || evidence.BuildID != rendition.BuildID {
				continue
			}
			stream, err := c.Rendition(ctx, rendition.AttachmentID, conflictSourceLimit)
			if err != nil {
				continue
			}
			defer func() { _ = stream.Close() }()
			if stream.BuildID != evidence.BuildID {
				continue
			}
			if slices.Contains(stream.Warnings, "truncated") {
				return ""
			}
			var data bytes.Buffer
			if _, err := stream.ContentStream.CopyVerified(&data); err != nil {
				continue
			}
			body := data.Bytes()
			if _, stripped, err := document.ParseRenditionFrontMatterV1(body); err == nil {
				body = stripped
			} else if bytes.HasPrefix(body, []byte("---\n")) {
				return ""
			}
			return string(body)
		}
	}
	if !store.SupportsTextExtractionMIME(summary.MediaType) {
		return ""
	}
	stream, err := c.VersionContent(ctx, hit.ContentVersionID)
	if err != nil {
		return ""
	}
	defer func() { _ = stream.Close() }()
	if stream.Size > conflictSourceLimit {
		return ""
	}
	var data bytes.Buffer
	if _, err := stream.CopyVerified(&data); err != nil || !utf8.Valid(data.Bytes()) {
		return ""
	}
	return strings.TrimPrefix(data.String(), "\uFEFF")
}

func findConflictsSchemas() (schema, schema) {
	input, _ := searchDocumentsSchemas()
	properties, _ := input["properties"].(schema)
	properties["limit"] = integerSchema(1, 20)
	delete(properties, "explain")
	delete(properties, "rerank")
	delete(properties, "mode")
	delete(properties, "binding_id")
	figure := objectSchema(schema{
		"passage": integerSchema(1, 100), "text": stringSchema(512), "value": stringSchema(512), "quote": stringSchema(512),
	}, "passage", "text", "value", "quote")
	conflict := objectSchema(schema{
		conflictKindField: enumSchema("money", "percent", "duration"), "unit": stringSchema(512),
		"figures": arraySchema(figure, 0),
	}, conflictKindField, "unit", "figures")
	passage := objectSchema(schema{
		"rank": integerSchema(1, 100), "node_id": integerSchema(1, 0), versionField: uuidSchema(), //nolint:goconst // JSON Schema vocabulary is intentionally repeated.
		"path":        schema{"type": "string", "minLength": 1, "maxLength": maxPathCharacters, "pattern": "^/"}, //nolint:goconst // JSON Schema vocabulary is intentionally repeated.
		"modified_at": dateTimeSchema(), "evidence_ids": arraySchema(stringSchema(1024), 64), "compared": booleanSchema(),
	}, "rank", "node_id", versionField, "path", "modified_at", "evidence_ids", "compared")
	output := rootObjectSchema(withPrivateCache(schema{
		"status": enumSchema("CONFLICTING", "NO_CONFLICT", "NOT_COMPARABLE"), "vault_id": uuidSchema(),
		"truncated": booleanSchema(),
		"passages":  arraySchema(passage, 100), "conflicts": arraySchema(conflict, 0),
	}), cacheRequired("status", "truncated", "passages", "conflicts")...)
	return input, output
}
