package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"

	pgsearch "go.kenn.io/kit/search/postgres"
	"go.kenn.io/kit/search/sqlquery"
)

// PostgreSQL search text replaces NUL only in its derived index projection.
// Original text, portable metadata and evidence hashes keep their exact bytes.
func lexicalSearchText(text string) string { return strings.ReplaceAll(text, "\x00", " ") }

func insertExtractedTextMetadata(ctx context.Context, tx *sql.Tx, row metadataExtractedText, postgres bool) error {
	column, value := "", ""
	args := []any{row.BlobHash, storedString(postgres, row.Extractor), row.ExtractorVersion,
		row.Status, storedOptionalString(postgres, row.Error), row.Attempts,
		storedOptionalString(postgres, row.Text), row.ExtractedAt}
	if postgres {
		column, value = ",search_text", ",$9"
		text := ""
		if row.Text != nil {
			text = lexicalSearchText(*row.Text)
		}
		args = append(args, text)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO extracted_text(blob_hash,extractor,extractor_version,
 status,error,attempts,text,extracted_at`+column+`) VALUES($1,$2,$3,$4,$5,$6,$7,$8`+value+`)`, args...)
	return err
}

// Kit rebases anonymous query parameters; standalone expressions use numbered binds.
const anonymousSearchParameters = -1

// PostgreSQL shares the natural-query policy; native constructors own tokenization.
func postgresSearchExpression(query string, offset int) sqlquery.Predicate {
	args := []any{}
	bind := func(value string) string {
		args = append(args, value)
		if offset == anonymousSearchParameters {
			return "?"
		}
		return "$" + strconv.Itoa(offset+len(args))
	}
	natural, quotes, err := analyzeSearch(query)
	if err != nil {
		return sqlquery.Predicate{SQL: "plainto_tsquery('pg_catalog.simple',lexical_text(" + bind(query) + "))", Args: args}
	}
	typed := strings.Fields(strings.ReplaceAll(strings.ToLower(quotes), `"`, " "))
	wordless := func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.IsSpace(r) }
	groups := []string{}
	for _, word := range natural.Words {
		forms := []string{}
		for _, form := range word.Forms {
			if slices.ContainsFunc(typed, func(text string) bool { return strings.TrimFunc(text, wordless) == form }) {
				forms = append(forms, "to_tsquery('pg_catalog.simple',phraseto_tsquery('pg_catalog.simple',lexical_text("+bind(form)+"))::text||':*')")
			} else {
				forms = append(forms, "phraseto_tsquery('pg_catalog.simple',lexical_text("+bind(form)+"))")
			}
		}
		groups = append(groups, "("+strings.Join(forms, " || ")+")")
	}
	for _, phrase := range natural.Phrases {
		groups = append(groups, "phraseto_tsquery('pg_catalog.simple',lexical_text("+bind(phrase)+"))")
	}
	return sqlquery.Predicate{SQL: strings.Join(groups, " && "), Args: args}
}

func postgresNameSearch(query string, opts SearchOptions, limit int, filesOnly bool) (sqlquery.Query, error) {
	expression := postgresSearchExpression(query, anonymousSearchParameters)
	predicate := "d.trashed_at IS NULL"
	if filesOnly {
		predicate += " AND d.kind='file' AND d.current_version_id IS NOT NULL"
	}
	names, err := pgsearch.BuildLexical(pgsearch.LexicalRequest{
		Mapping: pgsearch.LexicalMapping{SourceTable: "nodes", SourceKey: "id", Vector: `to_tsvector('pg_catalog.simple',lexical_text(d.name))`},
		TSQuery: expression.SQL, TSQueryArgs: expression.Args, SourcePredicate: sqlquery.Predicate{SQL: predicate}, CandidateLimit: math.MaxInt,
	})
	if err != nil {
		return sqlquery.Query{}, fmt.Errorf("build PostgreSQL filename search: %w", err)
	}
	filter, args := searchFilterSQL(opts, true, len(names.Args))
	names.Args = append(names.Args, args...)
	statement := `SELECT ` + nodeCols + ` FROM ` + nodeFrom + ` JOIN (` + names.SQL + `) names ON names.doc_key=n.id
 WHERE n.trashed_at IS NULL ` + filter + `
 ORDER BY names.score DESC,n.name,n.id LIMIT $` + strconv.Itoa(len(names.Args)+1)
	return sqlquery.Query{SQL: statement, Args: append(names.Args, limit+1)}, nil
}

