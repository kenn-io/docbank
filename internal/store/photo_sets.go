package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/query"
)

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

type PhotoSetSummary struct {
	PhotoSet
	MemberCount           int64   `json:"member_count"`
	IncludedCount         int64   `json:"included_count"`
	EffectiveCoverAssetID *string `json:"effective_cover_asset_id,omitzero"`
	CoverGenerationID     *string `json:"cover_generation_id,omitzero"`
}

// PhotoSetSelection supplies either explicit IDs or the complete browse population.
type PhotoSetSelection struct {
	AssetIDs []string
	Query    *query.Query
	Coverage CoverageSelection
}

func validPhotoSetName(name string) bool {
	return strings.TrimSpace(name) != "" && utf8.ValidString(name) && utf8.RuneCountInString(name) <= 256 && !strings.ContainsRune(name, 0)
}

func photoSetByID(ctx context.Context, q metadataQuerier, id string) (PhotoSet, error) {
	if validateUUIDv4(id) != nil {
		return PhotoSet{}, ErrNotFound
	}
	var set PhotoSet
	err := q.QueryRowContext(ctx, `SELECT set_id,name,starred,revision,cover_asset_id,created_at,updated_at,deleted_at FROM photo_sets WHERE set_id=? AND deleted_at IS NULL`, id).Scan(&set.ID, &set.Name, &set.Starred, &set.Revision, &set.CoverAssetID, &set.CreatedAt, &set.UpdatedAt, &set.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoSet{}, ErrNotFound
	}
	return set, err
}

func photoSetSummary(ctx context.Context, q metadataQuerier, set PhotoSet, recipe string) (PhotoSetSummary, error) {
	out := PhotoSetSummary{PhotoSet: set}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_set_members WHERE set_id=?`, set.ID).Scan(&out.MemberCount); err != nil {
		return out, err
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_set_members m JOIN photo_assets a ON a.asset_id=m.asset_id JOIN photo_files f ON f.file_id=a.display_file_id JOIN nodes n ON n.id=f.node_id WHERE m.set_id=? AND `+photoBrowseLiveDisplay, set.ID).Scan(&out.IncludedCount); err != nil {
		return out, err
	}
	var id, generation string
	coverSQL := `SELECT a.asset_id,g.generation_id FROM photo_set_members m
 CROSS JOIN photo_assets a ON a.asset_id=m.asset_id
 CROSS JOIN photo_files f ON f.file_id=a.display_file_id
 CROSS JOIN nodes n ON n.id=f.node_id
 CROSS JOIN content_versions v ON v.version_id=n.current_version_id
 CROSS JOIN visual_preview_generations g ON g.content_version_id=v.version_id AND g.source_sha256=v.blob_hash
 WHERE m.set_id=? AND ` + photoBrowseLiveDisplay + ` AND g.recipe_fingerprint=? AND g.state='ready' AND g.output_blob_hash IS NOT NULL`
	if set.CoverAssetID != nil {
		err := q.QueryRowContext(ctx, coverSQL+` AND m.asset_id=?`, set.ID, recipe, *set.CoverAssetID).Scan(&id, &generation)
		if err == nil {
			out.EffectiveCoverAssetID, out.CoverGenerationID = &id, &generation
			return out, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
	}
	err := q.QueryRowContext(ctx, coverSQL+` ORDER BY m.added_at DESC,m.asset_id ASC LIMIT 1`, set.ID, recipe).Scan(&id, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err == nil {
		out.EffectiveCoverAssetID = &id
		out.CoverGenerationID = &generation
	}
	return out, err
}

func (s *Store) PhotoSet(ctx context.Context, id, recipe string) (PhotoSetSummary, error) {
	var out PhotoSetSummary
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		set, err := photoSetByID(ctx, tx, id)
		if err != nil {
			return err
		}
		out, err = photoSetSummary(ctx, tx, set, recipe)
		return err
	})
	return out, err
}

