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

type PhotoRejectsRequest struct {
	Query    query.Query
	Coverage CoverageSelection
	Hidden   bool
}

type PhotoRejectMember struct {
	FileID string `json:"file_id"`
	Name   string `json:"name"`
	Flag   string `json:"flag"`
}

type PhotoRejectMixed struct {
	AssetID string              `json:"asset_id"`
	Members []PhotoRejectMember `json:"members"`
}

type PhotoRejectsPreflight struct {
	Digest    string             `json:"digest"`
	Photos    int                `json:"photos"`
	Files     int                `json:"files"`
	Unchanged int                `json:"unchanged"`
	Mixed     []PhotoRejectMixed `json:"mixed"`
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
		var assets []PhotoAsset
		var err error
		generation, err := readActiveLexicalGeneration(ctx, tx)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		out, assets, err = s.photoRejects(ctx, tx, generation.ID, request)
		if err != nil {
			return err
		}
		if digest == "" || digest != out.Digest {
			return fmt.Errorf("%w: photo scope changed; preview rejects again", ErrStaleRevision)
		}
		for _, asset := range assets {
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

func (s *Store) photoRejects(ctx context.Context, q metadataQuerier, generation string, request PhotoRejectsRequest) (PhotoRejectsPreflight, []PhotoAsset, error) {
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
		sql: `SELECT a.asset_id,a.revision,member.file_id,member.revision,member.role,member.flag,mn.id,mn.revision,COALESCE(mn.current_version_id,''),mn.trashed_at FROM ` + photoBrowseDisplayFrom + `
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
	var ids []string
	previous, candidate := "", ""
	for rows.Next() {
		var fence struct {
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
		}
		if err := rows.Scan(&fence.AssetID, &fence.AssetRevision, &fence.FileID, &fence.FileRevision, &fence.Role, &fence.Flag, &fence.NodeID, &fence.NodeRevision, &fence.VersionID, &fence.TrashedAt); err != nil {
			_ = rows.Close()
			return out, nil, err
		}
		encoded, err := json.Marshal(fence)
		if err != nil {
			_ = rows.Close()
			return out, nil, err
		}
		_, _ = hash.Write(encoded)
		if fence.AssetID != previous {
			out.Unchanged++
			previous = fence.AssetID
		}
		if fence.Role != PhotoRoleSidecar && fence.Flag == "reject" && candidate != fence.AssetID {
			ids = append(ids, fence.AssetID)
			candidate = fence.AssetID
		}
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return out, nil, err
	}
	if closeErr != nil {
		return out, nil, closeErr
	}
	var eligible []PhotoAsset
	for _, id := range ids {
		asset, err := s.photoAssetReadQuery(ctx, q, id)
		if err != nil {
			return out, nil, err
		}
		mixed := PhotoRejectMixed{AssetID: id, Members: []PhotoRejectMember{}}
		rejected, originals, live := 0, 0, 0
		firstFlag, disagrees := "", false
		for _, file := range asset.Files {
			node, err := nodeByIDQuery(ctx, q, file.NodeID)
			if err != nil {
				return out, nil, err
			}
			if node.TrashedAt == nil {
				live++
			}
			if file.Role == PhotoRoleSidecar {
				continue
			}
			if originals == 0 {
				firstFlag = file.Flag
			} else {
				disagrees = disagrees || firstFlag != file.Flag
			}
			originals++
			if file.Flag == "reject" {
				rejected++
			}
			mixed.Members = append(mixed.Members, PhotoRejectMember{file.ID, node.Name, file.Flag})
		}
		if originals > 0 && rejected == originals {
			eligible = append(eligible, asset)
			out.Photos++
			out.Files += live
			if out.Photos > maxBatchTagTargets || out.Files > maxBatchTagTargets {
				return out, nil, fmt.Errorf("%w: eligible rejects exceed %d photos or live files", ErrInvalidPhotoQuery, maxBatchTagTargets)
			}
		} else {
			if disagrees {
				out.Mixed = append(out.Mixed, mixed)
			}
		}
	}
	out.Unchanged -= out.Photos
	out.Digest = hex.EncodeToString(hash.Sum(nil))
	return out, eligible, nil
}
