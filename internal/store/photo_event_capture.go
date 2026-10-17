package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

func (s *Store) warnPhotoCapture(err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	f := &s.photoEvents
	f.Lock()
	warn := (len(f.subscribers) > 0 || s.hiddenNow().Before(f.pollUntil)) && !f.captureDegraded
	if warn {
		f.captureDegraded = true
	}
	f.Unlock()
	if warn {
		slog.Warn("photo event capture unavailable; viewers will refetch", "error", err)
	}
}

// Temporary objects follow the writer connection; the flag is reset before every write.
func (s *Store) preparePhotoChanges(ctx context.Context, conn *sql.Conn, active bool) bool {
	var present int
	err := conn.QueryRowContext(ctx, `SELECT count(*) FROM temp.sqlite_schema WHERE name='docbank_photo_capture'`).Scan(&present)
	if err == nil && present == 0 {
		if !active {
			return false
		}
		// Commit installation separately so a rolled-back mutation retains the objects.
		var tx *sql.Tx
		tx, err = conn.BeginTx(context.WithoutCancel(ctx), nil)
		if err == nil {
			defer func() { _ = tx.Rollback() }()
			err = installPhotoChangeTriggers(ctx, tx)
			if err == nil {
				err = tx.Commit()
			}
		}
	}
	if err == nil {
		_, err = conn.ExecContext(ctx, `UPDATE docbank_photo_capture SET enabled=?`, active)
	}
	if err == nil && active {
		_, err = conn.ExecContext(ctx, `DELETE FROM docbank_photo_changes`)
	}
	if err != nil {
		s.warnPhotoCapture(err)
	}
	return active && err == nil
}

const (
	photoChangeRefetch        = "refetch"
	photoChangeCurrentVersion = "current_version"
	// Preview outputs replace a row's bytes without changing query membership.
	photoChangePreview = "preview"
)

// Related keys survive deletion of the row that originally linked a worker result to a photo.
type photoEventCaptureRule struct{ table, key, relation, kind, columns string }

var photoEventCaptureRules = []photoEventCaptureRule{
	{"photo_assets", "asset_id", "asset_id", "asset", "revision,hidden_at,excluded_at,display_file_id"},
	{"photo_files", "file_id", "asset_id", "asset", "file_id,node_id,asset_id,revision,rating,flag,label,caption,creator,copyright,rotation,role,sidecar_of_file_id"},
	{"nodes", "id", "id", "node", "revision,name,parent_id,current_version_id,trashed_at,created_at,modified_at"},
	{"node_tags", "node_id", "node_id", "node", ""},
	{metadataProvenanceType, "node_id", "node_id", "node", ""},
	{"extracted_text", "blob_hash", "blob_hash", "source", "status,text"},
	{"text_searchable_versions", "version_id", "version_id", photoChangeCurrentVersion, ""},
	{"source_metadata_heads", "source_sha256", "source_sha256", "source", "generation_id"},
	{"photo_technical_metadata", "generation_id", "generation_id", "generation", ""},
	{"photo_faces", "face_id", "content_version_id", photoChangeCurrentVersion, "content_version_id,x,y,width,height,revision,assertion_id"},
	{"photo_quality_signals", metadataContentVersionIDField, metadataContentVersionIDField, photoChangeCurrentVersion, ""},
	{"visual_preview_generations", metadataContentVersionIDField, metadataContentVersionIDField, photoChangePreview, ""},
	{"visual_preview_heads", metadataContentVersionIDField, metadataContentVersionIDField, photoChangePreview, "generation_id"},
	{"rendition_heads", metadataContentVersionIDField, metadataContentVersionIDField, photoChangeCurrentVersion, "attachment_id"},
	{"rendition_job_waiters", "waiter_id", "content_version_id", photoChangePreview, "job_id,content_version_id,profile_fingerprint,state"},
	{"rendition_jobs", "job_id", "job_id", "job", "state,updated_at"},
	{"person_document_assertions", metadataContentVersionIDField, metadataContentVersionIDField, photoChangeCurrentVersion, ""},
	{"photo_sets", "set_id", "set_id", photoChangeRefetch, "name,starred,cover_asset_id,deleted_at"},
	{"photo_set_members", "set_id", "asset_id", "asset", ""},
	{"tags", "id", "id", photoChangeRefetch, "name,revision"},
	{"saved_queries", "id", "id", photoChangeRefetch, ""},
	{"collection_labels", "ingest_id", "ingest_id", photoChangeRefetch, ""},
	{"person_aliases", "retired_person_id", "retired_person_id", photoChangeRefetch, ""},
	{"persons", "person_id", "person_id", photoChangeRefetch, "display_name_folded,state,revision"},
	{"photo_library_settings", "singleton", "singleton", photoChangeRefetch, ""},
	{"photo_hidden_credentials", "singleton", "singleton", photoChangeRefetch, ""},
}

