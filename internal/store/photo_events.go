package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// PhotoEventIDLimit bounds identified changes in one stream batch.
const PhotoEventIDLimit = 64

// PhotoEvent identifies a changed photo, or requests a full refetch when AssetID is empty.
type PhotoEvent struct {
	AssetID    string `json:"asset_id,omitempty"`
	hiddenOnly bool   // Internal access scope survives ID-free batching.
}

// CollapsePhotoEvents keeps one event per photo. More than
// PhotoEventIDLimit photos, or any ID-free event, becomes one ID-free refetch.
func CollapsePhotoEvents(events []PhotoEvent) []PhotoEvent {
	batch := photoEventBatch{}
	for _, event := range events {
		batch.add(event)
	}
	return batch.events()
}

// Every capture and overflow uses the same bounded fold, including its access scope.
type photoEventBatch struct {
	byID       map[string]PhotoEvent
	refetch    bool
	hasVisible bool
}

func (b *photoEventBatch) add(event PhotoEvent) {
	b.hasVisible = b.hasVisible || !event.hiddenOnly
	b.refetch = b.refetch || event.AssetID == ""
	if b.refetch {
		return
	}
	if b.byID == nil {
		b.byID = make(map[string]PhotoEvent)
	}
	b.byID[event.AssetID] = event
	if len(b.byID) > PhotoEventIDLimit {
		b.refetch = true
		b.byID = nil
	}
}

func (b *photoEventBatch) events() []PhotoEvent {
	if b.refetch {
		return []PhotoEvent{{hiddenOnly: !b.hasVisible}}
	}
	collapsed := make([]PhotoEvent, 0, len(b.byID))
	for _, event := range b.byID {
		collapsed = append(collapsed, event)
	}
	slices.SortFunc(collapsed, func(a, b PhotoEvent) int { return strings.Compare(a.AssetID, b.AssetID) })
	return collapsed
}

type photoEventFeed struct {
	sync.Mutex

	closed      bool
	subscribers map[chan []PhotoEvent]struct{}
	installed   atomic.Bool // The writer connection holds capture triggers.
}

// SubscribePhotoEvents delivers committed photo changes until the caller unsubscribes or the store closes.
func (s *Store) SubscribePhotoEvents() (<-chan []PhotoEvent, func()) {
	f := &s.photoEvents
	f.Lock()
	defer f.Unlock()
	ch := make(chan []PhotoEvent, 1)
	if f.closed {
		close(ch)
		return ch, func() {}
	}
	if f.subscribers == nil {
		f.subscribers = make(map[chan []PhotoEvent]struct{})
	}
	f.subscribers[ch] = struct{}{}
	return ch, func() {
		f.Lock()
		defer f.Unlock()
		if _, ok := f.subscribers[ch]; ok {
			delete(f.subscribers, ch)
			close(ch)
		}
	}
}

func (s *Store) hasPhotoSubscribers() bool {
	s.photoEvents.Lock()
	defer s.photoEvents.Unlock()
	return len(s.photoEvents.subscribers) > 0
}

func (s *Store) closePhotoEvents() {
	f := &s.photoEvents
	f.Lock()
	defer f.Unlock()
	f.closed = true
	for ch := range f.subscribers {
		close(ch)
		delete(f.subscribers, ch)
	}
}

func (s *Store) publishPhotoEvents(events []PhotoEvent) {
	if len(events) == 0 {
		return
	}
	f := &s.photoEvents
	f.Lock()
	defer f.Unlock()
	for ch := range f.subscribers {
		select {
		case ch <- events:
		default:
			var pending []PhotoEvent
			select {
			case pending = <-ch:
			default:
			}
			// Overflow requests a refetch within the combined changes' access scope.
			batch := photoEventBatch{refetch: true}
			for _, event := range slices.Concat(pending, events) {
				batch.add(event)
			}
			ch <- batch.events()
		}
	}
}

