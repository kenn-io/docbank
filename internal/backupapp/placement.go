package backupapp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

const (
	// PlacementFormat identifies the portable, non-secret physical-placement
	// description captured alongside Docbank's logical metadata.
	PlacementFormat = "docbank-placement-v1"
	placementName   = "placement"
)

type placementManifest struct {
	Format    string              `json:"format"`
	Stores    []placementStore    `json:"stores"`
	Locations []placementLocation `json:"locations"`
}

type placementStore struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Role    string `json:"role"`
	Objects int64  `json:"objects"`
	Bytes   int64  `json:"bytes"`
}

type placementLocation struct {
	Hash     string   `json:"hash"`
	StoreIDs []string `json:"store_ids"`
}

var _ backup.AuxiliarySource = (*metadataSnapshot)(nil)

func (s *metadataSnapshot) AuxiliaryArtifacts(
	ctx context.Context,
) ([]backup.AuxiliaryArtifact, error) {
	if s.closed {
		return nil, errors.New("backupapp: metadata snapshot is closed")
	}
	raw, err := encodePlacementManifest(ctx, s.tx)
	if err != nil {
		return nil, err
	}
	return []backup.AuxiliaryArtifact{{
		Name: placementName, Format: PlacementFormat,
		Open: func(context.Context) (io.ReadCloser, int64, error) {
			return io.NopCloser(bytes.NewReader(raw)), int64(len(raw)), nil
		},
	}}, nil
}

