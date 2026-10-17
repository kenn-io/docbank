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
	warn := len(f.subscribers) > 0 && !f.captureDegraded
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

const photoChangeRefetch = "refetch"

// Related keys survive deletion of the row that originally linked a worker result to a photo.
type photoEventCaptureRule struct{ table, key, relation, kind, columns string }

func photoEventCaptureRules() []photoEventCaptureRule {
	return []photoEventCaptureRule{
		{"photo_assets", "asset_id", "asset_id", "asset", "revision,hidden_at,excluded_at,display_file_id"},
		{"photo_files", "file_id", "asset_id", "asset", "file_id,node_id,asset_id,revision,rating,flag,label,caption,creator,copyright,rotation,role,sidecar_of_file_id"},
		{"nodes", "id", "id", "node", "revision,name,parent_id,current_version_id,trashed_at,created_at,modified_at"},
		{"node_tags", "node_id", "node_id", "node", ""},
		{metadataProvenanceType, "node_id", "node_id", "node", ""},
		{"extracted_text", "blob_hash", "blob_hash", "source", "status,text"},
		{"text_searchable_versions", "version_id", "version_id", "current_version", ""},
		{"source_metadata_heads", "source_sha256", "source_sha256", "source", "generation_id"},
		{"photo_technical_metadata", "generation_id", "generation_id", "generation", ""},
		{"photo_faces", "face_id", "content_version_id", "current_version", "content_version_id,x,y,width,height,revision,assertion_id"},
		{"photo_quality_signals", metadataContentVersionIDField, metadataContentVersionIDField, "current_version", ""},
		{"visual_preview_generations", metadataContentVersionIDField, metadataContentVersionIDField, "current_version", ""},
		{"visual_preview_heads", metadataContentVersionIDField, metadataContentVersionIDField, "current_version", "generation_id"},
		{"rendition_heads", metadataContentVersionIDField, metadataContentVersionIDField, "current_version", "attachment_id"},
		{"person_document_assertions", metadataContentVersionIDField, metadataContentVersionIDField, "current_version", ""},
		{"photo_sets", "set_id", "set_id", photoChangeRefetch, ""},
		{"photo_set_members", "set_id", "asset_id", photoChangeRefetch, ""},
		{"tags", "id", "id", photoChangeRefetch, "name"},
		{"saved_queries", "id", "id", photoChangeRefetch, ""},
		{"collection_labels", "ingest_id", "ingest_id", photoChangeRefetch, ""},
		{"person_aliases", "retired_person_id", "retired_person_id", photoChangeRefetch, ""},
		{"persons", "person_id", "person_id", photoChangeRefetch, "display_name_folded,state,revision"},
		{"photo_library_settings", "singleton", "singleton", photoChangeRefetch, ""},
		{"photo_hidden_credentials", "singleton", "singleton", photoChangeRefetch, ""},
	}
}

