package store

import (
	"context"
	"strings"

	"go.kenn.io/docbank/internal/query"
)

func compilePDFPredicate(field, value string, start, end int) (compiledQueryFragment, error) {
	operand, err := query.ParsePDFOperand(field, value)
	if err != nil {
		return compiledQueryFragment{}, compileExpressionError(start, end, err.Error())
	}
	kind := query.PDFFieldKind(field)
	base, operator := query.PDFFieldComparison(field)
	comparison := `json_extract(fact.value, '$.value.` + kind + `') ` + operator + ` ?`
	if kind == "string_list" {
		comparison = `EXISTS (SELECT 1 FROM json_each(json_extract(fact.value, '$.value.strings')) permission WHERE permission.value=?)`
	}
	return compiledQueryFragment{
		sql: `EXISTS (SELECT 1 FROM source_metadata_heads head
   JOIN source_metadata_generations generation ON generation.generation_id=head.generation_id,
   json_each(CASE WHEN instr(generation.canonical_json, '"namespace":"pdf.info"')>0
     THEN docbank_source_metadata_json_v1(generation.canonical_json,generation.contract_version,generation.checksum) END, '$.fields') fact
   WHERE head.source_sha256=cv.blob_hash
   AND json_extract(fact.value, '$.key')=?
   AND json_extract(fact.value, '$.namespace')='pdf.info'
   AND json_extract(fact.value, '$.value.kind')=? AND ` + comparison + `)`,
		args: []any{"pdf.info." + strings.TrimPrefix(base, "pdf_"), kind, operand},
	}, nil
}

// PDF filters accept only PDF fact fields, so references cannot resolve.
type pdfFilterResolver struct{}

func (pdfFilterResolver) Resolve(context.Context, query.ReferenceKind, string, bool) (query.Reference, error) {
	return query.Reference{}, query.ErrUnknownReference
}

func compilePDFFilter(ctx context.Context, text string) (compiledQueryFragment, error) {
	compiled, err := (queryCompiler{pdfOnly: true}).compile(ctx, query.Query{
		V: 1, Text: text, Syntax: "advanced", Mode: "lexical",
		Sort: query.Sort{Field: string(DocumentCatalogSortName), Direction: "asc"},
	}, pdfFilterResolver{})
	if err != nil {
		return compiledQueryFragment{}, err
	}
	return compiled.bind("", nil)
}
