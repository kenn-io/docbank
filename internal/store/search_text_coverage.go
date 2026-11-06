package store

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"slices"

	"go.kenn.io/docbank/document"
)

// SearchTextCoverage counts files whose missing text can be recovered by
// processing, and current native-text files still waiting for extraction.
type SearchTextCoverage struct {
	NoText   int
	Families []string
	Pending  int
}

func (s *Store) SearchTextCoverage(ctx context.Context, opts SearchOptions) (out SearchTextCoverage, err error) {
	opts, err = s.NormalizeSearchOptions(ctx, opts)
	if err != nil {
		return out, err
	}
	formats := make(map[string]string)
	for _, entry := range document.FormatMetadataCatalog() {
		if !entry.Provider || SupportsTextExtractionMIME(entry.MediaType) {
			continue
		}
		family := "other"
		switch entry.Family {
		case "pdf":
			family = "pdf"
		case "word", "presentation", "spreadsheet":
			family = "office"
		}
		formats[entry.MediaType] = family
	}
	families := make(map[string]bool)
	err = s.withLexicalGenerationRead(ctx, func(q metadataQuerier, generation LexicalGeneration) (retErr error) {
		scope, args := liveSearchScopeSQL(searchFilterSQL(opts))
		args = append([]any{generation.ID}, args...)
		rows, err := q.QueryContext(ctx, `WITH coverage AS (
		 SELECT cv.mime_type,
		 EXISTS(SELECT 1 FROM text_searchable_versions sv WHERE sv.version_id=cv.version_id) native,
		 EXISTS(SELECT 1 FROM text_extraction_queue tq WHERE tq.blob_hash=cv.blob_hash) pending,
		 EXISTS(SELECT 1 FROM extracted_text e WHERE e.blob_hash=cv.blob_hash AND e.status='ok' AND length(trim(e.text))>0) extracted,
		 EXISTS(SELECT 1 FROM rendition_heads h
		 JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
		  AND a.content_version_id=h.content_version_id AND a.profile_fingerprint=h.profile_fingerprint
		 JOIN rendition_lexical_generation_builds gb ON gb.build_id=a.build_id
		 WHERE h.content_version_id=cv.version_id AND gb.generation_id=? AND `+renditionHasSearchableTextSQL("a.build_id")+`) serving
		 `+scope+`)
		 SELECT mime_type,native,pending,extracted,serving,COUNT(*) FROM coverage
		 GROUP BY mime_type,native,pending,extracted,serving`, args...)
		if err != nil {
			return fmt.Errorf("counting search text coverage: %w", err)
		}
		defer func() { retErr = errors.Join(retErr, rows.Close()) }()
		for rows.Next() {
			var mediaType string
			var native, pending, extracted, serving bool
			var count int
			if err := rows.Scan(&mediaType, &native, &pending, &extracted, &serving, &count); err != nil {
				return err
			}
			if native && pending && !serving && (generation.ID != "" || !extracted) {
				out.Pending += count
			}
			mediaType, _, err = mime.ParseMediaType(mediaType)
			if err != nil {
				continue
			}
			if family, ok := formats[mediaType]; ok && !native && !serving {
				out.NoText += count
				families[family] = true
			}
		}
		return rows.Err()
	})
	out.Families = slices.DeleteFunc([]string{"pdf", "office", "other"}, func(family string) bool { return !families[family] })
	return out, err
}
