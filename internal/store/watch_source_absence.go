package store

import (
	"context"
	"database/sql"
	"path"
	"slices"
	"time"
)

// Push-owned documents remain applicable even if an old watch cursor is marked.
const sourcePresentSQL = `NOT EXISTS (
 SELECT 1 FROM watch_sources ws JOIN watch_source_absences a
 ON a.watch_name=ws.watch_name AND a.source_ref=ws.source_ref
 WHERE ws.node_id=n.id
 AND NOT EXISTS (SELECT 1 FROM push_sources ps WHERE ps.node_id=n.id))`

// WatchPresenceScan contains evidence from one complete daemon-watch inventory.
// ListedDirs uses the empty string for the watch root. A source is gone when
// its deepest listed ancestor lacks it: a listed parent has no regular file at
// the ref, or a listed directory has no entry of any type on the ref's path.
// Unsettled references, including skipped subtrees, retain their prior marks
// and those of their descendants, overriding regular-file evidence from before
// a failed recheck.
type WatchPresenceScan struct {
	RegularFiles []string
	ListedDirs   []string
	// ListedEntries names every entry of any type in ListedDirs.
	ListedEntries []string
	Unsettled     []string
	// Excluded matches durable refs and ancestors whose marks must remain unchanged.
	Excluded func(sourceRef string) bool
}

type watchPresenceEvidence struct {
	present, listed, entries, unsettled map[string]struct{}
	excluded                            func(string) bool
}

func newWatchPresenceEvidence(scan WatchPresenceScan) watchPresenceEvidence {
	return watchPresenceEvidence{
		present: stringSet(scan.RegularFiles), listed: stringSet(scan.ListedDirs),
		entries: stringSet(scan.ListedEntries), unsettled: stringSet(scan.Unsettled),
		excluded: scan.Excluded,
	}
}

// state reports whether the scan proves ref present or gone. Neither leaves
// the prior mark unchanged.
func (e watchPresenceEvidence) state(ref string) (present, gone bool) {
	for entry := ref; ; {
		if _, held := e.unsettled[entry]; held || (e.excluded != nil && e.excluded(entry)) {
			return false, false
		}
		if entry == ref {
			if _, present := e.present[ref]; present {
				return true, false
			}
		}
		parent := path.Dir(entry)
		if parent == "." {
			parent = ""
		}
		if _, listed := e.listed[parent]; listed {
			// An unlisted entry, such as a symlinked directory, holds its descendants.
			_, exists := e.entries[entry]
			return false, entry == ref || !exists
		}
		if parent == "" {
			return false, false
		}
		entry = parent
	}
}

// watchPresenceChanges returns marks the scan clears and records.
func watchPresenceChanges(ctx context.Context, q rowsQuerier, watchName string, scan WatchPresenceScan) (cleared, recorded []string, err error) {
	rows, err := q.QueryContext(ctx, `SELECT ws.source_ref, a.source_ref IS NOT NULL
 FROM watch_sources ws LEFT JOIN watch_source_absences a
 ON a.watch_name=ws.watch_name AND a.source_ref=ws.source_ref
 WHERE ws.watch_name=?`, watchName)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	evidence := newWatchPresenceEvidence(scan)
	for rows.Next() {
		var ref string
		var marked bool
		if err := rows.Scan(&ref, &marked); err != nil {
			return nil, nil, err
		}
		present, gone := evidence.state(ref)
		if marked && present {
			cleared = append(cleared, ref)
		} else if !marked && gone {
			recorded = append(recorded, ref)
		}
	}
	return cleared, recorded, rows.Err()
}

// WatchSourcePresenceChanged reports, without writing, whether
// ReconcileWatchSourcePresence would change any mark for this scan.
func (s *Store) WatchSourcePresenceChanged(ctx context.Context, watchName string, scan WatchPresenceScan) (bool, error) {
	cleared, recorded, err := watchPresenceChanges(ctx, s.db, watchName, scan)
	return len(cleared)+len(recorded) != 0, err
}

// ReconcileWatchSourcePresence records first-observed absence without changing
// archived documents or metadata. Callers must supply a complete inventory.
func (s *Store) ReconcileWatchSourcePresence(ctx context.Context, watchName string, scan WatchPresenceScan, now time.Time) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		cleared, recorded, err := watchPresenceChanges(ctx, tx, watchName, scan)
		if err != nil {
			return err
		}
		for _, ref := range cleared {
			if _, err := tx.ExecContext(ctx, `DELETE FROM watch_source_absences WHERE watch_name=? AND source_ref=?`, watchName, ref); err != nil {
				return err
			}
		}
		for _, ref := range recorded {
			if _, err := tx.ExecContext(ctx, `INSERT INTO watch_source_absences(watch_name,source_ref,absent_since) VALUES(?,?,?)`, watchName, ref, now.UTC().Format(time.RFC3339)); err != nil {
				return err
			}
		}
		return nil
	})
}

func watchPresenceStrings(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// DeleteWatchSourceAbsencesExcept clears observations belonging to watches
// no longer configured, before the daemon starts any watch jobs.
func (s *Store) DeleteWatchSourceAbsencesExcept(ctx context.Context, configured []string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT watch_name FROM watch_source_absences`)
		if err != nil {
			return err
		}
		names, err := watchPresenceStrings(rows)
		if err != nil {
			return err
		}
		for _, name := range names {
			if !slices.Contains(configured, name) {
				if _, err := tx.ExecContext(ctx, `DELETE FROM watch_source_absences WHERE watch_name=?`, name); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
