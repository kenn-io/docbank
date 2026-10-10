package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/query"
)

const MaxPhotoRejectsMove = 1000
const MaxPhotoRejectsMixed = 20

type PhotoRejectsRequest struct {
	Query  query.Query
	Hidden bool
}

type PhotoRejectMember struct {
	FileID  string `json:"file_id"`
	Name    string `json:"name"`
	Flag    string `json:"flag"`
	InTrash bool   `json:"in_trash"`
}

type PhotoRejectMixed struct {
	AssetID string              `json:"asset_id"`
	Members []PhotoRejectMember `json:"members"`
}

type PhotoRejectsPreflight struct {
	Targets    []PhotoRejectTarget `json:"targets"`
	Photos     int                 `json:"photos"`
	Movable    int                 `json:"movable"`
	Files      int                 `json:"files"`
	Unchanged  int                 `json:"unchanged"`
	Mixed      []PhotoRejectMixed  `json:"mixed"`
	MixedCount int                 `json:"mixed_count"`
}

// PreflightPhotoRejects evaluates the complete scope in one read snapshot.
func (s *Store) PreflightPhotoRejects(ctx context.Context, request PhotoRejectsRequest) (PhotoRejectsPreflight, error) {
	var out PhotoRejectsPreflight
	err := s.withLexicalGenerationRead(ctx, func(q metadataQuerier, generation LexicalGeneration) error {
		var err error
		out, err = s.photoRejects(ctx, q, generation.ID, request)
		return err
	})
	return out, err
}

type PhotoRejectTarget struct {
	AssetID        string `json:"asset_id" format:"uuid"`
	Revision       int64  `json:"revision" minimum:"1"`
	MemberRevision int64  `json:"member_revision" minimum:"1"`
}

type PhotoRejectsMoved struct {
	Moved []string `json:"moved"`
}

// MovePhotoRejects rechecks and trashes the previewed batch atomically.
func (s *Store) MovePhotoRejects(ctx context.Context, hidden bool, targets []PhotoRejectTarget) (PhotoRejectsMoved, error) {
	out := PhotoRejectsMoved{Moved: []string{}}
	if len(targets) > MaxPhotoRejectsMove {
		return out, fmt.Errorf("%w: at most %d targets", ErrInvalidPhotoAsset, MaxPhotoRejectsMove)
	}
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		if validateUUIDv4(target.AssetID) != nil || target.Revision < 1 || target.MemberRevision < 1 || seen[target.AssetID] {
			return out, fmt.Errorf("%w: invalid rejects target", ErrInvalidPhotoAsset)
		}
		seen[target.AssetID] = true
	}
	stale := fmt.Errorf("%w: preview rejects again", ErrStaleRevision)
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if hidden {
			if _, err := s.hiddenSession(ctx, tx); err != nil {
				return err
			}
		}
		live := 0
		for _, target := range targets {
			asset, err := s.photoAssetForMutationTx(ctx, tx, target.AssetID, target.Revision)
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrStaleRevision) || !hidden && errors.Is(err, ErrHiddenLocked) {
				return stale
			}
			if err != nil {
				return err
			}
			if (asset.HiddenAt != nil) != hidden {
				return stale
			}
			var memberRevision int64
			for _, file := range asset.Files {
				if file.Role != PhotoRoleSidecar && file.Flag != "reject" {
					return stale
				}
				node, err := nodeByIDTx(tx, file.NodeID)
				if errors.Is(err, ErrNotFound) {
					return stale
				}
				if err != nil {
					return err
				}
				memberRevision += node.Revision
				if node.TrashedAt == nil {
					live++
				}
			}
			// Ordinary member trash advances node revisions without advancing the asset.
			if memberRevision != target.MemberRevision {
				return stale
			}
			if live > MaxPhotoRejectsMove {
				return fmt.Errorf("%w: at most %d live files", ErrInvalidPhotoAsset, MaxPhotoRejectsMove)
			}
			if _, err := s.trashPhotoAssetTx(ctx, tx, asset); err != nil {
				return err
			}
			out.Moved = append(out.Moved, asset.ID)
		}
		return nil
	})
	if err != nil {
		return PhotoRejectsMoved{}, err
	}
	return out, nil
}

