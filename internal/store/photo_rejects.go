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

var errPhotoRejectsStale = fmt.Errorf("%w: preview rejects again", ErrStaleRevision)

type PhotoRejectsRequest struct {
	Query    query.Query
	Hidden   bool
	Coverage CoverageSelection
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
	SelectionLimit int                 `json:"selection_limit"`
	Targets        []PhotoRejectTarget `json:"targets"`
	Photos         int                 `json:"photos"`
	Files          int                 `json:"files"`
	Unchanged      int                 `json:"unchanged"`
	Mixed          []PhotoRejectMixed  `json:"mixed"`
	MixedCount     int                 `json:"mixed_count"`
}

// PreflightPhotoRejects evaluates the complete scope in one read snapshot.
func (s *Store) PreflightPhotoRejects(
	ctx context.Context, request PhotoRejectsRequest,
) (PhotoRejectsPreflight, error) {
	var out PhotoRejectsPreflight
	read := func(q metadataQuerier, generation LexicalGeneration) error {
		var err error
		out, err = s.photoRejects(ctx, q, generation.ID, request)
		return err
	}
	err := s.withLexicalGenerationRead(ctx, read)
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
func (s *Store) MovePhotoRejects(
	ctx context.Context, hidden bool, targets []PhotoRejectTarget,
) (PhotoRejectsMoved, error) {
	if err := validatePhotoRejectTargets(targets); err != nil {
		return PhotoRejectsMoved{}, err
	}
	out := PhotoRejectsMoved{Moved: []string{}}
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if hidden {
			if _, err := s.hiddenSession(ctx, tx); err != nil {
				return err
			}
		}
		audited, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		live := 0
		for _, target := range targets {
			asset, nodes, err := photoRejectForMoveTx(ctx, tx, hidden, target)
			if err != nil {
				return err
			}
			pending := 0
			for _, node := range nodes {
				if node.TrashedAt == nil {
					pending++
				}
			}
			if pending == 0 {
				return errPhotoRejectsStale
			}
			if live+pending > MaxPhotoRejectsMove {
				return fmt.Errorf("%w: at most %d live files", ErrInvalidPhotoAsset, MaxPhotoRejectsMove)
			}
			live += pending
			if _, _, err := s.trashPhotoAssetTx(ctx, tx, asset, nodes, audited); err != nil {
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

func validatePhotoRejectTargets(targets []PhotoRejectTarget) error {
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		if seen[target.AssetID] {
			return fmt.Errorf("%w: invalid rejects target %q", ErrInvalidPhotoAsset, target.AssetID)
		}
		seen[target.AssetID] = true
	}
	return nil
}

// photoRejectForMoveTx rereads one previewed target and reports any change
// since the preview as stale.
func photoRejectForMoveTx(
	ctx context.Context, tx *sql.Tx, hidden bool, target PhotoRejectTarget,
) (PhotoAsset, []Node, error) {
	asset, err := photoAssetByIDQuery(ctx, tx, target.AssetID)
	if errors.Is(err, ErrNotFound) {
		return PhotoAsset{}, nil, errPhotoRejectsStale
	}
	if err != nil {
		return PhotoAsset{}, nil, err
	}
	if asset.Revision != target.Revision || (asset.HiddenAt != nil) != hidden {
		return PhotoAsset{}, nil, errPhotoRejectsStale
	}
	var memberRevision int64
	nodes := make([]Node, 0, len(asset.Files))
	for _, file := range asset.Files {
		if file.Role != PhotoRoleSidecar && file.Flag != "reject" {
			return PhotoAsset{}, nil, errPhotoRejectsStale
		}
		node, err := nodeByIDTx(tx, file.NodeID)
		if errors.Is(err, ErrNotFound) {
			return PhotoAsset{}, nil, errPhotoRejectsStale
		}
		if err != nil {
			return PhotoAsset{}, nil, err
		}
		memberRevision += node.Revision
		nodes = append(nodes, node)
	}
	// Content replacement and member trash advance node revisions independently.
	if memberRevision != target.MemberRevision {
		return PhotoAsset{}, nil, errPhotoRejectsStale
	}
	return asset, nodes, nil
}

// photoRejectMember is one member of an asset with a rejected original.
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

// Only assets with rejected originals need member classification in Go.
const photoRejectsSelect = `SELECT a.asset_id,a.revision,COALESCE(member.file_id,''),
 COALESCE(member.role,''),COALESCE(member.flag,''),COALESCE(mn.revision,0),mn.trashed_at,
 COALESCE(mn.name,'') FROM ` + photoBrowseDisplayFrom + `
 JOIN photo_files member ON member.asset_id=a.asset_id AND EXISTS (
  SELECT 1 FROM photo_files rejected WHERE rejected.asset_id=a.asset_id
  AND rejected.role<>'sidecar' AND rejected.flag='reject')
 LEFT JOIN nodes mn ON mn.id=member.node_id`

func (s *Store) photoRejects(
	ctx context.Context, q metadataQuerier, generation string, request PhotoRejectsRequest,
) (PhotoRejectsPreflight, error) {
	if request.Hidden {
		if _, err := s.hiddenSession(ctx, q); err != nil {
			return PhotoRejectsPreflight{}, err
		}
	}
	coverage, err := normalizeCoverageSelection(request.Coverage)
	if err != nil {
		return PhotoRejectsPreflight{}, err
	}
	compiler := queryCompiler{photoDisplayMetadata: true, photoHidden: request.Hidden}
	compiled, err := compiler.compile(ctx, request.Query, queryResolver{q: q})
	if err != nil {
		return PhotoRejectsPreflight{}, err
	}
	scope, err := newPhotoBrowseScope(ctx, q, compiled, generation, coverage, request.Hidden)
	if err != nil {
		return PhotoRejectsPreflight{}, err
	}
	countSQL, countArgs, err := scope.bind(`SELECT COUNT(*) FROM ` + photoBrowseDisplayFrom + ` WHERE ` + scope.where())
	if err != nil {
		return PhotoRejectsPreflight{}, err
	}
	batch := newPhotoRejectsBatch()
	var scopedCount int
	if err := q.QueryRowContext(ctx, countSQL, countArgs...).Scan(&scopedCount); err != nil {
		return PhotoRejectsPreflight{}, err
	}
	statement, args, err := scope.bind(photoRejectsSelect + ` WHERE ` + scope.where() +
		` ORDER BY a.asset_id,member.file_id`)
	if err != nil {
		return PhotoRejectsPreflight{}, err
	}
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return PhotoRejectsPreflight{}, err
	}
	defer func() { _ = rows.Close() }()
	var members []photoRejectMember
	for rows.Next() {
		var m photoRejectMember
		if err := rows.Scan(&m.AssetID, &m.AssetRevision, &m.FileID, &m.Role, &m.Flag,
			&m.NodeRevision, &m.TrashedAt, &m.Name); err != nil {
			return PhotoRejectsPreflight{}, err
		}
		if len(members) > 0 && m.AssetID != members[0].AssetID {
			batch.add(members)
			members = members[:0]
		}
		members = append(members, m)
	}
	batch.add(members)
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return PhotoRejectsPreflight{}, err
	}
	if closeErr != nil {
		return PhotoRejectsPreflight{}, closeErr
	}
	batch.out.Unchanged = scopedCount - batch.out.Photos
	return batch.out, nil
}

// photoRejectsBatch classifies scoped assets and fills one bounded move batch.
type photoRejectsBatch struct {
	out   PhotoRejectsPreflight
	files int
}

func newPhotoRejectsBatch() *photoRejectsBatch {
	return &photoRejectsBatch{out: PhotoRejectsPreflight{
		Mixed: []PhotoRejectMixed{}, Targets: []PhotoRejectTarget{}, SelectionLimit: query.MaxIDValues,
	}}
}

// add classifies one asset's members. An eligible photo that would push the
// batch past MaxPhotoRejectsMove waits for a later batch while smaller photos
// after it still fill this one. PhotoMaxFiles keeps every photo within the bound.
func (b *photoRejectsBatch) add(members []photoRejectMember) {
	if len(members) == 0 {
		return
	}
	var memberRevision int64
	originals, rejected, live := 0, 0, 0
	for _, member := range members {
		memberRevision += member.NodeRevision
		if member.TrashedAt == nil {
			live++
		}
		if member.Role != PhotoRoleSidecar {
			originals++
			if member.Flag == "reject" {
				rejected++
			}
		}
	}
	if rejected != originals {
		b.addMixed(members)
		return
	}
	b.out.Photos++
	b.out.Files += live
	if b.files+live <= MaxPhotoRejectsMove {
		b.files += live
		b.out.Targets = append(b.out.Targets, PhotoRejectTarget{
			AssetID: members[0].AssetID, Revision: members[0].AssetRevision, MemberRevision: memberRevision,
		})
	}
}

func (b *photoRejectsBatch) addMixed(members []photoRejectMember) {
	b.out.MixedCount++
	if len(b.out.Mixed) >= MaxPhotoRejectsMixed {
		return
	}
	mixed := PhotoRejectMixed{AssetID: members[0].AssetID, Members: []PhotoRejectMember{}}
	for _, member := range members {
		if member.Role != PhotoRoleSidecar {
			mixed.Members = append(mixed.Members, PhotoRejectMember{
				FileID: member.FileID, Name: member.Name, Flag: member.Flag, InTrash: member.TrashedAt != nil,
			})
		}
	}
	b.out.Mixed = append(b.out.Mixed, mixed)
}