const photoChangeCurrentVersionFilter = "EXISTS(SELECT 1 FROM nodes n JOIN photo_files f ON f.node_id=n.id AND f.asset_id IS NOT NULL WHERE n.current_version_id=$)"

// Filters keep rows that cannot reach a photo from consuming the capture budget; $ is the related key.
var photoChangeFilters = map[string]string{
	"node":                    "EXISTS(SELECT 1 FROM photo_files WHERE node_id=$ AND asset_id IS NOT NULL)",
	photoChangeCurrentVersion: photoChangeCurrentVersionFilter,
	photoChangePreview:        photoChangeCurrentVersionFilter,
	"job":                     "EXISTS(SELECT 1 FROM rendition_job_waiters w JOIN content_versions v ON v.version_id=w.content_version_id JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id JOIN photo_files f ON f.node_id=n.id AND f.asset_id IS NOT NULL WHERE w.job_id=$)",
}

// The 65th distinct key represents overflow. Further rows do no capture work.
func photoChangeRecord(source, key, related string) string {
	return `INSERT INTO docbank_photo_changes SELECT '` + source + `',CAST(` + key + ` AS TEXT),CAST(` + related + ` AS TEXT) WHERE ` + key + ` IS NOT NULL AND ` + related + ` IS NOT NULL AND (SELECT count(*) FROM docbank_photo_changes)<65 ON CONFLICT DO NOTHING;`
}

