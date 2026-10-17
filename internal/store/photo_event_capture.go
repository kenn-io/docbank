package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
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

func (s *Store) photoCaptureSucceeded() {
	f := &s.photoEvents
	f.Lock()
	f.captureDegraded = false
	f.Unlock()
}

func (s *Store) clearIdlePhotoCapture() {
	// Wait briefly for a busy writer; the next write removes the capture otherwise.
	wait, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := s.writeDB.Conn(wait)
	if err != nil {
		return
	}
	// A slow cleanup finishes once it holds the writer, rather than leaving the capture pending.
	ctx := context.Background()
	defer func() { _ = conn.Close() }()
	f := &s.photoEvents
	f.Lock()
	defer f.Unlock()
	if len(f.subscribers) > 0 || f.closed || !f.cleanupPending {
		return
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	if dropIdlePhotoCapture(ctx, tx) == nil {
		if tx.Commit() == nil {
			f.cleanupPending = false
		}
	}
}

func (s *Store) prepareIdlePhotoCleanup(ctx context.Context, tx *sql.Tx) bool {
	f := &s.photoEvents
	f.Lock()
	pending := f.cleanupPending && len(f.subscribers) == 0
	f.Unlock()
	if !pending {
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT photo_events_cleanup`); err != nil {
		return false
	}
	err := dropIdlePhotoCapture(ctx, tx)
	if err == nil {
		_, err = tx.ExecContext(ctx, `RELEASE photo_events_cleanup`)
	}
	if err != nil {
		_, _ = tx.ExecContext(ctx, `ROLLBACK TO photo_events_cleanup`)
		_, _ = tx.ExecContext(ctx, `RELEASE photo_events_cleanup`)
		return false
	}
	return true
}

func (s *Store) finishIdlePhotoCleanup(epoch uint64) {
	f := &s.photoEvents
	f.Lock()
	defer f.Unlock()
	if f.epoch == epoch && len(f.subscribers) == 0 {
		f.cleanupPending = false
	}
}

func dropIdlePhotoCapture(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM temp.sqlite_schema WHERE type='trigger' AND name LIKE 'photo_events_%'`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		names = append(names, name)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER temp."`+strings.ReplaceAll(name, `"`, `""`)+`"`); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.docbank_photo_peers; DROP TABLE IF EXISTS temp.docbank_photo_changes`)
	return err
}

// Temporary triggers follow the writer connection, including its replacement after a failure.
func (s *Store) preparePhotoChanges(ctx context.Context, tx *sql.Tx, hidden bool) bool {
	var present int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM temp.sqlite_schema WHERE name='docbank_photo_changes'`).Scan(&present); err != nil {
		s.warnPhotoCapture(err)
		return false
	}
	if present != 0 {
		_, err := tx.ExecContext(ctx, `DELETE FROM docbank_photo_changes`)
		if err != nil {
			s.warnPhotoCapture(err)
		}
		return err == nil && s.preparePhotoAudience(ctx, tx, hidden)
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT photo_events`); err != nil {
		s.warnPhotoCapture(err)
		return false
	}
	if err := installPhotoChangeTriggers(ctx, tx); err != nil {
		s.warnPhotoCapture(err)
		_, _ = tx.ExecContext(ctx, `ROLLBACK TO photo_events`)
		_, _ = tx.ExecContext(ctx, `RELEASE photo_events`)
		return false
	}
	_, err := tx.ExecContext(ctx, `RELEASE photo_events`)
	if err != nil {
		s.warnPhotoCapture(err)
	}
	return err == nil && s.preparePhotoAudience(ctx, tx, hidden)
}

func (s *Store) preparePhotoAudience(ctx context.Context, tx *sql.Tx, hidden bool) bool {
	if hidden {
		return true
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO docbank_photo_changes(kind,id,audience) VALUES('disabled','',2)`)
	if err != nil {
		s.warnPhotoCapture(err)
	}
	return err == nil
}

func photoEventNodeSelection(value string) string {
	return `SELECT 'asset',asset_id FROM photo_files WHERE node_id=` + value + ` AND asset_id IS NOT NULL`
}

func installPhotoChangeTriggers(ctx context.Context, tx *sql.Tx) error {
	const nodeTable = "nodes"
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE docbank_photo_changes(kind TEXT,id TEXT,audience INTEGER,PRIMARY KEY(audience,kind,id)) WITHOUT ROWID`); err != nil {
		return err
	}
	if err := installPhotoPeerCapture(ctx, tx); err != nil {
		return err
	}
	node := photoEventNodeSelection
	version := photoEventVersionSelection
	source := photoEventSourceSelection
	global := func(_ string) string { return `SELECT 'refetch',''` }
	rules := []struct {
		table, key, columns string
		selectIDs           func(string) string
	}{
		{"photo_assets", "asset_id", "revision,hidden_at,excluded_at,display_file_id", func(v string) string { return `SELECT 'asset',` + v }},
		{"photo_files", "asset_id", "asset_id,revision,rating,flag,label,caption,creator,copyright,rotation,role,sidecar_of_file_id", func(v string) string { return `SELECT 'asset',` + v + ` WHERE ` + v + ` IS NOT NULL` }},
		{"photo_sets", "set_id", "name,starred,cover_asset_id,deleted_at", func(v string) string { return `SELECT 'album',` + v }},
		{"photo_set_members", "set_id", "", func(v string) string { return `SELECT 'album',` + v }},
		{nodeTable, "id", "revision,name,parent_id,current_version_id,trashed_at,created_at,modified_at", node},
		{"node_tags", "node_id", "", node},
		{"tags", "id", "name", global},
		{"saved_queries", "id", "", global},
		{"collection_labels", "ingest_id", "", global},
		{metadataProvenanceType, "node_id", "", node},
		{"extracted_text", "blob_hash", "status,text", source},
		{"text_searchable_versions", "version_id", "", version},
		{"source_metadata_heads", "source_sha256", "generation_id", source},
		{"photo_technical_metadata", "generation_id", "", photoEventMetadataSelection},
		{"photo_quality_signals", metadataContentVersionIDField, "", version},
		{"visual_preview_generations", metadataContentVersionIDField, "", version},
		{"visual_preview_heads", metadataContentVersionIDField, "generation_id", version},
		{"rendition_heads", metadataContentVersionIDField, "attachment_id", version},
		{"person_document_assertions", metadataContentVersionIDField, "", version},
		{"person_aliases", "retired_person_id", "", global},
		{"photo_library_settings", "singleton", "", global},
		{"photo_hidden_credentials", "singleton", "", global},
	}
	// Each audience keeps 64 identities and one overflow marker.
	done := func(bucket int) string {
		predicate := "kind='refetch'"
		if bucket == 2 {
			predicate = "kind IN ('refetch','disabled')"
		}
		return fmt.Sprintf("EXISTS(SELECT 1 FROM docbank_photo_changes WHERE audience=%d AND %s)", bucket, predicate)
	}
	insert := func(selection, visibility string, indirect bool) string {
		classified := photoEventAudienceSelection(selection, visibility)
		var body strings.Builder
		for _, bucket := range []int{1, 2} {
			finished := done(bucket)
			if bucket == 2 {
				finished = "(" + done(1) + " OR " + finished + ")"
			}
			if indirect {
				_, _ = fmt.Fprintf(&body, "INSERT OR IGNORE INTO docbank_photo_changes SELECT 'refetch','',%d WHERE NOT %s AND EXISTS(SELECT 1 FROM (%s) candidate WHERE candidate.audience=%d);", bucket, finished, classified, bucket)
				continue
			}
			_, _ = fmt.Fprintf(&body, "INSERT INTO docbank_photo_changes SELECT DISTINCT candidate.kind,candidate.id,candidate.audience FROM (%s) candidate WHERE candidate.audience=%d AND NOT EXISTS(SELECT 1 FROM docbank_photo_changes recorded WHERE recorded.audience=candidate.audience AND recorded.kind=candidate.kind AND recorded.id=candidate.id) LIMIT CASE WHEN %s THEN 0 ELSE MAX(0,65-(SELECT count(*) FROM docbank_photo_changes WHERE audience=%d)) END;", classified, bucket, finished, bucket)
			_, _ = fmt.Fprintf(&body, "UPDATE docbank_photo_changes SET kind='refetch',id='' WHERE (audience,kind,id) IN (SELECT audience,kind,id FROM docbank_photo_changes WHERE audience=%d ORDER BY kind,id LIMIT CASE WHEN %s THEN 0 ELSE 1 END OFFSET 64);", bucket, finished)
		}
		return body.String()
	}
	// Public overflow refetches every viewer; Hidden overflow must keep seeking public changes.
	guard := " WHEN NOT " + done(1)

	for _, rule := range rules {
		for _, action := range []string{"INSERT", "UPDATE", "DELETE"} {
			refs := []string{"NEW"}
			timing := "AFTER"
			if action == "DELETE" {
				refs = []string{"OLD"}
				timing = "BEFORE"
			}
			if action == "UPDATE" {
				refs = []string{"OLD", "NEW"}
				timing = "BEFORE"
			}
			when := guard
			if rule.table == nodeTable || rule.table == metadataProvenanceType || rule.table == "node_tags" {
				var eligible []string
				for _, ref := range refs {
					eligible = append(eligible, "EXISTS(SELECT 1 FROM photo_files WHERE node_id="+ref+"."+rule.key+" AND asset_id IS NOT NULL)")
				}
				when += " AND (" + strings.Join(eligible, " OR ") + ")"
			}
			if action == "UPDATE" && rule.columns != "" {
				var changed []string
				for column := range strings.SplitSeq(rule.columns, ",") {
					changed = append(changed, "OLD."+column+" IS NOT NEW."+column)
				}
				when += " AND (" + strings.Join(changed, " OR ") + ")"
			}
			var body strings.Builder
			for _, ref := range refs {
				visibility := ""
				if rule.table == "photo_assets" {
					visibility = ref + ".hidden_at"
				}
				if rule.table == "photo_set_members" {
					visibility = "(SELECT hidden_at FROM photo_assets WHERE asset_id=" + ref + ".asset_id)"
				}
				if rule.table == "photo_sets" && action == "UPDATE" {
					// Removing a member also clears its explicit cover. Keep that derived change
					// in the member's audience; clearing a retained cover is a public album edit.
					visibility = "CASE WHEN OLD.name IS NEW.name AND OLD.starred IS NEW.starred AND OLD.deleted_at IS NEW.deleted_at AND NEW.cover_asset_id IS NULL AND NOT EXISTS(SELECT 1 FROM photo_set_members WHERE set_id=OLD.set_id AND asset_id=OLD.cover_asset_id) THEN (SELECT hidden_at FROM photo_assets WHERE asset_id=OLD.cover_asset_id) END"
				}
				indirect := rule.table == metadataProvenanceType || rule.table == "extracted_text" || rule.table == "text_searchable_versions" || rule.table == "source_metadata_heads" || rule.table == "photo_technical_metadata" || rule.table == "photo_quality_signals" || rule.table == "visual_preview_generations" || rule.table == "visual_preview_heads" || rule.table == "rendition_heads" || rule.table == "person_document_assertions"
				body.WriteString(insert(rule.selectIDs(ref+"."+rule.key), visibility, indirect))
			}
			statement := fmt.Sprintf("CREATE TEMP TRIGGER photo_events_%s_%s %s %s ON main.%s%s BEGIN %s END", rule.table, action, timing, action, rule.table, when, body.String())
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("installing photo trigger on %s: %w", rule.table, err)
			}
		}
	}
	// Duplicate membership follows live current content, including ordinary document peers.
	peers := photoEventPeerSelection
	for _, action := range []string{"INSERT", "UPDATE", "DELETE"} {
		refs := []string{"NEW"}
		timing := "AFTER"
		when := guard + " AND NEW.kind='file'"
		if action == "DELETE" {
			refs = []string{"OLD"}
			timing = "BEFORE"
			when = guard + " AND OLD.kind='file'"
		}
		if action == "UPDATE" {
			refs = []string{"OLD", "NEW"}
			when += " AND (OLD.current_version_id IS NOT NEW.current_version_id OR OLD.trashed_at IS NOT NEW.trashed_at)"
		}
		var eligible []string
		for _, ref := range refs {
			eligible = append(eligible, "EXISTS("+peers(ref+".current_version_id")+")")
		}
		when += " AND (" + strings.Join(eligible, " OR ") + ")"
		var body strings.Builder
		for _, ref := range refs {
			body.WriteString(insert(peers(ref+".current_version_id"), "", true))
		}
		statement := fmt.Sprintf("CREATE TEMP TRIGGER photo_events_peers_%s %s %s ON main.nodes%s BEGIN %s END", action, timing, action, when, body.String())
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	// Initial nodes precede their content version; the insert completes duplicate membership.
	photoPeer := "EXISTS(" + peers("NEW.version_id") + ")"
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TRIGGER photo_events_initial_content AFTER INSERT ON main.content_versions`+guard+` AND EXISTS(SELECT 1 FROM nodes WHERE id=NEW.node_id AND current_version_id=NEW.version_id AND trashed_at IS NULL) AND `+photoPeer+` BEGIN `+insert(peers("NEW.version_id"), "", true)+` END`); err != nil {
		return err
	}
	// `person:` resolves names and revisions through persons. A new person changes resolution
	// only when it duplicates a name, so other inserts, such as people backfill, stay quiet.
	people := []struct{ action, when string }{
		{"INSERT", " AND NEW.state<>'retired' AND EXISTS(SELECT 1 FROM persons other WHERE other.display_name_folded=NEW.display_name_folded AND other.person_id<>NEW.person_id AND other.state<>'retired')"},
		{"UPDATE", " AND (OLD.display_name_folded IS NOT NEW.display_name_folded OR OLD.state IS NOT NEW.state OR OLD.revision IS NOT NEW.revision)"},
		{"DELETE", ""},
	}
	for _, rule := range people {
		statement := `CREATE TEMP TRIGGER photo_events_persons_` + rule.action + ` AFTER ` + rule.action + ` ON main.persons` + guard + rule.when + ` BEGIN ` + insert(global(""), "", true) + ` END`
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("installing photo trigger on persons: %w", err)
		}
	}
	// Directory paths affect a query without updating descendant rows.
	_, err := tx.ExecContext(ctx, `CREATE TEMP TRIGGER photo_events_subtree BEFORE UPDATE OF parent_id,name ON main.nodes`+guard+` AND OLD.kind='dir' AND (OLD.parent_id IS NOT NEW.parent_id OR OLD.name IS NOT NEW.name) BEGIN `+insert(global(""), "", true)+` END`)

	return err
}

func (s *Store) readPhotoChanges(ctx context.Context, tx *sql.Tx, capture bool) []PhotoEvent {
	refetch := []PhotoEvent{{Refetch: true, audience: photoDegraded}}
	if !capture {
		return refetch
	}
	rows, err := tx.QueryContext(ctx, `SELECT kind,id,audience FROM docbank_photo_changes c WHERE kind!='disabled' AND (kind='refetch' OR NOT EXISTS(SELECT 1 FROM docbank_photo_changes filled WHERE filled.audience=c.audience AND filled.kind='refetch'))`)
	if err != nil {
		s.warnPhotoCapture(err)
		return refetch
	}
	defer func() { _ = rows.Close() }()
	var events []PhotoEvent
	identities := map[string]int{}
	var overflow photoAudience
	for rows.Next() {
		var kind, id string
		var audience photoAudience
		if err := rows.Scan(&kind, &id, &audience); err != nil {
			s.warnPhotoCapture(err)
			return refetch
		}
		if kind == "refetch" {
			overflow |= audience
			continue
		}
		event := PhotoEvent{audience: audience}
		if kind == "asset" {
			event.AssetID = id
		} else {
			event.AlbumID = id
		}
		key := kind + ":" + id
		if i, ok := identities[key]; ok {
			events[i].audience |= audience
		} else {
			identities[key] = len(events)
			events = append(events, event)
		}
	}
	if err := rows.Err(); err != nil {
		s.warnPhotoCapture(err)
		return refetch
	}
	if overflow != 0 {
		events = append(events, PhotoEvent{Refetch: true, audience: overflow})
	}
	s.photoCaptureSucceeded()
	return events
}

func photoEventAudienceSelection(selection, visibility string) string {
	classification := "CASE WHEN a.hidden_at IS NOT NULL THEN 2 ELSE 1 END"
	if visibility != "" {
		classification = "CASE WHEN " + visibility + " IS NULL THEN 1 ELSE 2 END"
	}
	classified := "WITH selected(kind,id) AS (" + selection + ") SELECT selected.kind,CASE WHEN selected.kind='refetch' THEN '' ELSE selected.id END AS id," + classification + " AS audience FROM selected LEFT JOIN photo_assets a ON selected.kind!='album' AND a.asset_id=selected.id"
	if visibility == "" {
		classified += " WHERE selected.kind!='asset' OR a.asset_id IS NOT NULL"
	}
	return classified
}

func photoEventPeerSelection(version string) string {
	return `SELECT 'asset',peer.asset_id FROM content_versions changed
 CROSS JOIN docbank_photo_peers peer ON peer.blob_hash=changed.blob_hash AND peer.size=changed.size
 WHERE changed.version_id=` + version + ` AND peer.node_id<>changed.node_id`
}

func installPhotoPeerCapture(ctx context.Context, tx *sql.Tx) error {
	// This writer-local projection gives duplicate capture a photo-only hash index.
	// It follows the capture's transaction and connection lifetime.
	selection := `SELECT f.node_id,v.blob_hash,v.size,f.asset_id FROM photo_files f
 CROSS JOIN nodes n ON n.id=f.node_id AND n.trashed_at IS NULL
 CROSS JOIN content_versions v ON v.version_id=n.current_version_id WHERE f.asset_id IS NOT NULL`
	for _, statement := range []string{
		`CREATE TEMP TABLE docbank_photo_peers(node_id INTEGER PRIMARY KEY,blob_hash TEXT NOT NULL,size INTEGER NOT NULL,asset_id TEXT NOT NULL)`,
		`CREATE INDEX temp.docbank_photo_peers_blob ON docbank_photo_peers(blob_hash,size)`,
		`INSERT INTO docbank_photo_peers ` + selection,
		`CREATE TEMP TRIGGER photo_events_peer_file_insert AFTER INSERT ON main.photo_files BEGIN INSERT INTO docbank_photo_peers ` + selection + ` AND f.node_id=NEW.node_id; END`,
		`CREATE TEMP TRIGGER photo_events_peer_file_update AFTER UPDATE OF node_id,asset_id ON main.photo_files BEGIN DELETE FROM docbank_photo_peers WHERE node_id=OLD.node_id; INSERT INTO docbank_photo_peers ` + selection + ` AND f.node_id=NEW.node_id; END`,
		`CREATE TEMP TRIGGER photo_events_peer_file_delete AFTER DELETE ON main.photo_files BEGIN DELETE FROM docbank_photo_peers WHERE node_id=OLD.node_id; END`,
		`CREATE TEMP TRIGGER photo_events_peer_node_update AFTER UPDATE OF current_version_id,trashed_at ON main.nodes WHEN (OLD.current_version_id IS NOT NEW.current_version_id OR OLD.trashed_at IS NOT NEW.trashed_at) AND EXISTS(SELECT 1 FROM photo_files WHERE node_id=NEW.id) BEGIN DELETE FROM docbank_photo_peers WHERE node_id=NEW.id; INSERT INTO docbank_photo_peers ` + selection + ` AND f.node_id=NEW.id; END`,
		`CREATE TEMP TRIGGER photo_events_peer_version_insert AFTER INSERT ON main.content_versions WHEN EXISTS(SELECT 1 FROM photo_files WHERE node_id=NEW.node_id) BEGIN DELETE FROM docbank_photo_peers WHERE node_id=NEW.node_id; INSERT INTO docbank_photo_peers ` + selection + ` AND f.node_id=NEW.node_id; END`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("installing photo duplicate capture: %w", err)
		}
	}
	return nil
}

func photoEventVersionSelection(value string) string {
	return `SELECT 'asset',f.asset_id FROM content_versions v CROSS JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id CROSS JOIN photo_files f ON f.node_id=n.id WHERE v.version_id=` + value + ` AND f.asset_id IS NOT NULL`
}
func photoEventSourceSelection(value string) string {
	return `SELECT 'asset',f.asset_id FROM content_versions v CROSS JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id CROSS JOIN photo_files f ON f.node_id=n.id WHERE v.blob_hash=` + value + ` AND f.asset_id IS NOT NULL`
}
func photoEventMetadataSelection(value string) string {
	return `SELECT 'asset',f.asset_id FROM source_metadata_heads h CROSS JOIN content_versions v ON v.blob_hash=h.source_sha256 CROSS JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id CROSS JOIN photo_files f ON f.node_id=n.id WHERE h.generation_id=` + value + ` AND f.asset_id IS NOT NULL`
}