type photoRejectMember struct {
	AssetID       string
	AssetRevision int64
	FileID        string
	Role          string
	Flag          string
	NodeRevision  int64
	TrashedAt     *string
	Name          string
}

func (s *Store) photoRejects(ctx context.Context, q metadataQuerier, generation string, request PhotoRejectsRequest) (PhotoRejectsPreflight, error) {
	out := PhotoRejectsPreflight{Mixed: []PhotoRejectMixed{}, Targets: []PhotoRejectTarget{}}
	if request.Hidden {
		if _, err := s.hiddenSession(ctx, q); err != nil {
			return out, err
		}
	}
	coverage := CoverageSelection{}
	compiled, err := (queryCompiler{photoDisplayMetadata: true, photoHidden: request.Hidden}).compile(ctx, request.Query, queryResolver{q: q})
	if err != nil {
		return out, err
	}
	match, err := photoBrowseMatch(compiled, generation, coverage, request.Hidden)
	if err != nil {
		return out, err
	}
	statement, args, err := bindQueryPopulation(compiledQueryFragment{
		sql: `SELECT a.asset_id,a.revision,member.file_id,member.role,member.flag,mn.revision,mn.trashed_at,mn.name FROM ` + photoBrowseDisplayFrom + `
 CROSS JOIN photo_files member ON member.asset_id=a.asset_id
 CROSS JOIN nodes mn ON mn.id=member.node_id WHERE ` + photoBrowseLiveDisplay + ` AND ` + photoVisibilityPredicate(request.Hidden) + ` AND ` + match.sql + ` ORDER BY a.asset_id,member.file_id`,
		args: match.args, relations: match.relations,
	}, coverage, generation)
	if err != nil {
		return out, err
	}
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	batchFiles := 0
	var members []photoRejectMember
	finish := func() {
		if len(members) == 0 {
			return
		}
		out.Unchanged++
		var memberRevision int64
		originals, rejected, live := 0, 0, 0
		for _, member := range members {
			memberRevision += member.NodeRevision
			if member.TrashedAt == nil {
				live++
			}
			if member.Role == PhotoRoleSidecar {
				continue
			}
			originals++
			if member.Flag == "reject" {
				rejected++
			}
		}
		if rejected == 0 {
			return
		}
		if rejected == originals {
			out.Photos++
			out.Files += live
			if batchFiles+live <= MaxPhotoRejectsMove {
				batchFiles += live
				out.Targets = append(out.Targets, PhotoRejectTarget{members[0].AssetID, members[0].AssetRevision, memberRevision})
			}
		} else {
			out.MixedCount++
			if len(out.Mixed) < MaxPhotoRejectsMixed {
				mixed := PhotoRejectMixed{AssetID: members[0].AssetID, Members: []PhotoRejectMember{}}
				for _, member := range members {
					if member.Role != PhotoRoleSidecar {
						mixed.Members = append(mixed.Members, PhotoRejectMember{member.FileID, member.Name, member.Flag, member.TrashedAt != nil})
					}
				}
				out.Mixed = append(out.Mixed, mixed)
			}
		}
	}
	for rows.Next() {
		var member photoRejectMember
		if err := rows.Scan(&member.AssetID, &member.AssetRevision, &member.FileID, &member.Role, &member.Flag, &member.NodeRevision, &member.TrashedAt, &member.Name); err != nil {
			return out, err
		}
		if len(members) > 0 && member.AssetID != members[0].AssetID {
			finish()
			members = members[:0]
		}
		members = append(members, member)
	}
	finish()
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return out, err
	}
	if closeErr != nil {
		return out, closeErr
	}
	out.Movable = len(out.Targets)
	out.Unchanged -= out.Photos
	return out, nil
}
