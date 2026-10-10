package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/query"
)

const maxPhotoRejectsMoveTargets = 1000

type PhotoRejectsRequest struct {
	Query    query.Query
	Coverage CoverageSelection
	Hidden   bool
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
	Digest     string             `json:"digest"`
	Photos     int                `json:"photos"`
	Files      int                `json:"files"`
	Unchanged  int                `json:"unchanged"`
	Mixed      []PhotoRejectMixed `json:"mixed"`
	MixedCount int                `json:"mixed_count"`
}

// PreflightPhotoRejects evaluates the complete scope in one read snapshot.
func (s *Store) PreflightPhotoRejects(ctx context.Context, request PhotoRejectsRequest) (PhotoRejectsPreflight, error) {
	var out PhotoRejectsPreflight
	err := s.withLexicalGenerationRead(ctx, func(q metadataQuerier, generation LexicalGeneration) error {
		var err error
		out, _, err = s.photoRejects(ctx, q, generation.ID, request)
		return err
	})
	return out, err
}

// MovePhotoRejects checks the preflight and trashes every eligible asset in one transaction.
func (s *Store) MovePhotoRejects(ctx context.Context, request PhotoRejectsRequest, digest string) (PhotoRejectsPreflight, error) {
	var out PhotoRejectsPreflight
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var ids []string
		var err error
		generation, err := readActiveLexicalGeneration(ctx, tx)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		out, ids, err = s.photoRejects(ctx, tx, generation.ID, request)
		if err != nil {
			return err
		}
		if out.Photos > maxPhotoRejectsMoveTargets || out.Files > maxPhotoRejectsMoveTargets {
			return fmt.Errorf("%w: select fewer photos; moves allow at most %d photos or live files", ErrInvalidPhotoQuery, maxPhotoRejectsMoveTargets)
		}
		if digest == "" || digest != out.Digest {
			return fmt.Errorf("%w: photo scope changed; preview rejects again", ErrStaleRevision)
		}
		for _, id := range ids {
			asset, err := s.photoAssetReadQuery(ctx, tx, id)
			if err != nil {
				return err
			}
			if _, err := s.trashPhotoAssetTx(ctx, tx, asset); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return PhotoRejectsPreflight{}, err
	}
	return out, nil
}

func (s *Store) photoRejects(ctx context.Context, q metadataQuerier, generation string, request PhotoRejectsRequest) (PhotoRejectsPreflight, []string, error) {
	out := PhotoRejectsPreflight{Mixed: []PhotoRejectMixed{}}
	if request.Hidden {
		if _, err := s.hiddenSession(ctx, q); err != nil {
			return out, nil, err
		}
	}
	coverage, err := normalizeCoverageSelection(request.Coverage)
	if err != nil {
		return out, nil, err
	}
	if coverage.Configuration == photoBrowseConfiguredCoverage {
		if err := validateSnapshotCoverageProfile(ctx, q, coverage); err != nil {
			return out, nil, err
		}
	}
	compiled, err := (queryCompiler{photoDisplayMetadata: true, photoHidden: request.Hidden}).compile(ctx, request.Query, queryResolver{q: q})
	if err != nil {
		return out, nil, err
	}
	match, err := photoBrowseMatch(compiled, generation, coverage, request.Hidden)
	if err != nil {
		return out, nil, err
	}
	canonical, err := query.Canonical(compiled.Query)
	if err != nil {
		return out, nil, err
	}
	hash := sha256.New()
	identity, err := json.Marshal(struct {
		Query        []byte
		Dependencies []query.Dependency
		Coverage     CoverageSelection
		Hidden       bool
	}{canonical, compiled.Dependencies, coverage, request.Hidden})
	if err != nil {
		return out, nil, err
	}
	_, _ = hash.Write(identity)
	statement, args, err := bindQueryPopulation(compiledQueryFragment{
		sql: `SELECT a.asset_id,a.revision,member.file_id,member.revision,member.role,member.flag,mn.id,mn.revision,COALESCE(mn.current_version_id,''),mn.trashed_at,mn.name FROM ` + photoBrowseDisplayFrom + `
 CROSS JOIN photo_files member ON member.asset_id=a.asset_id
 CROSS JOIN nodes mn ON mn.id=member.node_id WHERE ` + photoBrowseLiveDisplay + ` AND ` + photoVisibilityPredicate(request.Hidden) + ` AND ` + match.sql + ` ORDER BY a.asset_id,member.file_id`,
		args: match.args, relations: match.relations,
	}, coverage, generation)
	if err != nil {
		return out, nil, err
	}
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return out, nil, err
	}
	var eligible []string
	type memberFence struct {
		AssetID       string
		AssetRevision int64
		FileID        string
		FileRevision  int64
		Role          string
		Flag          string
		NodeID        int64
		NodeRevision  int64
		VersionID     string
		TrashedAt     *string
		Name          string
	}
	var members []memberFence
	finish := func() error {
		if len(members) == 0 {
			return nil
		}
		out.Unchanged++
		// Scope membership matters; unrelated retained-file edits do not.
		_, _ = fmt.Fprintln(hash, members[0].AssetID)
		originals, rejected, live := 0, 0, 0
		for _, member := range members {
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
			return nil
		}
		encoded, err := json.Marshal(members)
		if err != nil {
			return err
		}
		_, _ = hash.Write(encoded)
		if rejected == originals {
			out.Photos++
			out.Files += live
			if out.Photos <= maxPhotoRejectsMoveTargets && out.Files <= maxPhotoRejectsMoveTargets {
				eligible = append(eligible, members[0].AssetID)
			}
		} else {
			out.MixedCount++
			if len(out.Mixed) < 20 {
				mixed := PhotoRejectMixed{AssetID: members[0].AssetID, Members: []PhotoRejectMember{}}
				for _, member := range members {
					if member.Role != PhotoRoleSidecar {
						mixed.Members = append(mixed.Members, PhotoRejectMember{member.FileID, member.Name, member.Flag, member.TrashedAt != nil})
					}
				}
				out.Mixed = append(out.Mixed, mixed)
			}
		}
		return nil
	}
	for rows.Next() {
		var member memberFence
		if err := rows.Scan(&member.AssetID, &member.AssetRevision, &member.FileID, &member.FileRevision, &member.Role, &member.Flag, &member.NodeID, &member.NodeRevision, &member.VersionID, &member.TrashedAt, &member.Name); err != nil {
			_ = rows.Close()
			return out, nil, err
		}
		if len(members) > 0 && member.AssetID != members[0].AssetID {
			if err := finish(); err != nil {
				_ = rows.Close()
				return out, nil, err
			}
			members = members[:0]
		}
		members = append(members, member)
	}
	if err := finish(); err != nil {
		_ = rows.Close()
		return out, nil, err
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return out, nil, err
	}
	if closeErr != nil {
		return out, nil, closeErr
	}
	out.Unchanged -= out.Photos
	out.Digest = hex.EncodeToString(hash.Sum(nil))
	return out, eligible, nil
}