// Capture OLD and NEW asset IDs; definitions use the empty ID for a full refetch.
// An update value of "-" skips UPDATE capture for immutable rows.
// Triggers drain photo_timeline_pending into captured photo_timeline rows in the
// same transaction. Photos reads use the captured person, assertion, and
// face rows, while person_identities and person_splits serve identity/split replay.
// Album membership and person aliases/external identities update photo_sets or
// persons in the same transaction, which supplies their notification.
// photo_change_receipts records history alongside captured authority changes.
var photoCaptureDependencies = []struct{ table, ids, update string }{
	{"photo_assets", `SELECT $row.asset_id AS id`, ""},
	{"photo_files", `SELECT $row.asset_id AS id`, ""},
	{"photo_timeline", `SELECT asset_id AS id FROM photo_files WHERE file_id=$row.file_id`, ""},
	{photoCaptureNodes, `SELECT asset_id AS id FROM photo_files WHERE node_id=$row.id`, ""},
	{photoCaptureNodes, `WITH RECURSIVE descendants(id) AS (SELECT $row.id UNION ALL SELECT n.id FROM nodes n JOIN descendants d ON n.parent_id=d.id) SELECT f.asset_id AS id FROM photo_files f JOIN descendants d ON d.id=f.node_id WHERE $row.kind='dir'`, "OLD.name IS NOT NEW.name OR OLD.parent_id IS NOT NEW.parent_id OR OLD.trashed_at IS NOT NEW.trashed_at"},
	// Duplicate status follows version and trash changes.
	{photoCaptureNodes, photoCaptureDuplicateNodeIDs, "OLD.current_version_id IS NOT NEW.current_version_id OR OLD.trashed_at IS NOT NEW.trashed_at"},
	{"content_versions", photoCaptureDuplicateVersionIDs, ""},
	{"provenance", `SELECT asset_id AS id FROM photo_files WHERE node_id=$row.node_id`, "-"},
	{"source_metadata_heads", `SELECT f.asset_id AS id FROM content_versions v JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id JOIN photo_files f ON f.node_id=n.id WHERE v.blob_hash=$row.source_sha256`, ""},
	{"photo_technical_metadata", `SELECT f.asset_id AS id FROM source_metadata_heads h JOIN content_versions v ON v.blob_hash=h.source_sha256 JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id JOIN photo_files f ON f.node_id=n.id WHERE h.generation_id=$row.generation_id`, ""},
	{"visual_preview_heads", photoCaptureVersionIDs, ""},
	{"visual_preview_generations", photoCaptureVersionIDs, "-"},
	{"photo_quality_signals", photoCaptureVersionIDs, ""},
	{"person_document_assertions", photoCaptureVersionIDs, ""},
	{"photo_faces", `SELECT f.asset_id AS id FROM content_versions v JOIN photo_files f ON f.node_id=v.node_id WHERE v.version_id=$row.content_version_id`, ""},
	{"person_candidate_faces", photoCaptureVersionIDs, ""},
	{"person_match_candidates", `SELECT f.asset_id AS id FROM person_candidate_faces c JOIN content_versions v ON v.version_id=c.content_version_id JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id JOIN photo_files f ON f.node_id=n.id WHERE c.candidate_id=$row.candidate_id`, ""},
	{"rendition_heads", photoCaptureVersionIDs, ""},
	{"rendition_job_waiters", photoCaptureVersionIDs, ""},
	{"rendition_jobs", `SELECT f.asset_id AS id FROM rendition_job_waiters w JOIN content_versions v ON v.version_id=w.content_version_id JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id JOIN photo_files f ON f.node_id=n.id WHERE w.job_id=$row.job_id`, "OLD.state IS NOT NEW.state OR OLD.phase IS NOT NEW.phase OR OLD.failure_code IS NOT NEW.failure_code"},
	{"text_searchable_versions", `SELECT f.asset_id AS id FROM content_versions v JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id JOIN photo_files f ON f.node_id=n.id WHERE v.version_id=$row.version_id`, ""},
	{"extracted_text", `SELECT f.asset_id AS id FROM content_versions v JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id JOIN photo_files f ON f.node_id=n.id WHERE v.blob_hash=$row.blob_hash`, ""},
	{"photo_sets", `SELECT '' AS id`, ""},
	{"photo_library_settings", `SELECT '' AS id`, ""},
	{"photo_hidden_credentials", `SELECT '' AS id`, ""},
	{"tags", `SELECT '' AS id`, "OLD.name IS NOT NEW.name OR OLD.revision IS NOT NEW.revision"},
	{"persons", `SELECT '' AS id`, ""},
	{"saved_queries", `SELECT '' AS id`, ""},
	{"collection_labels", `SELECT '' AS id`, ""},
}