func encodePlacementManifest(ctx context.Context, q rowQuerier) ([]byte, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT s.store_id, s.name, s.kind, s.role,
		       COUNT(l.blob_hash), COALESCE(SUM(b.size), 0)
		FROM blob_stores s
		LEFT JOIN blob_locations l ON l.store_id = s.store_id
		LEFT JOIN blobs b ON b.hash = l.blob_hash
		GROUP BY s.store_id, s.name, s.kind, s.role
		ORDER BY CASE s.role WHEN 'primary' THEN 0 ELSE 1 END, s.name, s.store_id`)
	if err != nil {
		return nil, fmt.Errorf("backupapp: listing placement stores: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var manifest placementManifest
	manifest.Format = PlacementFormat
	for rows.Next() {
		var store placementStore
		if err := rows.Scan(
			&store.ID, &store.Name, &store.Kind, &store.Role, &store.Objects, &store.Bytes,
		); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("backupapp: scanning placement store: %w", err)
		}
		manifest.Stores = append(manifest.Stores, store)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("backupapp: listing placement stores: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("backupapp: closing placement stores: %w", err)
	}

	rows, err = q.QueryContext(ctx, `
		SELECT b.hash, l.store_id
		FROM blobs b
		LEFT JOIN blob_locations l ON l.blob_hash = b.hash
		ORDER BY b.hash, l.store_id`)
	if err != nil {
		return nil, fmt.Errorf("backupapp: listing placement locations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var hash string
		var storeID sql.NullString
		if err := rows.Scan(&hash, &storeID); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("backupapp: scanning placement location: %w", err)
		}
		last := len(manifest.Locations) - 1
		if last < 0 || manifest.Locations[last].Hash != hash {
			manifest.Locations = append(manifest.Locations, placementLocation{Hash: hash})
			last++
		}
		if storeID.Valid {
			manifest.Locations[last].StoreIDs = append(
				manifest.Locations[last].StoreIDs, storeID.String,
			)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("backupapp: listing placement locations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("backupapp: closing placement locations: %w", err)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("backupapp: encoding placement manifest: %w", err)
	}
	return append(raw, '\n'), nil
}

type placementRestoreTarget struct {
	manifest *placementManifest
}

var _ backup.AuxiliaryTarget = placementRestoreTarget{}

func (target placementRestoreTarget) StageAuxiliary(
	_ context.Context, artifacts []backup.RestoredAuxiliary,
) (backup.AuxiliaryRestore, error) {
	manifest, err := decodePlacementArtifacts(artifacts)
	if err != nil {
		return nil, err
	}
	if target.manifest != nil {
		*target.manifest = manifest
	}
	return placementAuxiliaryRestore{}, nil
}

type placementAuxiliaryRestore struct{}

func (placementAuxiliaryRestore) Commit(context.Context) error   { return nil }
func (placementAuxiliaryRestore) Rollback(context.Context) error { return nil }

func decodePlacementArtifacts(
	artifacts []backup.RestoredAuxiliary,
) (placementManifest, error) {
	if len(artifacts) != 1 {
		return placementManifest{}, fmt.Errorf(
			"backupapp: expected one placement artifact, got %d", len(artifacts),
		)
	}
	artifact := artifacts[0]
	if artifact.Name != placementName || artifact.Format != PlacementFormat {
		return placementManifest{}, fmt.Errorf(
			"backupapp: unsupported auxiliary artifact %q (%s)",
			artifact.Name, artifact.Format,
		)
	}
	var manifest placementManifest
	decoder := json.NewDecoder(bytes.NewReader(artifact.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return placementManifest{}, fmt.Errorf("backupapp: decoding placement manifest: %w", err)
	}
	if err := validatePlacementManifest(manifest); err != nil {
		return placementManifest{}, err
	}
	return manifest, nil
}

func loadPlacementManifest(
	ctx context.Context, repo *backup.Repo, snapshotID, packExtension string,
) (placementManifest, string, error) {
	manifest, err := repo.LoadManifest(snapshotID)
	if err != nil {
		return placementManifest{}, "", fmt.Errorf(
			"backupapp: loading snapshot manifest: %w", err,
		)
	}
	if len(manifest.Auxiliary) != 1 {
		return placementManifest{}, "", fmt.Errorf(
			"backupapp: expected one placement artifact, got %d", len(manifest.Auxiliary),
		)
	}
	artifact := manifest.Auxiliary[0]
	id, err := pack.ParseBlobID(artifact.Blob)
	if err != nil {
		return placementManifest{}, "", fmt.Errorf(
			"backupapp: placement artifact identity: %w", err,
		)
	}
	known, err := repo.LoadBlobIndex()
	if err != nil {
		return placementManifest{}, "", fmt.Errorf(
			"backupapp: loading repository blob index: %w", err,
		)
	}
	stream, err := repo.OpenBlob(ctx, known, id, nil, packExtension)
	if err != nil {
		return placementManifest{}, "", fmt.Errorf(
			"backupapp: opening placement artifact: %w", err,
		)
	}
	data, err := readPlacementArtifact(stream, artifact.Bytes)
	if err != nil {
		return placementManifest{}, "", err
	}
	placement, err := decodePlacementArtifacts([]backup.RestoredAuxiliary{{
		Name: artifact.Name, Format: artifact.Format,
		SHA256: artifact.SHA256, Data: data,
	}})
	if err != nil {
		return placementManifest{}, "", err
	}
	return placement, manifest.SnapshotID, nil
}

func readPlacementArtifact(
	stream *backup.BlobStream, expectedSize int64,
) (data []byte, resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, stream.Close()) }()
	if stream.Size() != expectedSize {
		return nil, fmt.Errorf(
			"backupapp: placement artifact size is %d, expected %d",
			stream.Size(), expectedSize,
		)
	}
	data, err := io.ReadAll(stream)
	if err != nil {
		return nil, fmt.Errorf("backupapp: reading placement artifact: %w", err)
	}
	if err := stream.Verify(); err != nil {
		return nil, fmt.Errorf("backupapp: verifying placement artifact: %w", err)
	}
	return data, nil
}

func validatePlacementManifest(manifest placementManifest) error {
	if manifest.Format != PlacementFormat {
		return fmt.Errorf("backupapp: placement format is %q", manifest.Format)
	}
	storeIDs := make(map[string]struct{}, len(manifest.Stores))
	for i, store := range manifest.Stores {
		if store.ID == "" || store.Name == "" || store.Kind == "" || store.Role == "" {
			return fmt.Errorf("backupapp: placement store %d has incomplete identity", i)
		}
		if store.Objects < 0 || store.Bytes < 0 {
			return fmt.Errorf("backupapp: placement store %q has negative inventory", store.Name)
		}
		if _, duplicate := storeIDs[store.ID]; duplicate {
			return fmt.Errorf("backupapp: duplicate placement store %q", store.ID)
		}
		storeIDs[store.ID] = struct{}{}
	}
	for i, location := range manifest.Locations {
		if _, err := packstore.ParseHash(location.Hash); err != nil {
			return fmt.Errorf("backupapp: placement location %d: %w", i, err)
		}
		if i > 0 && manifest.Locations[i-1].Hash >= location.Hash {
			return errors.New("backupapp: placement locations are not uniquely sorted")
		}
		if !slices.IsSorted(location.StoreIDs) {
			return fmt.Errorf(
				"backupapp: placement location %s has invalid store order", location.Hash,
			)
		}
		for j, storeID := range location.StoreIDs {
			if j > 0 && location.StoreIDs[j-1] == storeID {
				return fmt.Errorf(
					"backupapp: placement location %s repeats store %s",
					location.Hash, storeID,
				)
			}
			if _, known := storeIDs[storeID]; !known {
				return fmt.Errorf(
					"backupapp: placement location %s names unknown store %s",
					location.Hash, storeID,
				)
			}
		}
	}
	return nil
}