func installPhotoChangeTriggers(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE docbank_photo_capture(enabled INTEGER); INSERT INTO docbank_photo_capture VALUES(0); CREATE TEMP TABLE docbank_photo_changes(source TEXT,key TEXT,related TEXT,PRIMARY KEY(source,key,related)) WITHOUT ROWID`); err != nil {
		return err
	}
	install := func(name, action, table, when, body string) error {
		_, err := tx.ExecContext(ctx, `CREATE TEMP TRIGGER photo_events_`+name+` BEFORE `+action+` ON main.`+table+` WHEN (SELECT enabled FROM docbank_photo_capture) AND (SELECT count(*) FROM docbank_photo_changes)<65 AND (`+when+`) BEGIN `+body+` END`)
		return err
	}
	for _, r := range photoEventCaptureRules {
		for _, action := range []string{"INSERT", "UPDATE", "DELETE"} {
			refs := []string{"NEW"}
			if action == "DELETE" {
				refs = []string{"OLD"}
			}
			if action == "UPDATE" {
				refs = []string{"OLD", "NEW"}
			}
			when := "1"
			if action == "UPDATE" && r.columns != "" {
				var changed []string
				for c := range strings.SplitSeq(r.columns, ",") {
					changed = append(changed, "OLD."+c+" IS NOT NEW."+c)
				}
				when = strings.Join(changed, " OR ")
			}
			var body strings.Builder
			for _, ref := range refs {
				insert := photoChangeRecord(r.table, ref+"."+r.key, ref+"."+r.relation)
				if filter, ok := photoChangeFilters[r.kind]; ok {
					insert = strings.Replace(insert, " WHERE ", " WHERE "+strings.ReplaceAll(filter, "$", ref+"."+r.relation)+" AND ", 1)
				}
				body.WriteString(insert)
			}
			if err := install(r.table+"_"+action, action, r.table, when, body.String()); err != nil {
				return fmt.Errorf("photo capture %s: %w", r.table, err)
			}
		}
	}
	if err := install("hide", "UPDATE", "photo_assets", "OLD.hidden_at IS NULL AND NEW.hidden_at IS NOT NULL", photoChangeRecord(photoChangeRefetch, "''", "''")); err != nil {
		return err
	}
	if err := install("directory", "UPDATE", "nodes", "OLD.kind='dir' AND (OLD.name IS NOT NEW.name OR OLD.parent_id IS NOT NEW.parent_id)", photoChangeRecord(photoChangeRefetch, "''", "''")); err != nil {
		return err
	}
	for _, action := range []string{"INSERT", "UPDATE", "DELETE"} {
		refs := []string{"NEW"}
		if action == "DELETE" {
			refs = []string{"OLD"}
		}
		if action == "UPDATE" {
			refs = []string{"OLD", "NEW"}
		}
		var body strings.Builder
		for _, ref := range refs {
			body.WriteString(photoChangeRecord("hash", "(SELECT blob_hash FROM content_versions WHERE version_id="+ref+".current_version_id)", ref+".id"))
		}
		when := "1"
		if action == "UPDATE" {
			when = "OLD.current_version_id IS NOT NEW.current_version_id OR OLD.trashed_at IS NOT NEW.trashed_at"
		}
		if err := install("hash_"+action, action, "nodes", when, body.String()); err != nil {
			return err
		}
	}
	return install("initial_hash", "INSERT", "content_versions", "EXISTS(SELECT 1 FROM nodes WHERE id=NEW.node_id AND current_version_id=NEW.version_id)", photoChangeRecord("hash", "NEW.blob_hash", "NEW.node_id"))
}

const photoChangeCurrentVersionSelection = `SELECT a.asset_id FROM content_versions v JOIN nodes n ON n.current_version_id=v.version_id JOIN photo_files f ON f.node_id=n.id JOIN photo_assets a ON a.asset_id=f.asset_id WHERE v.version_id IN (SELECT value FROM json_each(?))`

var photoChangeSelections = map[string]string{
	"asset":                   `SELECT value FROM json_each(?)`,
	"node":                    `SELECT a.asset_id FROM photo_files f JOIN photo_assets a ON a.asset_id=f.asset_id WHERE f.node_id IN (SELECT value FROM json_each(?))`,
	photoChangeCurrentVersion: photoChangeCurrentVersionSelection,
	photoChangePreview:        photoChangeCurrentVersionSelection,
	"job":                     `SELECT a.asset_id FROM rendition_job_waiters w JOIN content_versions v ON v.version_id=w.content_version_id JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id JOIN photo_files f ON f.node_id=n.id JOIN photo_assets a ON a.asset_id=f.asset_id WHERE w.job_id IN (SELECT value FROM json_each(?))`,
}

type photoSourceChange struct{ kind, key, node string }

func (s *Store) readPhotoChanges(ctx context.Context, tx *sql.Tx, capture bool) ([]PhotoEvent, []photoSourceChange) {
	fallback := []PhotoEvent{{Refetch: true}}
	if !capture {
		return fallback, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT source,key,related FROM docbank_photo_changes`)
	if err != nil {
		s.warnPhotoCapture(err)
		return fallback, nil
	}
	defer func() { _ = rows.Close() }()
	kinds := map[string]string{}
	for _, r := range photoEventCaptureRules {
		kinds[r.table] = r.kind
	}
	keys := map[string][]string{}
	var sources []photoSourceChange
	count := 0
	refetch := false
	for rows.Next() {
		var source, key, related string
		if err = rows.Scan(&source, &key, &related); err != nil {
			break
		}
		count++
		kind := kinds[source]
		if source == "hash" || kind == "source" || kind == "generation" {
			if source == "hash" {
				sources = append(sources, photoSourceChange{source, key, related})
			} else {
				sources = append(sources, photoSourceChange{kind, related, ""})
			}
		} else if source == photoChangeRefetch || kind == photoChangeRefetch {
			refetch = true
		} else {
			keys[kind] = append(keys[kind], related)
		}
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		s.warnPhotoCapture(err)
		return fallback, nil
	}
	if count > 64 || refetch {
		return fallback, nil
	}
	var events []PhotoEvent
	seen := map[PhotoEvent]bool{}
	resolved := 0
	for kind, values := range keys {
		encoded, _ := json.Marshal(values)
		rows, err := tx.QueryContext(ctx, photoChangeSelections[kind]+" LIMIT ?", string(encoded), 65-resolved)
		if err != nil {
			s.warnPhotoCapture(err)
			return fallback, nil
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			event := PhotoEvent{Refetch: kind != photoChangePreview && kind != "job"}
			if err = rows.Scan(&event.AssetID); err != nil {
				break
			}
			resolved++
			if resolved == 65 {
				_ = rows.Close()
				return fallback, nil
			}
			if !seen[event] {
				seen[event] = true
				events = append(events, event)
			}
		}
		err = errors.Join(err, rows.Err(), rows.Close())
		if err != nil {
			s.warnPhotoCapture(err)
			return fallback, nil
		}
	}
	return events, sources
}

