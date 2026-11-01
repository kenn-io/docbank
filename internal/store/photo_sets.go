package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/query"
)

const photoSelectionChunkSize = 200

// ErrInvalidPhotoAlbum reports an invalid album name, cover, or member selection.
var ErrInvalidPhotoAlbum = errors.New("invalid photo album")

// PhotoSet is an album. Its membership is independent of photo visibility.
type PhotoSet struct {
	ID           string  `json:"id" db:"set_id"`
	Name         string  `json:"name" db:"name"`
	Starred      bool    `json:"starred" db:"starred"`
	Revision     int64   `json:"revision" db:"revision"`
	CoverAssetID *string `json:"cover_asset_id,omitzero" db:"cover_asset_id"`
	CreatedAt    string  `json:"created_at" db:"created_at"`
	UpdatedAt    string  `json:"updated_at" db:"updated_at"`
	DeletedAt    *string `json:"deleted_at,omitzero" db:"deleted_at"`
}

// PhotoSetSummary adds member counts and the effective ready cover to an album.
type PhotoSetSummary struct {
	PhotoSet

	MemberCount           int64   `json:"member_count"`
	IncludedCount         int64   `json:"included_count"`
	EffectiveCoverAssetID *string `json:"effective_cover_asset_id,omitzero"`
	CoverRotation         int     `json:"cover_rotation"`
	CoverGenerationID     *string `json:"cover_generation_id,omitzero"`
}

// PhotoSetSelection supplies either explicit IDs or the complete browse population.
type PhotoSetSelection struct {
	AssetIDs        []string
	Query           *query.Query
	Coverage        CoverageSelection
	Hidden          bool
	Limit           int
	SkipUnavailable bool
}

func validPhotoSetName(name string) bool {
	return strings.TrimSpace(name) != "" && utf8.ValidString(name) && utf8.RuneCountInString(name) <= 256 && !strings.ContainsRune(name, 0)
}

const photoSetColumns = `set_id,name,starred,revision,cover_asset_id,created_at,updated_at,deleted_at`

func scanPhotoSet(row interface{ Scan(dest ...any) error }) (PhotoSet, error) {
	var set PhotoSet
	err := row.Scan(&set.ID, &set.Name, &set.Starred, &set.Revision, &set.CoverAssetID, &set.CreatedAt, &set.UpdatedAt, &set.DeletedAt)
	return set, err
}

