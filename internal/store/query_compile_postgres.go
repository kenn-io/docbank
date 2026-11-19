package store

// Use the lexical indexes' normalizer for literal phrases and prefixes. Form
// prefixes from PostgreSQL's quoted lexemes so user punctuation stays literal.
func postgresLiteralQuery(text string, prefix bool) compiledQueryFragment {
	phrase := `phraseto_tsquery('pg_catalog.simple',lexical_text(?))`
	if prefix {
		phrase = `to_tsquery('pg_catalog.simple',NULLIF(CAST(` + phrase + ` AS text),'')||':*')`
	}
	return compiledQueryFragment{sql: phrase, args: []any{lexicalSearchText(text)}}
}

func postgresDocumentLexicalPredicate(query compiledQueryFragment, includeContent bool) compiledQueryFragment {
	name := compiledQueryFragment{
		sql:  `n.id IN (SELECT id FROM nodes WHERE to_tsvector('pg_catalog.simple',lexical_text(name)) @@ (` + query.sql + `))`,
		args: query.args,
	}
	if !includeContent {
		return name
	}
	return joinCompiledFragments([]compiledQueryFragment{name, postgresDocumentContentPredicate(query)}, ` OR `)
}

func postgresDocumentContentPredicate(query compiledQueryFragment) compiledQueryFragment {
	generation := compiledGenerationArgument{}
	profile := compiledProfileArgument{}
	args := append([]any{generation}, query.args...)
	args = append(args, generation, profile, profile)
	args = append(args, query.args...)
	args = append(args, generation)
	return compiledQueryFragment{
		sql: `(
			(CAST(? AS text)='' AND cv.blob_hash IN (
				SELECT t.blob_hash FROM extracted_text t WHERE t.status='ok' AND t.search_vector @@ (` + query.sql + `)
			) AND EXISTS (SELECT 1 FROM text_searchable_versions tsv WHERE tsv.version_id=cv.version_id))
			OR (CAST(? AS text)<>'' AND cv.version_id IN (
				SELECT a.content_version_id FROM rendition_lexical_index li
				JOIN rendition_attachments a ON a.build_id=li.build_id
				JOIN rendition_heads rh
				  ON rh.content_version_id=a.content_version_id
				 AND rh.profile_fingerprint=a.profile_fingerprint
				 AND rh.attachment_id=a.attachment_id
				WHERE (CAST(? AS text) IS NULL OR a.profile_fingerprint=?)
				  AND li.search_vector @@ (` + query.sql + `)
				  AND EXISTS (
					SELECT 1 FROM rendition_lexical_generation_builds gb
					WHERE gb.generation_id=? AND gb.build_id=li.build_id
				  )
			))
		)`,
		args: args,
	}
}
