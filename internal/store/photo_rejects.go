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
	Digest          string             `json:"digest"`
	Photos          int                `json:"photos"`
	Files           int                `json:"files"`
	Unchanged       int                `json:"unchanged"`
	CheckoutSkipped int                `json:"checkout_skipped"`
	Mixed           []PhotoRejectMixed `json:"mixed"`
}

// PreflightPhotoRejects reads the complete scope within the existing batch bound.
func (s *Store) PreflightPhotoRejects(ctx context.Context, request PhotoRejectsRequest) (PhotoRejectsPreflight, error) {
	var out PhotoRejectsPreflight
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, _, err = s.photoRejectsTx(ctx, tx, request)
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
		out, assets, err = s.photoRejectsTx(ctx, tx, request)
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

func (s *Store) photoRejectsTx(ctx context.Context, tx *sql.Tx, request PhotoRejectsRequest) (PhotoRejectsPreflight, []PhotoAsset, error) {
	out := PhotoRejectsPreflight{Mixed: []PhotoRejectMixed{}}
	if request.Hidden {
		if _, err := s.hiddenSession(ctx, tx); err != nil {
			return out, nil, err
		}
	}
	coverage, err := normalizeCoverageSelection(request.Coverage)
	if err != nil {
		return out, nil, err
	}
	if coverage.Configuration == photoBrowseConfiguredCoverage {
		if err := validateSnapshotCoverageProfile(ctx, tx, coverage); err != nil {
			return out, nil, err
		}
	}
	compiled, err := (queryCompiler{photoDisplayMetadata: true, photoHidden: request.Hidden}).compile(ctx, request.Query, queryResolver{q: tx})
	if err != nil {
		return out, nil, err
	}
	generation, err := readActiveLexicalGeneration(ctx, tx)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return out, nil, err
	}
	match, err := photoBrowseMatch(compiled, generation.ID, coverage, request.Hidden)
	if err != nil {
		return out, nil, err
	}
	statement, args, err := bindQueryPopulation(compiledQueryFragment{
		sql:  `SELECT a.asset_id FROM ` + photoBrowseDisplayFrom + ` WHERE ` + photoBrowseLiveDisplay + ` AND ` + photoVisibilityPredicate(request.Hidden) + ` AND ` + match.sql + ` ORDER BY a.asset_id LIMIT ?`,
		args: append(match.args, maxBatchTagTargets+1), relations: match.relations,
	}, coverage, generation.ID)
	if err != nil {
		return out, nil, err
	}
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return out, nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return out, nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return out, nil, err
	}
	if closeErr != nil {
		return out, nil, closeErr
	}
	if len(ids) > maxBatchTagTargets {
		return out, nil, fmt.Errorf("%w: rejects scope exceeds %d photos", ErrInvalidPhotoQuery, maxBatchTagTargets)
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
	var eligible []PhotoAsset
	totalFiles := 0
	for _, id := range ids {
		asset, err := s.photoAssetReadQuery(ctx, tx, id)
		if err != nil {
			return out, nil, err
		}
		totalFiles += len(asset.Files)
		if totalFiles > maxBatchTagTargets {
			return out, nil, fmt.Errorf("%w: rejects scope exceeds %d files", ErrInvalidPhotoQuery, maxBatchTagTargets)
		}
		encoded, err := json.Marshal(asset, json.Deterministic(true))
		if err != nil {
			return out, nil, err
		}
		_, _ = hash.Write(encoded)
		mixed := PhotoRejectMixed{AssetID: id, Members: []PhotoRejectMember{}}
		rejected, originals, live := 0, 0, 0
		firstFlag, disagrees := "", false
		for _, file := range asset.Files {
			node, err := nodeByIDTx(tx, file.NodeID)
			if err != nil {
				return out, nil, err
			}
			encoded, err := json.Marshal(struct {
				NodeID    int64
				Revision  int64
				VersionID string
				TrashedAt *string
			}{node.ID, node.Revision, node.CurrentVersionID, node.TrashedAt})
			if err != nil {
				return out, nil, err
			}
			_, _ = hash.Write(encoded)
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
		} else {
			out.Unchanged++
			if disagrees {
				out.Mixed = append(out.Mixed, mixed)
			}
		}
	}
	out.Digest = hex.EncodeToString(hash.Sum(nil))
	return out, eligible, nil
}