func photoSetByID(ctx context.Context, q metadataQuerier, id string) (PhotoSet, error) {
	if validateUUIDv4(id) != nil {
		return PhotoSet{}, ErrNotFound
	}
	set, err := scanPhotoSet(q.QueryRowContext(ctx, `SELECT `+photoSetColumns+` FROM photo_sets WHERE set_id=? AND deleted_at IS NULL`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoSet{}, ErrNotFound
	}
	return set, err
}

func (s *Store) photoSetSummary(ctx context.Context, q metadataQuerier, set PhotoSet, recipe string) (PhotoSetSummary, error) {
	visible, err := s.photoSetResponse(ctx, q, set)
	if err != nil {
		return PhotoSetSummary{}, err
	}
	out := PhotoSetSummary{PhotoSet: visible}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(n.id)
 FROM photo_set_members m
 JOIN photo_assets a ON a.asset_id=m.asset_id
 LEFT JOIN photo_files f ON f.file_id=a.display_file_id
 AND a.excluded_at IS NULL AND `+photoVisibilityPredicate(false)+`
 LEFT JOIN nodes n ON n.id=f.node_id AND `+photoBrowseLiveDisplay+`
 WHERE m.set_id=? AND (a.display_file_id IS NOT NULL OR
 EXISTS (SELECT 1 FROM photo_files member WHERE member.asset_id=m.asset_id))`,
		set.ID).Scan(&out.MemberCount, &out.IncludedCount); err != nil {
		return out, err
	}
	if out.IncludedCount == 0 {
		return out, nil
	}
	var id, generation string
	coverSQL := `SELECT a.asset_id,g.generation_id,f.rotation FROM photo_set_members m
 CROSS JOIN photo_assets a ON a.asset_id=m.asset_id
 CROSS JOIN photo_files f ON f.file_id=a.display_file_id
 CROSS JOIN nodes n ON n.id=f.node_id
 CROSS JOIN content_versions v ON v.version_id=n.current_version_id
 CROSS JOIN visual_preview_generations g ON g.content_version_id=v.version_id AND g.source_sha256=v.blob_hash
 WHERE m.set_id=? AND ` + photoBrowseLiveDisplay + ` AND ` + photoVisibilityPredicate(false) + ` AND g.recipe_fingerprint=? AND g.state='ready' AND g.output_blob_hash IS NOT NULL`
	if set.CoverAssetID != nil {
		err := q.QueryRowContext(ctx, coverSQL+` AND m.asset_id=?`, set.ID, recipe, *set.CoverAssetID).Scan(&id, &generation, &out.CoverRotation)
		if err == nil {
			out.EffectiveCoverAssetID, out.CoverGenerationID = &id, &generation
			return out, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
	}
	err = q.QueryRowContext(ctx, coverSQL+` ORDER BY m.added_at DESC,m.asset_id ASC LIMIT 1`, set.ID, recipe).Scan(&id, &generation, &out.CoverRotation)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err == nil {
		out.EffectiveCoverAssetID = &id
		out.CoverGenerationID = &generation
	}
	return out, err
}

// PhotoSet returns one live album summary using the given grid preview recipe.
func (s *Store) PhotoSet(ctx context.Context, id, recipe string) (PhotoSetSummary, error) {
	var out PhotoSetSummary
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		set, err := photoSetByID(ctx, tx, id)
		if err != nil {
			return err
		}
		out, err = s.photoSetSummary(ctx, tx, set, recipe)
		return err
	})
	return out, err
}

// ListPhotoSets returns live albums, starred first, then by name.
func (s *Store) ListPhotoSets(ctx context.Context, recipe string) ([]PhotoSetSummary, error) {
	out := make([]PhotoSetSummary, 0)
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+photoSetColumns+` FROM photo_sets WHERE deleted_at IS NULL ORDER BY starred DESC,name,set_id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var sets []PhotoSet
		for rows.Next() {
			set, err := scanPhotoSet(rows)
			if err != nil {
				return err
			}
			sets = append(sets, set)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		for _, set := range sets {
			summary, err := s.photoSetSummary(ctx, tx, set, recipe)
			if err != nil {
				return err
			}
			out = append(out, summary)
		}
		return nil
	})
	return out, err
}

func insertPhotoSet(ctx context.Context, tx *sql.Tx, name string) (PhotoSet, error) {
	if !validPhotoSetName(name) {
		return PhotoSet{}, ErrInvalidPhotoAlbum
	}
	id, err := newUUIDv4()
	if err != nil {
		return PhotoSet{}, err
	}
	now := nowRFC3339()
	set := PhotoSet{ID: id, Name: name, Revision: 1, CreatedAt: now, UpdatedAt: now}
	_, err = tx.ExecContext(ctx, `INSERT INTO photo_sets(set_id,name,revision,created_at,updated_at) VALUES(?,?,1,?,?)`, id, name, now, now)
	return set, err
}

func writePhotoSetReceipts(ctx context.Context, tx *sql.Tx, operation string, before, after PhotoSet, ids []string) error {
	for offset := 0; ; offset += 256 {
		end := min(offset+256, len(ids))
		state := func(set PhotoSet) any {
			return struct {
				Set      PhotoSet `json:"set"`
				AssetIDs []string `json:"asset_ids"`
			}{set, ids[offset:end]}
		}
		if err := writePhotoReceiptTx(ctx, tx, photoReceipt{Operation: operation, SetID: after.ID, BeforeRevision: before.Revision, AfterRevision: after.Revision, Before: state(before), After: state(after)}); err != nil {
			return err
		}
		if end == len(ids) {
			return nil
		}
	}
}

// CreatePhotoSet creates an empty album at revision 1.
func (s *Store) CreatePhotoSet(ctx context.Context, name string) (PhotoSet, error) {
	var out PhotoSet
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = insertPhotoSet(ctx, tx, name)
		if err != nil {
			return err
		}
		return writePhotoSetReceipts(ctx, tx, "set_create", PhotoSet{}, out, nil)
	})
	return out, err
}

func photoSetForMutation(ctx context.Context, tx *sql.Tx, id string, revision int64) (PhotoSet, error) {
	set, err := photoSetByID(ctx, tx, id)
	if err != nil {
		return set, err
	}
	if revision < 1 || set.Revision != revision {
		return PhotoSet{}, ErrStaleRevision
	}
	return set, nil
}