func postgresContentSearch(query, generation string, opts SearchOptions) (sqlquery.Query, error) {
	expression := postgresSearchExpression(query, anonymousSearchParameters)
	table, key := "extracted_text", "id"
	relation := `content_versions cv
 JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id
 JOIN text_searchable_versions tsv ON tsv.version_id=cv.version_id`
	binding, column := "cv.blob_hash", "blob_hash"
	condition := "d.status='ok'"
	if generation != "" {
		table, key = "rendition_lexical_index", "row_id"
		relation = `rendition_lexical_generation_builds gb
 JOIN rendition_attachments a ON a.build_id=gb.build_id
 JOIN rendition_heads rh ON rh.content_version_id=a.content_version_id
  AND rh.profile_fingerprint=a.profile_fingerprint AND rh.attachment_id=a.attachment_id
 JOIN content_versions cv ON cv.version_id=a.content_version_id
 JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id`
		binding, column = "gb.build_id", "build_id"
		condition = "TRUE"
	}
	hits, err := pgsearch.BuildLexical(pgsearch.LexicalRequest{
		Mapping: pgsearch.LexicalMapping{SourceTable: table, SourceKey: key, Vector: `d.search_vector`},
		TSQuery: expression.SQL, TSQueryArgs: expression.Args,
		SourcePredicate: sqlquery.Predicate{SQL: condition}, CandidateLimit: math.MaxInt,
	})
	if err != nil {
		return sqlquery.Query{}, fmt.Errorf("build PostgreSQL content search: %w", err)
	}
	// Resolve every output membership before bounding: one immutable build can serve several files.
	outputJoin := `JOIN (` + relation + `) ON ` + binding + `=text_index.` + column
	outputCondition := "text_index.status='ok'"
	if generation != "" {
		outputCondition = "$" + strconv.Itoa(len(hits.Args)+1) + "=gb.generation_id"
		hits.Args = append(hits.Args, generation)
		outputJoin += ` JOIN rendition_lexical_segments ls ON ls.build_id=text_index.build_id AND ls.segment_id=text_index.segment_id
 JOIN rendition_units ru ON ru.build_id=ls.build_id AND ru.unit_id=ls.unit_id`
	}
	filter, args := searchFilterSQL(opts, true, len(hits.Args))
	if len(opts.SelectedBuilds) > 0 {
		if generation == "" {
			return sqlquery.Query{}, errors.New("selected media builds have no lexical generation")
		}
		encoded, err := json.Marshal(opts.SelectedBuilds)
		if err != nil {
			return sqlquery.Query{}, err
		}
		filter += ` AND EXISTS(SELECT 1 FROM json_array_elements($` + strconv.Itoa(len(hits.Args)+len(args)+1) + `::json) selection
   WHERE selection->>'content_version_id'=a.content_version_id AND selection->>'build_id'=a.build_id)`
		args = append(args, string(encoded))
	}
	hits.Args = append(hits.Args, args...)
	snippet := `ts_headline('pg_catalog.simple',text_index.search_text,(SELECT tsq FROM requested_query),
 'MaxWords=24,MinWords=12,StartSel='||chr(1)||',StopSel='||chr(2))`
	evidence := `'' AS build_id,'' AS segment_id,` + snippet + `,'' AS locator_json`
	order := ",text_index.id"
	if generation != "" {
		evidence = `text_index.build_id,text_index.segment_id,` + snippet + `,ru.locator_json`
		order = ",text_index.build_id,text_index.segment_id"
	}
	statement := `WITH requested_query AS(SELECT ` + postgresSearchExpression(query, 0).SQL + ` AS tsq)
 SELECT ` + nodeCols + `,` + evidence + ` FROM (` + hits.SQL + `) matches
 JOIN ` + table + ` text_index ON text_index.` + key + `=matches.doc_key ` + outputJoin + `
 WHERE ` + outputCondition + ` AND n.trashed_at IS NULL ` + filter + `
 ORDER BY matches.score DESC,n.name,n.id` + order
	return sqlquery.Query{SQL: statement, Args: hits.Args}, nil
}