func (s *Store) ListPhotoSets(ctx context.Context, recipe string) ([]PhotoSetSummary, error) {
	out := make([]PhotoSetSummary, 0)
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT set_id FROM photo_sets WHERE deleted_at IS NULL ORDER BY starred DESC,name,set_id`)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		for _, id := range ids {
			set, err := photoSetByID(ctx, tx, id)
			if err != nil {
				return err
			}
			summary, err := photoSetSummary(ctx, tx, set, recipe)
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
				if validateUUIDv4(**cover) != nil {
					return ErrInvalidPhotoAlbum
				}
				var count int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_set_members WHERE set_id=? AND asset_id=?`, id, **cover).Scan(&count); err != nil {
					return err
				}
				if count != 1 {
					return ErrInvalidPhotoAlbum
				}
			}
			out.CoverAssetID = *cover
		}
		if out.Name == before.Name && out.Starred == before.Starred && equalPhotoString(out.CoverAssetID, before.CoverAssetID) {
			return nil
		}
		out, err = commitPhotoSet(ctx, tx, before, out, "set_update", nil)
		return err
	})
	return out, err
}

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
		return writePhotoSetReceipts(ctx, tx, "set_duplicate", PhotoSet{}, out, ids)
	})
	return out, err
}

func photoSetSelectionIDs(ctx context.Context, tx *sql.Tx, add bool, selection PhotoSetSelection) ([]string, error) {
	if (selection.Query == nil) == (len(selection.AssetIDs) == 0) || len(selection.AssetIDs) > maxBatchTagTargets {
		return nil, ErrInvalidPhotoAlbum
	}
	if selection.Query == nil {
		if selection.Coverage.Configuration != "" || selection.Coverage.ProfileFingerprint != "" {
			return nil, ErrInvalidPhotoQuery
		}
		ids := make([]string, 0, len(selection.AssetIDs))
		seen := map[string]bool{}
		for _, id := range selection.AssetIDs {
			if validateUUIDv4(id) != nil {
				return nil, ErrInvalidPhotoAlbum
			}
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_assets WHERE asset_id=?`, id).Scan(&count); err != nil {
				return nil, err
			}
			if count != 1 {
				return nil, ErrNotFound
			}
			if add {
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_files WHERE asset_id=?`, id).Scan(&count); err != nil {
					return nil, err
				}
				if count == 0 {
					return nil, ErrInvalidPhotoAlbum
				}
			}
			if !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
		return ids, nil
	}
	coverage, err := normalizeCoverageSelection(selection.Coverage)
	if err != nil {
		return nil, err
	}
	compiled, err := (queryCompiler{photoDisplayMetadata: true}).compile(ctx, *selection.Query, queryResolver{q: tx})
	if err != nil {
		return nil, err
	}
	if coverage.Configuration == photoBrowseConfiguredCoverage {
		if err := validateSnapshotCoverageProfile(ctx, tx, coverage); err != nil {
			return nil, err
		}
	}
	generation, err := collectionGenerationTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	match, err := photoBrowseMatch(compiled, generation, coverage)
	if err != nil {
		return nil, err
	}
	sql, args, err := bindQueryPopulation(compiledQueryFragment{sql: `SELECT a.asset_id FROM ` + photoBrowseDisplayFrom + ` WHERE ` + photoBrowseLiveDisplay + ` AND ` + match.sql + ` ORDER BY a.asset_id`, args: match.args, relations: match.relations}, coverage, generation)
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

func (s *Store) ChangePhotoSetMembers(ctx context.Context, id string, revision int64, add bool, selection PhotoSetSelection) (PhotoSet, error) {
	var out PhotoSet
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		before, err := photoSetForMutation(ctx, tx, id, revision)
		if err != nil {
			return err
		}
		out = before
		ids, err := photoSetSelectionIDs(ctx, tx, add, selection)
		if err != nil {
			return err
		}
		out, err = changePhotoSetMembersTx(ctx, tx, before, add, ids)
		return err
	})
	return out, err
}

func changePhotoSetMembersTx(ctx context.Context, tx *sql.Tx, before PhotoSet, add bool, ids []string) (PhotoSet, error) {
	out := before
	changed := make([]string, 0, len(ids))
	now := nowRFC3339()
	for _, asset := range ids {
		var result sql.Result
		var err error
		if add {
			result, err = tx.ExecContext(ctx, `INSERT INTO photo_set_members(set_id,asset_id,added_at) VALUES(?,?,?) ON CONFLICT(set_id,asset_id) DO NOTHING`, before.ID, asset, now)
		} else {
			result, err = tx.ExecContext(ctx, `DELETE FROM photo_set_members WHERE set_id=? AND asset_id=?`, before.ID, asset)
		}
		if err != nil {
			return PhotoSet{}, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return PhotoSet{}, err
		}
		if count > 0 {
			changed = append(changed, asset)
			if !add && out.CoverAssetID != nil && *out.CoverAssetID == asset {
				out.CoverAssetID = nil
			}
		}
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