func commitPhotoSet(ctx context.Context, tx *sql.Tx, before, after PhotoSet, operation string, ids []string) (PhotoSet, error) {
	after.Revision++
	after.UpdatedAt = nowRFC3339()
	if _, err := tx.ExecContext(ctx, `UPDATE photo_sets SET name=?,starred=?,revision=?,cover_asset_id=?,updated_at=?,deleted_at=? WHERE set_id=?`, after.Name, after.Starred, after.Revision, nullablePhotoString(after.CoverAssetID), after.UpdatedAt, nullablePhotoString(after.DeletedAt), after.ID); err != nil {
		return PhotoSet{}, err
	}
	return after, writePhotoSetReceipts(ctx, tx, operation, before, after, ids)
}

// UpdatePhotoSet applies one album property decision at the expected revision.
func (s *Store) UpdatePhotoSet(ctx context.Context, id string, revision int64, name *string, starred *bool, cover **string) (PhotoSet, error) {
	if name != nil && !validPhotoSetName(*name) {
		return PhotoSet{}, ErrInvalidPhotoAlbum
	}
	var out PhotoSet
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		before, err := photoSetForMutation(ctx, tx, id, revision)
		if err != nil {
			return err
		}
		out = before
		if name != nil {
			out.Name = *name
		}
		if starred != nil {
			out.Starred = *starred
		}
		if cover != nil {
			if *cover != nil {
				var hidden, member bool
				if err := tx.QueryRowContext(ctx, `SELECT hidden_at IS NOT NULL, EXISTS(SELECT 1 FROM photo_set_members WHERE set_id=? AND asset_id=?) FROM photo_assets WHERE asset_id=?`, id, **cover, **cover).Scan(&hidden, &member); err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return ErrInvalidPhotoAlbum
					}
					return err
				}
				if hidden {
					if _, err := s.hiddenSession(ctx, tx); err != nil {
						return err
					}
				}
				if !member {
					return ErrInvalidPhotoAlbum
				}
			}
			out.CoverAssetID = *cover
		}
		if out.Name != before.Name || out.Starred != before.Starred || !equalPhotoOptional(out.CoverAssetID, before.CoverAssetID) {
			out, err = commitPhotoSet(ctx, tx, before, out, "set_update", nil)
			if err != nil {
				return err
			}
		}
		out, err = s.photoSetResponse(ctx, tx, out)
		return err
	})
	return out, err
}

