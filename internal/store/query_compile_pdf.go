package store

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

func (c queryCompiler) compilePDFPredicate(field, value string, start, end int) (compiledQueryFragment, error) {
	operand, err := query.ParsePDFOperand(field, value)
	if err != nil {
		return compiledQueryFragment{}, compileExpressionError(start, end, err.Error())
	}
	kind := query.PDFFieldKind(field)
	base, operator := query.PDFFieldComparison(field)
	key := "pdf.info." + strings.TrimPrefix(base, "pdf_")
	if c.postgres {
		comparison := `jsonb_extract_path_text(generation.pdf_query_facts,?)::bigint ` + operator + ` ?`
		switch kind {
		case "boolean":
			comparison = `jsonb_extract_path_text(generation.pdf_query_facts,?)::boolean = ?`
		case "string_list":
			comparison = `jsonb_exists(jsonb_extract_path(generation.pdf_query_facts,?),?)`
			operand = hex.EncodeToString([]byte(value))
		}
		return compiledQueryFragment{
			sql: `EXISTS (SELECT 1 FROM source_metadata_heads head
				JOIN source_metadata_generations generation ON generation.generation_id=head.generation_id
				WHERE head.source_sha256=cv.blob_hash AND ` + comparison + `)`,
			args: []any{key, operand},
		}, nil
	}
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
		args: []any{key, kind, operand},
	}, nil
}

// Encode only the facts that PDF filters use. Hex preserves exact permission
// strings, including NUL, in the PostgreSQL query projection.
func encodePDFQueryFacts(metadata document.SourceMetadataV1) (string, error) {
	var facts map[string]any
	for _, field := range metadata.Fields {
		key, ok := strings.CutPrefix(field.Key, "pdf.info.")
		if !ok || field.Namespace != "pdf.info" || query.PDFFieldKind("pdf_"+key) != string(field.Value.Kind) {
			continue
		}
		if facts == nil {
			facts = make(map[string]any)
		}
		switch field.Value.Kind {
		case document.SourceMetadataBoolean:
			facts[field.Key] = *field.Value.Boolean
		case document.SourceMetadataInteger:
			facts[field.Key] = *field.Value.Integer
		case document.SourceMetadataStringList:
			permissions := make([]string, 0, len(field.Value.Strings))
			for _, permission := range field.Value.Strings {
				if _, err := query.ParsePDFOperand("pdf_permissions", permission); err == nil {
					permissions = append(permissions, hex.EncodeToString([]byte(permission)))
				}
			}
			facts[field.Key] = permissions
		case document.SourceMetadataString, document.SourceMetadataNumber, document.SourceMetadataTimestamp:
		}
	}
	if facts == nil {
		return "{}", nil
	}
	encoded, err := json.Marshal(facts)
	return string(encoded), err
}

// PDF filters accept only PDF fact fields, so references cannot resolve.
type pdfFilterResolver struct{}

func (pdfFilterResolver) Resolve(context.Context, query.ReferenceKind, string, bool) (query.Reference, error) {
	return query.Reference{}, query.ErrUnknownReference
}

func compilePDFFilter(ctx context.Context, text string, postgres bool) (compiledQueryFragment, error) {
	compiled, err := (queryCompiler{pdfOnly: true, postgres: postgres}).compile(ctx, query.Query{
		V: 1, Text: text, Syntax: "advanced", Mode: "lexical",
		Sort: query.Sort{Field: string(DocumentCatalogSortName), Direction: "asc"},
	}, pdfFilterResolver{})
	if err != nil {
		return compiledQueryFragment{}, err
	}
	return compiled.bind("", nil)
}
