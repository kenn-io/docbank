package store

import (
	"context"
	"encoding/json/v2"
	"errors"
)

// PDFTextTargetsAfter lists live PDFs without the profile's text, optionally
// including versions whose job is still waiting to publish.
func (s *Store) PDFTextTargetsAfter(ctx context.Context, profile, after string, maxBytes int64, limit int, includeWaiting bool) ([]DocumentSummary, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("invalid PDF backfill bounds")
	}
	mimeType := "lower(trim(CASE WHEN instr(v.mime_type,';')=0 THEN v.mime_type ELSE substr(v.mime_type,1,instr(v.mime_type,';')-1) END))"
	if s.postgresSchema != "" {
		mimeType = `lower(trim(CASE
 WHEN position(';'::bytea in v.mime_type)>0
  AND position('\x00'::bytea in v.mime_type)>0
  AND position('\x00'::bytea in v.mime_type)<position(';'::bytea in v.mime_type)
 THEN encode(substring(v.mime_type from 1 for position('\x00'::bytea in v.mime_type)-1),'escape')
 ELSE split_part(encode(v.mime_type,'escape'),';',1) END))`
	}
	waiting := ""
	if !includeWaiting {
		waiting = ` AND NOT ` + pdfWaitingSQL("v.version_id", "$2")
	}
	rows, err := s.db.QueryContext(ctx, liveProcessingTargetsSQL+` WHERE v.version_id>$1 AND `+`NOT EXISTS(SELECT 1 FROM rendition_heads h WHERE h.content_version_id=v.version_id AND h.profile_fingerprint=$2)`+waiting+` AND v.size>0 AND v.size<=$3 AND `+mimeType+`='application/pdf'
 ORDER BY v.version_id LIMIT $4`, after, profile, maxBytes, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DocumentSummary
	for rows.Next() {
		var item DocumentSummary
		if err := rows.Scan(&item.NodeID, &item.ContentVersionID, &item.Name); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func pdfOmissionCandidateSQL(d metadataJSONDialect, alias string) string {
	return alias + ".unit_count < " + d.number(alias+".provider_receipt_json", "usage.units")
}

// PDFTextCandidates returns active builds whose metadata records omitted pages.
func (s *Store) PDFTextCandidates(ctx context.Context, versions []string, profile string) (map[string]string, error) {
	d := metadataJSONDialect{s.postgresSchema != ""}
	encoded, err := json.Marshal(versions)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT h.content_version_id, b.build_id FROM rendition_heads h
		JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
		 AND a.content_version_id=h.content_version_id AND a.profile_fingerprint=h.profile_fingerprint
		JOIN rendition_builds b ON b.build_id=a.build_id
		WHERE h.content_version_id IN (SELECT value FROM `+d.arrayValues("$1")+`) AND h.profile_fingerprint=$2
		 AND `+pdfOmissionCandidateSQL(d, "b"), string(encoded), profile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	builds := make(map[string]string)
	for rows.Next() {
		var version, build string
		if err := rows.Scan(&version, &build); err != nil {
			return nil, err
		}
		builds[version] = build
	}
	return builds, rows.Err()
}

func pdfWaitingSQL(version, profile string) string {
	return `EXISTS(SELECT 1 FROM rendition_job_waiters w JOIN rendition_jobs j ON j.job_id=w.job_id WHERE w.content_version_id=` + version + ` AND w.profile_fingerprint=` + profile + ` AND w.state='waiting' AND j.state IN ('queued','running','retry_wait'))`
}