// DeletePhotoSet clears an album's members and cover and marks it deleted.
func (s *Store) DeletePhotoSet(ctx context.Context, id string, revision int64) (PhotoSet, error) {
	var out PhotoSet
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		before, err := photoSetForMutation(ctx, tx, id, revision)
		if err != nil {
			return err
		}
		ids, err := photoSetMemberIDs(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM photo_set_members WHERE set_id=?`, id); err != nil {
			return err
		}
		out = before
		out.CoverAssetID = nil
		out.DeletedAt = new(nowRFC3339())
		out, err = commitPhotoSet(ctx, tx, before, out, "set_delete", ids)
		return err
	})
	return out, err
}

func photoSetMemberIDs(ctx context.Context, q metadataQuerier, id string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT asset_id FROM photo_set_members WHERE set_id=? ORDER BY added_at,asset_id`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DuplicatePhotoSet copies an album's star, cover, members, and added dates.
func (s *Store) DuplicatePhotoSet(ctx context.Context, id string, revision int64, name string) (PhotoSet, error) {
	var out PhotoSet
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		before, err := photoSetForMutation(ctx, tx, id, revision)
		if err != nil {
			return err
		}
		out, err = insertPhotoSet(ctx, tx, name)
		if err != nil {
			return err
		}
		out.Starred = before.Starred
		out.CoverAssetID = before.CoverAssetID
		if _, err := tx.ExecContext(ctx, `UPDATE photo_sets SET starred=?,cover_asset_id=? WHERE set_id=?`, out.Starred, nullablePhotoString(out.CoverAssetID), out.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO photo_set_members(set_id,asset_id,added_at) SELECT ?,asset_id,added_at FROM photo_set_members WHERE set_id=?`, out.ID, id); err != nil {
			return err
		}
		ids, err := photoSetMemberIDs(ctx, tx, out.ID)
		if err != nil {
			return err
		}
		if err := writePhotoSetReceipts(ctx, tx, "set_duplicate", PhotoSet{}, out, ids); err != nil {
			return err
		}
		out, err = s.photoSetResponse(ctx, tx, out)
		return err
	})
	return out, err
}

var errPhotoSetSelector = fmt.Errorf("provide either asset_ids or query: %w", ErrInvalidPhotoAlbum)

// errPhotoSetTooManyAssets reports more explicit asset IDs than one selection accepts.
// Callers map it to their own public limit error.
var errPhotoSetTooManyAssets = fmt.Errorf("select at most %d asset IDs", maxBatchTagTargets)

func (s *Store) photoSetSelectionIDs(ctx context.Context, tx *sql.Tx, selection PhotoSetSelection) ([]string, error) {
	if selection.Hidden {
		if _, err := s.hiddenSession(ctx, tx); err != nil {
			return nil, err
		}
	}
	if len(selection.AssetIDs) > maxBatchTagTargets {
		return nil, errPhotoSetTooManyAssets
	}
	if selection.SkipUnavailable && selection.Query != nil {
		return nil, errPhotoSetSelector
	}
	if (selection.Query == nil) == (len(selection.AssetIDs) == 0) {
		return nil, errPhotoSetSelector
	}
	if selection.Query == nil {
		return s.photoSetExplicitIDs(ctx, tx, selection)
	}
	return s.photoSetQueryIDs(ctx, tx, selection)
}

func (s *Store) photoSetExplicitIDs(ctx context.Context, tx *sql.Tx, selection PhotoSetSelection) ([]string, error) {
	if selection.Coverage.Configuration != "" || selection.Coverage.ProfileFingerprint != "" {
		return nil, ErrInvalidPhotoQuery
	}
	ids := make([]string, 0, len(selection.AssetIDs))
	seen := map[string]bool{}
	for _, id := range selection.AssetIDs {
		if validateUUIDv4(id) != nil {
			return nil, ErrInvalidPhotoAlbum
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	if selection.SkipUnavailable {
		value, err := query.Parse([]byte(`{}`))
		if err != nil {
			return nil, err
		}
		selection.AssetIDs = nil
		selection.Query = &value
		selection.SkipUnavailable = false
		eligible := make(map[string]bool, len(ids))
		for chunk := range slices.Chunk(ids, query.MaxIDValues) {
			value.Filters.AssetIDs = chunk
			current, err := s.photoSetSelectionIDs(ctx, tx, selection)
			if err != nil {
				return nil, err
			}
			for _, id := range current {
				eligible[id] = true
			}
		}
		return slices.DeleteFunc(ids, func(id string) bool { return !eligible[id] }), nil
	}
	for chunk := range slices.Chunk(ids, photoSelectionChunkSize) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		count, hidden, err := func() (int, bool, error) {
			rows, err := tx.QueryContext(ctx, `SELECT hidden_at FROM photo_assets WHERE asset_id IN (`+placeholders(len(chunk))+`)`, args...)
			if err != nil {
				return 0, false, err
			}
			defer func() { _ = rows.Close() }()
			count, hidden := 0, false
			for rows.Next() {
				var at sql.NullString
				if err := rows.Scan(&at); err != nil {
					return 0, false, err
				}
				count++
				hidden = hidden || at.Valid
			}
			return count, hidden, rows.Err()
		}()
		if err != nil {
			return nil, err
		}
		if count != len(chunk) {
			return nil, ErrNotFound
		}
		if hidden {
			if _, err := s.hiddenSession(ctx, tx); err != nil {
				return nil, err
			}
		}
	}
	return ids, nil
}

func (s *Store) photoSetQueryIDs(ctx context.Context, tx *sql.Tx, selection PhotoSetSelection) ([]string, error) {
	coverage, err := normalizeCoverageSelection(selection.Coverage)
	if err != nil {
		return nil, err
	}
	compiled, err := (queryCompiler{photoDisplayMetadata: true, photoHidden: selection.Hidden}).compile(ctx, *selection.Query, queryResolver{q: tx})
	if err != nil {
		return nil, err
	}
	generation, err := collectionGenerationTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	scope, err := newPhotoBrowseScope(ctx, tx, compiled, generation, coverage, selection.Hidden)
	if err != nil {
		return nil, err
	}
	selectionSQL := `SELECT a.asset_id FROM ` + photoBrowseDisplayFrom + ` WHERE ` + scope.where() + ` ORDER BY a.asset_id`
	if selection.Limit > 0 {
		selectionSQL += fmt.Sprintf(" LIMIT %d", selection.Limit+1)
	}
	sql, args, err := scope.bind(selectionSQL)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ChangePhotoSetMembers adds or removes the selected assets at the expected revision.
// A query selection resolves only visible photos, so hidden members stay
// until they are removed by ID.
func (s *Store) ChangePhotoSetMembers(ctx context.Context, id string, revision int64, add bool, selection PhotoSetSelection) (PhotoSet, error) {
	var out PhotoSet
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		before, err := photoSetForMutation(ctx, tx, id, revision)
		if err != nil {
			return err
		}
		out = before
		ids, err := s.photoSetSelectionIDs(ctx, tx, selection)
		if errors.Is(err, errPhotoSetTooManyAssets) {
			return ErrInvalidPhotoAlbum
		}
		if err != nil {
			return err
		}
		out, err = changePhotoSetMembersTx(ctx, tx, before, add, ids, nil)
		if err != nil {
			return err
		}
		out, err = s.photoSetResponse(ctx, tx, out)
		return err
	})
	return out, err
}

type photoSetMemberStamp struct {
	ID      string `json:"id"`
	AddedAt string `json:"added_at"`
}

func changePhotoSetMembersTx(ctx context.Context, tx *sql.Tx, before PhotoSet, add bool, ids []string, stamps []photoSetMemberStamp) (PhotoSet, error) {
	out := before
	encoded, err := json.Marshal(ids)
	if err != nil {
		return PhotoSet{}, err
	}
	condition := `EXISTS(SELECT 1 FROM photo_set_members m WHERE m.set_id=? AND m.asset_id=j.value)`
	if add {
		condition = `NOT ` + condition
	}
	rows, err := tx.QueryContext(ctx, `SELECT j.value FROM json_each(?) j WHERE `+condition+` GROUP BY j.value ORDER BY MIN(j.key)`, string(encoded), before.ID)
	if err != nil {
		return PhotoSet{}, err
	}
	defer func() { _ = rows.Close() }()
	var changed []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return PhotoSet{}, err
		}
		changed = append(changed, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return PhotoSet{}, err
	}
	if add {
		if len(stamps) != 0 {
			ordered, marshalErr := json.Marshal(stamps)
			if marshalErr != nil {
				return PhotoSet{}, marshalErr
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO photo_set_members(set_id,asset_id,added_at) SELECT ?,json_extract(member.value,'$.id'),json_extract(member.value,'$.added_at') FROM json_each(?) member WHERE 1 ON CONFLICT(set_id,asset_id) DO NOTHING`, before.ID, string(ordered))
		} else {
			// SQLite needs a WHERE before ON CONFLICT on INSERT ... SELECT.
			_, err = tx.ExecContext(ctx, `INSERT INTO photo_set_members(set_id,asset_id,added_at) SELECT ?,value,? FROM json_each(?) WHERE 1 ON CONFLICT(set_id,asset_id) DO NOTHING`, before.ID, nowRFC3339(), string(encoded))
		}
	} else {
		_, err = tx.ExecContext(ctx, `DELETE FROM photo_set_members WHERE set_id=? AND asset_id IN (SELECT value FROM json_each(?))`, before.ID, string(encoded))
		if out.CoverAssetID != nil && slices.Contains(changed, *out.CoverAssetID) {
			out.CoverAssetID = nil
		}
	}
	if err != nil {
		return PhotoSet{}, err
	}
	if len(changed) == 0 {
		return out, nil
	}
	operation := "set_remove"
	if add {
		operation = "set_add"
	}
	return commitPhotoSet(ctx, tx, before, out, operation, changed)
}

func (s *Store) photoSetResponse(ctx context.Context, q metadataQuerier, set PhotoSet) (PhotoSet, error) {
	if set.CoverAssetID == nil {
		return set, nil
	}
	hidden, err := photoAssetHiddenQuery(ctx, q, *set.CoverAssetID)
	if err != nil {
		return PhotoSet{}, err
	}
	if hidden {
		if _, err := s.hiddenSession(ctx, q); errors.Is(err, ErrHiddenLocked) {
			set.CoverAssetID = nil
		} else if err != nil {
			return PhotoSet{}, err
		}
	}
	return set, nil
}

func photoAssetHiddenQuery(ctx context.Context, q metadataQuerier, id string) (bool, error) {
	var hidden bool
	err := q.QueryRowContext(ctx, `SELECT hidden_at IS NOT NULL FROM photo_assets WHERE asset_id=?`, id).Scan(&hidden)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	return hidden, err
}
