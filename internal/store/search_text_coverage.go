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

func (s *Store) SearchTextCoverage(ctx context.Context, opts SearchOptions, pdfProfile string, noTextLayer func(context.Context, string) (bool, error)) (out SearchTextCoverage, err error) {
	opts, err = s.NormalizeSearchOptions(ctx, opts)
	if err != nil {
		return out, err
	}
	formats := make(map[string]string)
	for _, entry := range document.FormatMetadataCatalog() {
		if !entry.Provider || SupportsTextExtractionMIME(entry.MediaType) || emailMIME(entry.MediaType) {
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
	err = s.withLexicalGenerationRead(ctx, func(q metadataQuerier, generation LexicalGeneration) error {
		scope, args := liveSearchScopeSQL(searchFilterSQL(opts))
		args = append([]any{pdfProfile, generation.ID, generation.ID}, args...)
		rows, err := q.QueryContext(ctx, `WITH pdf_profile AS (SELECT CAST(? AS TEXT) fingerprint), coverage AS (
		 SELECT COALESCE(cv.mime_type, '') AS mime_type,
 `+pdfWaitingSQL("cv.version_id", "(SELECT fingerprint FROM pdf_profile)")+` pdf_waiting,
 COALESCE((SELECT b.build_id FROM rendition_heads h JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
 JOIN rendition_builds b ON b.build_id=a.build_id
 JOIN rendition_lexical_generation_builds gb ON gb.build_id=b.build_id
 WHERE h.content_version_id=cv.version_id AND h.profile_fingerprint=(SELECT fingerprint FROM pdf_profile) AND gb.generation_id=?
 AND b.lexical_segment_count=0 LIMIT 1),'') pdf_evidence,
		 EXISTS(SELECT 1 FROM text_searchable_versions sv WHERE sv.version_id=cv.version_id) native,
		 EXISTS(SELECT 1 FROM text_extraction_queue tq WHERE tq.blob_hash=cv.blob_hash) pending,
		 EXISTS(SELECT 1 FROM extracted_text e WHERE e.blob_hash=cv.blob_hash AND e.status='ok' AND length(trim(e.text))>0) extracted,
		 EXISTS(SELECT 1 FROM rendition_heads h
		 JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
		  AND a.content_version_id=h.content_version_id AND a.profile_fingerprint=h.profile_fingerprint
		 JOIN rendition_lexical_generation_builds gb ON gb.build_id=a.build_id
		 WHERE h.content_version_id=cv.version_id AND gb.generation_id=? AND `+renditionHasSearchableTextSQL("a.build_id")+`) serving
		 `+scope+`)
		 SELECT mime_type,native,pending,extracted,serving,pdf_evidence,pdf_waiting,COUNT(*) FROM coverage
		 GROUP BY mime_type,native,pending,extracted,serving,pdf_evidence,pdf_waiting`, args...)
		if err != nil {
			return fmt.Errorf("counting search text coverage: %w", err)
		}
		defer func() { _ = rows.Close() }()
		type coverageRow struct {
			mediaType, pdfEvidence                       string
			native, pending, extracted, serving, waiting bool
			count                                        int
		}
		var records []coverageRow
		for rows.Next() {
			var r coverageRow
			if err := rows.Scan(&r.mediaType, &r.native, &r.pending, &r.extracted, &r.serving, &r.pdfEvidence, &r.waiting, &r.count); err != nil {
				_ = rows.Close()
				return err
			}
			records = append(records, r)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, r := range records {
			mediaType, pdfEvidence, waiting := r.mediaType, r.pdfEvidence, r.waiting
			native, pending, extracted, serving, count := r.native, r.pending, r.extracted, r.serving, r.count
			if native && pending && !serving && (generation.ID != "" || !extracted) {
				out.Pending += count
			}
			mediaType, _, err = mime.ParseMediaType(mediaType)
			if err != nil {
				continue
			}
			if mediaType == "application/pdf" && !serving {
				if waiting {
					out.Pending += count
					continue
				}
				if pdfEvidence == "" || noTextLayer == nil {
					continue
				}
				empty, err := noTextLayer(ctx, pdfEvidence)
				if err != nil || !empty {
					continue
				}
			}
			if family, ok := formats[mediaType]; ok && !native && !serving {
				out.NoText += count
				families[family] = true
			}
		}
		return nil
	})
	out.Families = slices.DeleteFunc([]string{"pdf", "office", "other"}, func(family string) bool { return !families[family] })
	return out, err
}
