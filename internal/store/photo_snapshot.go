package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"
)

func photoSnapshotPopulation(compiled CompiledQuery, generation string, coverage CoverageSelection, limit int64, suggestions bool) (compiledQueryFragment, error) {
	if err := validatePhotoQuerySort(compiled.Query); err != nil {
		return compiledQueryFragment{}, err
	}
	if compiled.Query.Sort.Field == "relevance" {
		statement, args, err := rankedPhotoStatement(compiled, generation, coverage, int(limit+1), true, suggestions)
		return compiledQueryFragment{sql: statement, args: args}, err
	}
	key := photoBrowseMissingKey(compiled.Query.Sort.Field)
	from := photoBrowseDisplayFrom
	if compiled.Query.Sort.Field == "added_time" {
		from, key, _ = photoBrowseOrder("added_time")
	}
	if key == "" {
		return compiledQueryFragment{}, ErrInvalidPhotoQuery
	}
	match, err := photoBrowseMatch(compiled, generation, coverage)
	if err != nil {
		return compiledQueryFragment{}, err
	}
	if compiled.Query.Sort.Field == "added_time" {
		match = joinCompiledFragments([]compiledQueryFragment{match, {sql: `sm.set_id=?`, args: []any{compiled.Query.Filters.SetIDs[0]}}}, ` AND `)
	}
	direction := "ASC"
	if compiled.Query.Sort.Direction == "desc" {
		direction = "DESC"
	}
	order := `(` + key + `='') ASC,` + key + ` ` + direction + `,a.asset_id ASC`
	return compiledQueryFragment{sql: `SELECT n.id node_id,v.version_id content_version_id,a.asset_id,row_number() OVER (ORDER BY ` + order + `) photo_position FROM ` + from + ` WHERE ` + photoBrowseLiveDisplay + ` AND ` + photoVisibilityPredicate(compiled.photoHidden) + ` AND ` + match.sql + ` ORDER BY ` + order + ` LIMIT ?`, args: append(match.args, limit+1), relations: match.relations}, nil
}

// CreatePhotoAlbum copies a caller's complete frozen photo set in one transaction.
func (s *QuerySnapshotService) CreatePhotoAlbum(ctx context.Context, owner, snapshotID, memberHash, name string, mutate func(func() error) error) (PhotoSet, error) {
	cached, err := s.lookup(owner, snapshotID)
	if err != nil {
		return PhotoSet{}, err
	}
	var metadata SnapshotProjection
	if err := json.Unmarshal(cached.metadata, &metadata); err != nil {
		return PhotoSet{}, err
	}
	if !metadata.Photos {
		return PhotoSet{}, fmt.Errorf("%w: Documents snapshots cannot create photo albums; use a Photos snapshot", ErrInvalidPhotoAlbum)
	}
	members, err := s.CopyMembersBounded(ctx, owner, snapshotID, memberHash, int(querySnapshotMaxRows))
	if err != nil {
		return PhotoSet{}, err
	}
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.AssetID)
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return PhotoSet{}, err
	}
	var out PhotoSet
	write := func() error {
		return s.store.withLogicalTx(ctx, func(tx *sql.Tx) error {
			if err := photoSnapshotLibraryMembersTx(ctx, tx, encoded, len(ids)); err != nil {
				return err
			}
			var err error
			out, err = insertPhotoSet(ctx, tx, name)
			if err != nil {
				return err
			}
			if err = writePhotoSetReceipts(ctx, tx, "set_create", PhotoSet{}, out, nil); err != nil {
				return err
			}
			// Fixed-width timestamps keep the default Added sort in frozen result order.
			stamps := make([]photoSetMemberStamp, len(ids))
			now := s.now().UTC()
			for i, id := range ids {
				stamps[i].ID = id
				stamps[i].AddedAt = now.Add(-time.Duration(i) * time.Nanosecond).Format(timestampLayout)
			}
			out, err = changePhotoSetMembersTx(ctx, tx, out, true, ids, stamps)
			if err != nil {
				return err
			}
			_, err = s.lookup(owner, snapshotID)
			return err
		})
	}
	if mutate == nil {
		err = write()
	} else {
		err = mutate(write)
	}
	return out, err
}

func photoSnapshotLibraryMembersTx(ctx context.Context, tx rowQuerier, encoded []byte, count int) error {
	var eligible int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM json_each(?) frozen JOIN photo_assets a ON a.asset_id=frozen.value `+strings.TrimPrefix(photoBrowseDisplayFrom, `photo_assets a`)+` WHERE `+photoBrowseLiveDisplay+` AND `+photoVisibilityPredicate(false), string(encoded)).Scan(&eligible); err != nil {
		return err
	}
	if eligible != count {
		return ErrPhotoSnapshotMemberNotInLibrary
	}
	return nil
}