const photoCaptureDuplicateNodeIDs = `SELECT f.asset_id AS id FROM content_versions v JOIN content_versions pv ON pv.blob_hash=v.blob_hash AND pv.size=v.size JOIN nodes n ON n.id=pv.node_id AND n.current_version_id=pv.version_id JOIN photo_files f ON f.node_id=n.id WHERE v.version_id=$row.current_version_id`
const photoCaptureDuplicateVersionIDs = `SELECT '' AS id WHERE EXISTS(SELECT 1 FROM content_versions pv JOIN nodes n ON n.id=pv.node_id AND n.current_version_id=pv.version_id JOIN photo_files f ON f.node_id=n.id WHERE pv.blob_hash=$row.blob_hash AND pv.size=$row.size)`

const photoCaptureNodes = "nodes"
const photoCaptureVersionIDs = `SELECT f.asset_id AS id FROM content_versions v JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id JOIN photo_files f ON f.node_id=n.id WHERE v.version_id=$row.content_version_id`

// Capture triggers run only while a subscribed transaction holds this flag.
const photoCaptureActive = `EXISTS(SELECT 1 FROM temp.photo_event_capture_active) AND NOT EXISTS(SELECT 1 FROM temp.photo_event_capture WHERE id='')`

type photoCaptureTrigger struct{ name, create string }

var photoCaptureTriggers = sync.OnceValue(func() []photoCaptureTrigger {
	// UPSERT keeps nested trigger conflict policies from changing capture deduplication.
	triggers := []photoCaptureTrigger{{"photo_event_hidden", `CREATE TEMP TRIGGER photo_event_hidden AFTER UPDATE OF hidden_at ON main.photo_assets
WHEN ` + photoCaptureActive + ` AND OLD.hidden_at IS NOT NEW.hidden_at BEGIN INSERT INTO photo_event_capture VALUES('',0) ON CONFLICT(id) DO NOTHING; END`}}
	for i, dep := range photoCaptureDependencies {
		for _, op := range []struct{ timing, action, row string }{{"AFTER", "INSERT", "NEW"}, {"BEFORE", "UPDATE", "OLD"}, {"AFTER", "UPDATE", "NEW"}, {"BEFORE", "DELETE", "OLD"}} {
			if op.action == "UPDATE" && dep.update == "-" {
				continue
			}
			when := " WHEN " + photoCaptureActive
			if op.action == "UPDATE" && dep.update != "" {
				when += " AND (" + dep.update + ")"
			}
			name := fmt.Sprintf("photo_event_%d_%s_%s", i, op.timing, op.action)
			triggers = append(triggers, photoCaptureTrigger{name, fmt.Sprintf(`CREATE TEMP TRIGGER %s %s %s ON main.%s%s BEGIN
 INSERT INTO photo_event_capture SELECT DISTINCT affected.id,COALESCE((SELECT hidden_at IS NOT NULL FROM photo_assets WHERE asset_id=affected.id),0) FROM (%s) affected WHERE affected.id IS NOT NULL ON CONFLICT(id) DO NOTHING;
END`, name, op.timing, op.action, dep.table, when, strings.ReplaceAll(dep.ids, "$row", op.row))})
		}
	}
	return triggers
})

