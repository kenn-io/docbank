package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// photoUnwatchedTables lists the main-schema tables whose writes no Photos read
// touches, grouped by reason. Every other table signals: the rule is what Photos
// reads, not who writes it, so document builds that move search or duplicate
// state still count. A table missing here costs one extra refresh, never a stale view.
var photoUnwatchedTables = map[string][]string{
	"Photos never reads these job queues and attempt logs": {
		"text_extraction_queue", "document_event_dirty", "document_event_attempts", "embedding_jobs",
		"vector_index_build_jobs", "mailbox_jobs", "export_jobs", "package_import_jobs", "page_render_jobs",
	},
	"Photos never reads these leases": {"vector_index_reader_leases"},
	"Photos never reads these storage operations": {
		"storage_operations", "storage_operation_stores", "storage_operation_cleanup",
	},
}

// photoRenditionJobsUpdate keeps lease renewals and claims from signalling; Photos shows only these columns.
const photoRenditionJobsUpdate = "OLD.state IS NOT NEW.state OR OLD.phase IS NOT NEW.phase OR OLD.failure_code IS NOT NEW.failure_code"

// The triggers use UPSERT because an outer statement's conflict policy, such as OR REPLACE, overrides OR IGNORE in a trigger body.
const photoTriggerPrefix = "photo_dirty_"

type photoEventFeed struct {
	sync.Mutex

	closed      bool
	subscribers map[chan struct{}]struct{}
	installed   atomic.Bool // The writer connection holds capture triggers.
}

// SubscribePhotoEvents delivers one signal per committed change to data Photos reads, until the caller unsubscribes or the store closes.
// Signals that go unread merge into one.
func (s *Store) SubscribePhotoEvents() (<-chan struct{}, func()) {
	f := &s.photoEvents
	f.Lock()
	defer f.Unlock()
	ch := make(chan struct{}, 1)
	if f.closed {
		close(ch)
		return ch, func() {}
	}
	if f.subscribers == nil {
		f.subscribers = make(map[chan struct{}]struct{})
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

func (s *Store) publishPhotoChange() {
	f := &s.photoEvents
	f.Lock()
	defer f.Unlock()
	for ch := range f.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// beginPhotoCapture arms capture for tx; setup failures fall back to a refetch instead of failing the write.
func (f *photoEventFeed) beginPhotoCapture(ctx context.Context, tx *sql.Tx) (bool, error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT photo_capture_setup`); err != nil {
		return false, err
	}
	setupErr := f.installPhotoCapture(ctx, tx)
	if setupErr == nil {
		_, setupErr = tx.ExecContext(ctx, `DELETE FROM photo_dirty`)
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

// photoChanged reports whether tx wrote anything Photos reads; a failed read counts as a change.
func photoChanged(ctx context.Context, tx *sql.Tx) bool {
	var dirty bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_dirty)`).Scan(&dirty); err != nil {
		return true
	}
	return dirty
}

// installPhotoCapture installs the triggers on the writer connection once the first subscriber arrives.
func (f *photoEventFeed) installPhotoCapture(ctx context.Context, tx *sql.Tx) error {
	var installed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_temp_master WHERE name='photo_dirty')`).Scan(&installed); err != nil {
		return err
	}
	if installed {
		return nil
	}
	tables, err := photoWatchedTables(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE photo_dirty (changed INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	f.installed.Store(true)
	for _, table := range tables {
		for _, op := range []struct{ suffix, action string }{{"ins", "INSERT"}, {"upd", "UPDATE"}, {"del", "DELETE"}} {
			when := ""
			if table == "rendition_jobs" && op.action == "UPDATE" {
				when = " WHEN " + photoRenditionJobsUpdate
			}
			name := photoTriggerPrefix + table + "_" + op.suffix
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`CREATE TEMP TRIGGER "%s" AFTER %s ON main."%s"%s BEGIN INSERT INTO photo_dirty VALUES(1) ON CONFLICT DO NOTHING; END`, name, op.action, table, when)); err != nil {
				return fmt.Errorf("installing %s: %w", name, err)
			}
		}
	}
	return nil
}

// photoWatchedTables lists the real main tables that carry triggers: no virtual tables, their shadows, or unwatched tables.
func photoWatchedTables(ctx context.Context, tx *sql.Tx) ([]string, error) {
	unwatched := map[string]bool{}
	for _, tables := range photoUnwatchedTables {
		for _, table := range tables {
			unwatched[table] = true
		}
	}
	names, err := photoNames(ctx, tx, `SELECT name FROM pragma_table_list WHERE schema='main' AND type='table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing photo tables: %w", err)
	}
	watched := names[:0]
	for _, name := range names {
		if !unwatched[name] {
			watched = append(watched, name)
		}
	}
	return watched, nil
}

func photoNames(ctx context.Context, tx *sql.Tx, query string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// dropPhotoCapture removes the triggers from the writer connection once the last subscriber has left,
// so imports return to full speed. It reports whether nothing remains installed.
func (f *photoEventFeed) dropPhotoCapture(ctx context.Context, tx *sql.Tx) bool {
	triggers, err := photoNames(ctx, tx, `SELECT name FROM sqlite_temp_master WHERE type='trigger' AND name LIKE 'photo\_dirty\_%' ESCAPE '\'`)
	if err != nil {
		return false
	}
	var drop strings.Builder
	for _, name := range triggers {
		fmt.Fprintf(&drop, `DROP TRIGGER IF EXISTS temp."%s";`, name)
	}
	drop.WriteString("DROP TABLE IF EXISTS temp.photo_dirty")
	_, err = tx.ExecContext(ctx, drop.String())
	return err == nil
}