// Resolve hash-based keys only after releasing the writer. Materialize the 64+1
// candidates before checking current membership, including ordinary and old versions.
func (s *Store) resolvePhotoSources(ctx context.Context, sources []photoSourceChange) []PhotoEvent {
	if len(sources) == 0 {
		return nil
	}
	values := make([][3]string, 0, len(sources))
	for _, source := range sources {
		values = append(values, [3]string{source.kind, source.key, source.node})
	}
	encoded, _ := json.Marshal(values)
	rows, err := s.db.QueryContext(ctx, `WITH raw AS (
 SELECT json_extract(value,'$[0]') kind,json_extract(value,'$[1]') key,json_extract(value,'$[2]') node FROM json_each(?)
 ), changed AS (
 SELECT kind,key hash,node FROM raw WHERE kind<>'generation'
 UNION ALL SELECT raw.kind,g.source_sha256,raw.node FROM raw JOIN source_metadata_generations g ON g.generation_id=raw.key WHERE raw.kind='generation'
 ), candidates AS MATERIALIZED (
 SELECT changed.kind,changed.node,v.version_id,v.node_id FROM changed
 CROSS JOIN content_versions v INDEXED BY content_versions_blob ON v.blob_hash=changed.hash LIMIT 65
 ), `+CurrentContentMembershipCTE+`
 SELECT c.kind,(SELECT f.asset_id FROM current_content_members current
 JOIN photo_files f ON f.node_id=current.node_id AND f.asset_id IS NOT NULL
 WHERE current.node_id=c.node_id AND current.version_id=c.version_id
 AND (c.kind<>'hash' OR current.node_id<>CAST(c.node AS INTEGER)) LIMIT 1)
 FROM candidates c`, string(encoded))
	fallback := []PhotoEvent{{Refetch: true}}
	if err != nil {
		s.warnPhotoCapture(err)
		return fallback
	}
	defer func() { _ = rows.Close() }()
	var events []PhotoEvent
	seen := map[string]bool{}
	count := 0
	for rows.Next() {
		var kind string
		var asset sql.NullString
		if err = rows.Scan(&kind, &asset); err != nil {
			break
		}
		count++
		if count > 64 {
			return fallback
		}
		if !asset.Valid {
			continue
		}
		if kind == "hash" {
			return fallback
		}
		if !seen[asset.String] {
			seen[asset.String] = true
			events = append(events, PhotoEvent{AssetID: asset.String, Refetch: true})
		}
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		s.warnPhotoCapture(err)
		return fallback
	}
	return events
}
