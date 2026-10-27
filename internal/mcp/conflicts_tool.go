package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/conflicts"
)

type conflictPassage struct {
	Rank             int      `json:"rank"`
	NodeID           int64    `json:"node_id"`
	ContentVersionID string   `json:"content_version_id"`
	Path             string   `json:"path"`
	EvidenceIDs      []string `json:"evidence_ids"`
	ModifiedAt       string   `json:"modified_at"`
	Excerpt          string   `json:"excerpt,omitempty"`
	Compared         bool     `json:"compared"`
	SkipReason       string   `json:"skip_reason,omitempty"`
}
type conflictSide struct {
	Passage      int            `json:"passage"`
	Quote        string         `json:"quote"`
	QuoteStart   int            `json:"quote_start"`
	QuoteEnd     int            `json:"quote_end"`
	Figure       string         `json:"figure"`
	FigureStart  int            `json:"figure_start"`
	FigureEnd    int            `json:"figure_end"`
	Value        string         `json:"value"`
	EndValue     string         `json:"end_value,omitempty"`
	Precision    string         `json:"precision,omitempty"`
	Qualifier    string         `json:"qualifier,omitempty"`
	StatedPeriod conflictPeriod `json:"stated_period"`
}
type conflictPeriod struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}
type conflictEdge struct {
	Kind           string         `json:"kind"`
	Unit           string         `json:"unit"`
	Subject        []string       `json:"subject"`
	PeriodsOverlap string         `json:"periods_overlap"`
	Sides          []conflictSide `json:"sides"`
}
type conflictsOutput struct {
	privateCache

	Status             conflicts.Status     `json:"status"`
	VaultID            string               `json:"vault_id"`
	FenceFingerprint   string               `json:"fence_fingerprint"`
	ActualMode         string               `json:"actual_mode"`
	Coverage           searchCoverageOutput `json:"coverage"`
	Truncated          bool                 `json:"truncated"`
	ConflictsTruncated bool                 `json:"conflicts_truncated"`
	PassagesTruncated  bool                 `json:"passages_truncated"`
	Passages           []conflictPassage    `json:"passages"`
	Conflicts          []conflictEdge       `json:"conflicts"`
}

func findConflicts(ctx context.Context, session documentReadSession, raw []byte) (conflictsOutput, error) {
	var input searchDocumentsInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return conflictsOutput{}, err
	}
	if input.Limit == 0 {
		input.Limit = 20
	}
	if input.Limit < 1 || input.Limit > 20 {
		return conflictsOutput{}, invalidToolArgumentsError()
	}
	adjusted, err := json.Marshal(input)
	if err != nil {
		return conflictsOutput{}, err
	}
	search, _, err := searchDocuments(ctx, session, adjusted)
	if err != nil {
		return conflictsOutput{}, err
	}
	out := conflictsOutput{privateCache: newPrivateCache(), Status: conflicts.NotComparable, VaultID: search.VaultID, FenceFingerprint: search.FenceFingerprint, ActualMode: search.ActualMode, Coverage: search.Coverage, Truncated: search.Truncated, Passages: []conflictPassage{}, Conflicts: []conflictEdge{}}
	if len(search.Results) == 0 {
		return out, nil
	}
	identities := make([]api.DocumentIdentity, len(search.Results))
	for i, hit := range search.Results {
		identities[i] = api.DocumentIdentity{NodeID: hit.NodeID, ContentVersionID: hit.ContentVersionID, Path: hit.Path}
	}
	summaries, err := documentRead(ctx, session, func(ctx context.Context, c documentClient) ([]api.DocumentSummary, error) {
		return c.ResolveDocumentSummaries(ctx, api.DocumentSummaryResolveRequest{Identities: identities})
	})
	if err != nil {
		return conflictsOutput{}, err
	}
	if len(summaries) != len(identities) {
		return conflictsOutput{}, errors.New("invalid summary response")
	}
	passages := make([]conflicts.Passage, len(search.Results))
	for i, hit := range search.Results {
		summary := summaries[i]
		if summary.NodeID != hit.NodeID || summary.ContentVersionID != hit.ContentVersionID || summary.Path != hit.Path {
			return conflictsOutput{}, errors.New("summary identity mismatch")
		}
		p := conflictPassage{Rank: hit.Rank, NodeID: hit.NodeID, ContentVersionID: hit.ContentVersionID, Path: hit.Path, EvidenceIDs: hit.EvidenceIDs, ModifiedAt: summary.ModifiedAt, Excerpt: hit.Excerpt}
		content := false
		for _, id := range hit.EvidenceIDs {
			var evidence api.DocumentEvidenceReference
			if json.Unmarshal([]byte(id), &evidence) == nil && (evidence.Kind == "rendition_segment" || evidence.Kind == "content_blob") {
				content = true
			}
		}
		switch {
		case hit.Excerpt == "":
			p.SkipReason = "no_excerpt"
		case !content:
			p.SkipReason = "no_content_evidence"
		case utf8.RuneCountInString(hit.Excerpt) >= 512 || strings.HasPrefix(hit.Excerpt, "…") || strings.HasSuffix(strings.TrimSpace(hit.Excerpt), "…"):
			p.SkipReason = "saturated_excerpt"
		default:
			p.Compared = true
			passages[i] = conflicts.Passage{Document: strconv.FormatInt(hit.NodeID, 10), Text: hit.Excerpt}
		}
		out.Passages = append(out.Passages, p)
	}
	report := conflicts.Find(conflicts.Input{Query: input.Query, Passages: passages})
	out.Status = report.Status
	out.ConflictsTruncated = report.Truncated
	for i, reason := range report.Skipped {
		if out.Passages[i].Compared {
			out.Passages[i].Compared = false
			out.Passages[i].SkipReason = reason
		}
	}
	for _, edge := range report.Conflicts {
		if len(out.Conflicts) == 16 {
			out.ConflictsTruncated = true
			break
		}
		e := conflictEdge{Kind: edge.Kind, Unit: edge.Unit, Subject: edge.Subject, PeriodsOverlap: edge.PeriodsOverlap, Sides: make([]conflictSide, 0, 2)}
		for _, side := range edge.Sides {
			f := side.Figure
			e.Sides = append(e.Sides, conflictSide{Passage: side.Passage, Quote: f.Clause, QuoteStart: f.ClauseStart, QuoteEnd: f.ClauseEnd, Figure: f.Text, FigureStart: f.Start, FigureEnd: f.End, Value: f.Value, EndValue: f.EndValue, Precision: f.Precision, Qualifier: f.Qualifier, StatedPeriod: conflictPeriod{From: f.Period.From, To: f.Period.To}})
		}
		out.Conflicts = append(out.Conflicts, e)
	}
	for {
		n, err := toolEnvelopeBytes(out)
		if err != nil {
			return conflictsOutput{}, err
		}
		if n <= maxToolResponseBytes-8<<10 {
			// A pruned report must not claim agreement or report a conflict
			// whose evidence no longer appears in the returned envelope.
			if len(out.Conflicts) == 0 && (out.ConflictsTruncated || out.PassagesTruncated) {
				out.Status = conflicts.NotComparable
			}
			return out, nil
		}
		if len(out.Conflicts) > 0 {
			out.Conflicts = out.Conflicts[:len(out.Conflicts)-1]
			out.ConflictsTruncated = true
			continue
		}
		if len(out.Passages) > 0 {
			out.Passages = out.Passages[:len(out.Passages)-1]
			out.PassagesTruncated = true
			continue
		}
		return conflictsOutput{}, errToolResultTooLarge
	}
}