// beginPhotoCapture arms capture for tx; setup failures fall back to a refetch instead of failing the write.
func (f *photoEventFeed) beginPhotoCapture(ctx context.Context, tx *sql.Tx) (bool, error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT photo_capture_setup`); err != nil {
		return false, err
	}
	setupErr := f.installPhotoCapture(ctx, tx)
	if setupErr == nil {
		_, setupErr = tx.ExecContext(ctx, `DELETE FROM photo_event_capture; INSERT OR REPLACE INTO photo_event_capture_active VALUES(1)`)
	}
	if setupErr != nil {
		if _, err := tx.ExecContext(ctx, `ROLLBACK TO photo_capture_setup`); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `RELEASE photo_capture_setup`); err != nil {
		return false, err
	}
	return setupErr == nil, nil
}

// finishPhotoCapture disarms capture before commit. When disarming fails, the
// caller must discard the connection so its armed triggers cannot run again.
func finishPhotoCapture(ctx context.Context, tx *sql.Tx) (events []PhotoEvent, disarmed bool) {
	events, err := capturedPhotoEvents(ctx, tx)
	if err != nil {
		events = []PhotoEvent{{}}
	}
	_, err = tx.ExecContext(context.WithoutCancel(ctx), `DELETE FROM photo_event_capture_active`)
	return events, err == nil
}

// installPhotoCapture installs the triggers on the writer connection once the first subscriber arrives.
func (f *photoEventFeed) installPhotoCapture(ctx context.Context, tx *sql.Tx) error {
	var installed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_temp_master WHERE name='photo_event_capture')`).Scan(&installed); err != nil {
		return err
	}
	if installed {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE photo_event_capture (id TEXT PRIMARY KEY, hidden_only INTEGER NOT NULL);
CREATE TEMP TABLE photo_event_capture_active (active INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	f.installed.Store(true)
	for _, trigger := range photoCaptureTriggers() {
		if _, err := tx.ExecContext(ctx, trigger.create); err != nil {
			return fmt.Errorf("installing %s: %w", trigger.name, err)
		}
	}
	return nil
}

// dropPhotoCapture removes the triggers from the writer connection once the last subscriber has left,
// so imports return to full speed. It reports whether nothing remains installed.
func (f *photoEventFeed) dropPhotoCapture(ctx context.Context, tx *sql.Tx) bool {
	var drop strings.Builder
	for _, trigger := range photoCaptureTriggers() {
		fmt.Fprintf(&drop, "DROP TRIGGER IF EXISTS temp.%s;", trigger.name)
	}
	drop.WriteString("DROP TABLE IF EXISTS temp.photo_event_capture; DROP TABLE IF EXISTS temp.photo_event_capture_active")
	_, err := tx.ExecContext(ctx, drop.String())
	return err == nil
}

func capturedPhotoEvents(ctx context.Context, tx *sql.Tx) (events []PhotoEvent, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,hidden_only FROM photo_event_capture`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	batch := photoEventBatch{}
	for rows.Next() {
		var event PhotoEvent
		if err := rows.Scan(&event.AssetID, &event.hiddenOnly); err != nil {
			return nil, err
		}
		batch.add(event)
	}
	return batch.events(), rows.Err()
}

func (s *Store) VisiblePhotoEvents(ctx context.Context, events []PhotoEvent) ([]PhotoEvent, error) {
	if len(events) == 0 {
		return nil, nil
	}
	visible := make([]PhotoEvent, 0, len(events))
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		var ids []string
		for _, event := range events {
			if event.AssetID != "" {
				ids = append(ids, event.AssetID)
			}
		}
		hidden := make(map[string]bool, len(ids))
		if len(ids) > 0 {
			err := queryRows(ctx, tx, `SELECT asset_id,hidden_at IS NOT NULL FROM photo_assets WHERE asset_id IN (`+placeholders(len(ids))+`)`, anyArgs(ids), func(rows *sql.Rows) error {
				var id string
				var value bool
				if err := rows.Scan(&id, &value); err != nil {
					return err
				}
				hidden[id] = value
				return nil
			})
			if err != nil {
				return fmt.Errorf("reading photo event visibility: %w", err)
			}
		}
		checked, unlocked := false, false
		for _, event := range events {
			currentHidden, exists := hidden[event.AssetID]
			if currentHidden || !exists && event.hiddenOnly {
				if !checked {
					_, err := s.hiddenSession(ctx, tx)
					if err != nil && !errors.Is(err, ErrHiddenLocked) {
						return err
					}
					checked, unlocked = true, err == nil
				}
				if !unlocked {
					continue
				}
			}
			if event.AssetID != "" && !exists {
				visible = []PhotoEvent{{}}
				return nil
			}
			event.hiddenOnly = false
			visible = append(visible, event)
		}
		return nil
	})
	return visible, err
}