func installPhotoChangeTriggers(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE docbank_photo_capture(enabled INTEGER); INSERT INTO docbank_photo_capture VALUES(0); CREATE TEMP TABLE docbank_photo_changes(source TEXT,key TEXT,related TEXT,PRIMARY KEY(source,key,related)) WITHOUT ROWID`); err != nil {
		return err
	}
	// The 65th distinct key represents overflow. Further rows do no capture work.
	record := func(source, key, related string) string {
		return `INSERT OR IGNORE INTO docbank_photo_changes SELECT '` + source + `',CAST(` + key + ` AS TEXT),CAST(` + related + ` AS TEXT) WHERE ` + related + ` IS NOT NULL AND (SELECT count(*) FROM docbank_photo_changes)<65;`
	}
	install := func(name, action, table, when, body string) error {
		_, err := tx.ExecContext(ctx, `CREATE TEMP TRIGGER photo_events_`+name+` BEFORE `+action+` ON main.`+table+` WHEN (SELECT enabled FROM docbank_photo_capture) AND (SELECT count(*) FROM docbank_photo_changes)<65 AND (`+when+`) BEGIN `+body+` END`)
		return err
	}
	for _, r := range photoEventCaptureRules() {
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
				insert := record(r.table, ref+"."+r.key, ref+"."+r.relation)
				if r.kind == "node" {
					insert = strings.Replace(insert, " WHERE ", " WHERE EXISTS(SELECT 1 FROM photo_files WHERE node_id="+ref+"."+r.relation+" AND asset_id IS NOT NULL) AND ", 1)
				}
				body.WriteString(insert)
			}
			if err := install(r.table+"_"+action, action, r.table, when, body.String()); err != nil {
				return fmt.Errorf("photo capture %s: %w", r.table, err)
			}
		}
	}
	if err := install("hide", "UPDATE", "photo_assets", "OLD.hidden_at IS NULL AND NEW.hidden_at IS NOT NULL", record(photoChangeRefetch, "''", "''")); err != nil {
		return err
	}
	if err := install("directory", "UPDATE", "nodes", "OLD.kind='dir' AND (OLD.name IS NOT NEW.name OR OLD.parent_id IS NOT NEW.parent_id)", record(photoChangeRefetch, "''", "''")); err != nil {
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
			body.WriteString(record("hash", "(SELECT blob_hash FROM content_versions WHERE version_id="+ref+".current_version_id)", ref+".id"))
		}
		when := "1"
		if action == "UPDATE" {
			when = "OLD.current_version_id IS NOT NEW.current_version_id OR OLD.trashed_at IS NOT NEW.trashed_at"
		}
		if err := install("hash_"+action, action, "nodes", when, body.String()); err != nil {
			return err
		}
	}
	return install("initial_hash", "INSERT", "content_versions", "EXISTS(SELECT 1 FROM nodes WHERE id=NEW.node_id AND current_version_id=NEW.version_id)", record("hash", "NEW.blob_hash", "NEW.node_id"))
}

type photoHashChange struct{ hash, node string }

func (s *Store) readPhotoChanges(ctx context.Context, tx *sql.Tx, capture bool) ([]PhotoEvent, []photoHashChange) {
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
	for _, r := range photoEventCaptureRules() {
		kinds[r.table] = r.kind
	}
	keys := map[string][]string{}
	var hashes []photoHashChange
	count := 0
	refetch := false
	for rows.Next() {
		var source, key, related string
		if err = rows.Scan(&source, &key, &related); err != nil {
			break
		}
		count++
		kind := kinds[source]
		if source == "hash" {
			hashes = append(hashes, photoHashChange{key, related})
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
		return fallback, hashes
	}
	var events []PhotoEvent
	seen := map[string]bool{}
	selections := map[string]string{
		"asset":           `SELECT keys.value,COALESCE(a.revision,0) FROM json_each(?) keys LEFT JOIN photo_assets a ON a.asset_id=keys.value`,
		"node":            `SELECT a.asset_id,a.revision FROM photo_files f JOIN photo_assets a ON a.asset_id=f.asset_id WHERE f.node_id IN (SELECT value FROM json_each(?))`,
		"current_version": `SELECT a.asset_id,a.revision FROM content_versions v JOIN nodes n ON n.current_version_id=v.version_id JOIN photo_files f ON f.node_id=n.id JOIN photo_assets a ON a.asset_id=f.asset_id WHERE v.version_id IN (SELECT value FROM json_each(?))`,
		"source":          `SELECT a.asset_id,a.revision FROM content_versions v JOIN nodes n ON n.current_version_id=v.version_id JOIN photo_files f ON f.node_id=n.id JOIN photo_assets a ON a.asset_id=f.asset_id WHERE v.blob_hash IN (SELECT value FROM json_each(?))`,
		"generation":      `SELECT a.asset_id,a.revision FROM source_metadata_generations g JOIN content_versions v ON v.blob_hash=g.source_sha256 JOIN nodes n ON n.current_version_id=v.version_id JOIN photo_files f ON f.node_id=n.id JOIN photo_assets a ON a.asset_id=f.asset_id WHERE g.generation_id IN (SELECT value FROM json_each(?))`,
	}
	for kind, values := range keys {
		encoded, _ := json.Marshal(values)
		rows, err := tx.QueryContext(ctx, selections[kind], string(encoded))
		if err != nil {
			s.warnPhotoCapture(err)
			return fallback, hashes
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var event PhotoEvent
			if err = rows.Scan(&event.AssetID, &event.Revision); err != nil {
				break
			}
			if !seen[event.AssetID] {
				seen[event.AssetID] = true
				events = append(events, event)
			}
		}
		err = errors.Join(err, rows.Err(), rows.Close())
		if err != nil {
			s.warnPhotoCapture(err)
			return fallback, hashes
		}
	}
	if len(events) > 64 {
		return fallback, hashes
	}
	return events, hashes
}

func (s *Store) photoDuplicateChanged(ctx context.Context, hashes []photoHashChange) bool {
	if len(hashes) == 0 {
		return false
	}
	values := make([][2]string, 0, len(hashes))
	for _, h := range hashes {
		values = append(values, [2]string{h.hash, h.node})
	}
	encoded, _ := json.Marshal(values)
	var found bool
	// Hash lookup starts at the existing blob index; current membership rejects historical versions.
	err := s.db.QueryRowContext(ctx, `WITH `+CurrentContentMembershipCTE+` SELECT EXISTS(
 SELECT 1 FROM json_each(?) changed JOIN content_versions v INDEXED BY content_versions_blob ON v.blob_hash=json_extract(changed.value,'$[0]')
 JOIN current_content_members current ON current.version_id=v.version_id
 JOIN photo_files f ON f.node_id=current.node_id AND f.asset_id IS NOT NULL
 WHERE current.node_id<>CAST(json_extract(changed.value,'$[1]') AS INTEGER))`, string(encoded)).Scan(&found)
	if err != nil {
		s.warnPhotoCapture(err)
		return true
	}
	return found
}