func findConflictsSchemas() (schema, schema) {
	const vaultIDField, nodeField, versionField = "vault_id", "node_id", "content_version_id"
	input, searchOutput := searchDocumentsSchemas()
	inputProperties, ok := input["properties"].(schema)
	if !ok {
		panic("search input schema lacks properties")
	}
	inputProperties[schemaLimitField] = integerSchema(1, 20)
	searchProperties, ok := searchOutput["properties"].(schema)
	if !ok {
		panic("search output schema lacks properties")
	}
	passage := objectSchema(schema{
		"rank": integerSchema(1, 100), nodeField: integerSchema(1, 0), versionField: uuidSchema(),
		"path": stringSchema(maxPathCharacters), "evidence_ids": arraySchema(stringSchema(1024), 64),
		"modified_at": dateTimeSchema(), "excerpt": stringSchema(512), "compared": booleanSchema(), "skip_reason": stringSchema(64),
	}, "rank", nodeField, versionField, "path", "evidence_ids", "modified_at", "compared")
	period := objectSchema(schema{"from": stringSchema(10), "to": stringSchema(10)})
	// Normalized values and subject tokens can expand beyond the original
	// excerpt's rune count. Bound them by the complete response budget rather
	// than assuming a fixed Unicode normalization expansion factor.
	normalizedText := stringSchema(maxToolResponseBytes)
	side := objectSchema(schema{
		"passage": integerSchema(0, 19), "quote": stringSchema(512), "quote_start": integerSchema(0, 512), "quote_end": integerSchema(0, 512),
		"figure": stringSchema(512), "figure_start": integerSchema(0, 512), "figure_end": integerSchema(0, 512),
		"value": normalizedText, "end_value": normalizedText, "precision": stringSchema(16), "qualifier": stringSchema(32), "stated_period": period,
	}, "passage", "quote", "quote_start", "quote_end", "figure", "figure_start", "figure_end", "value", "stated_period")
	edge := objectSchema(schema{"kind": stringSchema(32), "unit": stringSchema(64), "subject": arraySchema(normalizedText, maxToolResponseBytes), "periods_overlap": enumSchema("true", "false", "unknown"), "sides": arraySchema(side, 2)}, "kind", "unit", "subject", "periods_overlap", "sides")
	fingerprint := stringSchema(71)
	fingerprint["pattern"] = "^sha256:[0-9a-f]{64}$"
	output := rootObjectSchema(withPrivateCache(schema{
		"status":     enumSchema("CONFLICTING", "NO_CONFLICT", "NOT_COMPARABLE"),
		vaultIDField: uuidSchema(), "fence_fingerprint": fingerprint,
		"actual_mode": enumSchema("lexical", "semantic", "hybrid"), "coverage": searchProperties["coverage"],
		"truncated": booleanSchema(), "conflicts_truncated": booleanSchema(), "passages_truncated": booleanSchema(),
		"passages": arraySchema(passage, 20), "conflicts": arraySchema(edge, 16),
	}), cacheRequired("status", vaultIDField, "fence_fingerprint", "actual_mode", "coverage", "truncated", "conflicts_truncated", "passages_truncated", "passages", "conflicts")...)
	return input, output
}
