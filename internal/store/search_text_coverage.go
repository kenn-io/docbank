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
	NoText           int
	Families         []string
	Pending          int
	PDFsWithoutText  int
	PagesWithoutText int
	OtherWithoutText int
}

func (s *Store) SearchTextCoverage(ctx context.Context, opts SearchOptions, pdfProfile string, noTextLayer func(context.Context, string) ([]int, error)) (out SearchTextCoverage, err error) {
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
	err = s.withLexicalGenerationRead(ctx, func(q metadataQuerier, generation LexicalGeneration) (retErr error) {
		d := metadataJSONDialect{s.postgresSchema != ""}
		scope, args := liveSearchScopeSQL(searchFilterSQL(opts))
		args = append([]any{pdfProfile, pdfProfile, generation.ID, generation.ID}, args...)
		rows, err := q.QueryContext(ctx, `WITH coverage AS (
		 SELECT COALESCE(cv.mime_type, '') AS mime_type,
 `+pdfWaitingSQL("cv.version_id", "?")+` pdf_waiting,
 COALESCE((SELECT b.build_id FROM rendition_heads h JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
 JOIN rendition_builds b ON b.build_id=a.build_id
 JOIN rendition_lexical_generation_builds gb ON gb.build_id=b.build_id
 WHERE h.content_version_id=cv.version_id AND h.profile_fingerprint=? AND gb.generation_id=?
 AND b.unit_count < `+d.number("b.provider_receipt_json", "usage.units")+`
 LIMIT 1),'') pdf_evidence,
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
		defer func() { retErr = errors.Join(retErr, rows.Close()) }()
		type evidenceCheck struct {
			buildID, family  string
			count            int
			serving, waiting bool
		}
		var checks []evidenceCheck
		for rows.Next() {
			var mediaType, pdfEvidence string
			var native, pending, extracted, serving, waiting bool
			var count int
			if err := rows.Scan(&mediaType, &native, &pending, &extracted, &serving, &pdfEvidence, &waiting, &count); err != nil {
				return err
			}
			if native && pending && !serving && (generation.ID != "" || !extracted) {
				out.Pending += count
			}
			mediaType, _, err = mime.ParseMediaType(mediaType)
			if err != nil {
				continue
			}
			if mediaType == "application/pdf" {
				if waiting && !serving {
					out.Pending += count
				}
				if family, ok := formats[mediaType]; ok && !native && pdfEvidence != "" && noTextLayer != nil {
					checks = append(checks, evidenceCheck{pdfEvidence, family, count, serving, waiting})
				}
				continue
			}
			if family, ok := formats[mediaType]; ok && !native && !serving {
				out.NoText += count
				out.OtherWithoutText += count
				families[family] = true
			}
		}
		// Evidence reads use their own queries, so they wait for these rows to close.
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		read := make(map[string][]int)
		for _, check := range checks {
			pages, ok := read[check.buildID]
			if !ok {
				pages, err = noTextLayer(ctx, check.buildID)
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err != nil {
					pages = nil
				}
				read[check.buildID] = pages
			}
			if len(pages) > 0 {
				out.PDFsWithoutText += check.count
				out.PagesWithoutText += len(pages) * check.count
				if !check.serving && !check.waiting {
					out.NoText += check.count
					families[check.family] = true
				}
			}
		}
		return nil
	})
	out.Families = slices.DeleteFunc([]string{"pdf", "office", "other"}, func(family string) bool { return !families[family] })
	return out, err
}
