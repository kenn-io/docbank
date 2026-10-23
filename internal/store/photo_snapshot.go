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

func rankedPhotoSnapshotPopulation(ranking *PhotoRanking) (compiledQueryFragment, error) {
	encoded, err := json.Marshal(ranking.Members)
	if err != nil {
		return compiledQueryFragment{}, err
	}
	return compiledQueryFragment{sql: `SELECT n.id node_id,v.version_id content_version_id,a.asset_id,CAST(rank.key AS INTEGER)+1 photo_position FROM json_each(?) rank JOIN photo_assets a ON a.asset_id=json_extract(rank.value,'$.asset_id') ` + strings.TrimPrefix(photoBrowseDisplayFrom, `photo_assets a`) + ` WHERE n.id=json_extract(rank.value,'$.node_id') AND v.version_id=json_extract(rank.value,'$.content_version_id') AND ` + photoBrowseLiveDisplay + ` AND ` + photoVisibilityPredicate(false) + ` ORDER BY CAST(rank.key AS INTEGER)`, args: []any{string(encoded)}}, nil
}

// CapturePhotoSnapshot gives Cull the complete rows from the accepted ranked set.
func (s *QuerySnapshotService) CapturePhotoSnapshot(ctx context.Context, owner, id string, request SnapshotRequest) (SnapshotPage, error) {
	first, err := s.FirstPage(ctx, owner, id)
	if err != nil {
		return SnapshotPage{}, err
	}
	if !first.Photos {
		return SnapshotPage{}, ErrInvalidSnapshotRequest
	}
	rows, err := s.copyPhotoSnapshotRows(ctx, owner, id)
	if err != nil {
		return SnapshotPage{}, err
	}
	ranking := PhotoRanking{RequestedMode: first.Query.Mode, ActualMode: queryModeLexical}
	if first.PhotoRanking != nil {
		ranking = *first.PhotoRanking
	}
	ranking.Members = snapshotMembers(rows)
	request.photoCaptureRows = rows
	request.Query, request.Photos, request.PhotoRanking = first.Query, true, &ranking
	request.Coverage = first.Coverage
	request.PhotoProjection = "cull"
	request.GroupPhotoStacks = false
	return s.Create(ctx, owner, request)
}

func materializePhotoCapture(ctx context.Context, q metadataQuerier, rows []SnapshotRow, options snapshotMaterializeOptions, projection *SnapshotProjection) error {
	live, err := options.PhotoStore.currentPhotoRowsTx(ctx, q, snapshotMembers(rows), nil)
	if err == nil && int64(len(rows)) > options.MaxRows {
		err = ErrQuerySnapshotTooLarge
	} else if err == nil {
		err = chargeSnapshotMaterialization(options, &projection.SerializedBytes, 0, 2)
	}
	if err != nil {
		return err
	}
	for i, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		row.Flag = new(live[i].Flag)
		encoded, err := json.Marshal(row)
		if err == nil && int64(len(encoded)) > options.MaxRowBytes {
			err = ErrQuerySnapshotTooLarge
		} else if err == nil {
			err = chargeSnapshotMaterialization(options, &projection.SerializedBytes, 1, int64(len(encoded))+1)
		}
		if err != nil {
			return err
		}
		projection.Rows = append(projection.Rows, row)
		projection.TotalBytes += row.Size
	}
	projection.Total = int64(len(rows))
	return nil
}

func (s *QuerySnapshotService) copyPhotoSnapshotRows(ctx context.Context, owner, id string) ([]SnapshotRow, error) {
	cached, err := s.lookup(owner, id)
	if err != nil {
		return nil, err
	}
	rows := make([]SnapshotRow, len(cached.rows))
	for i, encoded := range cached.rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(encoded, &rows[i]); err != nil {
			return nil, err
		}
	}
	return rows, nil
}
