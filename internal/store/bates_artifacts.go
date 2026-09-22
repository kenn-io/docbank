package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/canonical"
)

type BatesArtifactPage struct {
	Ordinal          int    `json:"ordinal"`
	OccurrenceID     string `json:"occurrence_id"`
	SourceBlobSHA256 string `json:"source_blob_sha256"`
	SourcePage       int    `json:"source_page"`
	OutputPage       int    `json:"output_page"`
	Label            string `json:"label"`
}

type BatesArtifact struct {
	ArtifactID, AllocationID, BlobSHA256, MediaType, RecipeSHA256, ManifestSHA256, State, CreatedAt string
	Size                                                                                            int64
	PageCount                                                                                       int
	RecipeJSON                                                                                      jsontext.Value
	Pages                                                                                           []BatesArtifactPage
}

type BatesArtifactPublication struct {
	ArtifactID, AllocationID, BlobSHA256 string
	Size                                 int64
	PageCount                            int
	RecipeJSON                           jsontext.Value
	Pages                                []BatesArtifactPage
}

// BatesPublicationPlan resolves one reservation back to the exact immutable
// snapshot bytes and page order that it allocated.
func (s *Store) BatesPublicationPlan(ctx context.Context, allocationID string) (BatesAllocation, []BatesArtifactPage, error) {
	allocation, err := s.BatesAllocation(ctx, allocationID)
	if err != nil {
		return BatesAllocation{}, nil, err
	}
	inputs, err := expectedBatesPagesLimited(ctx, s.db, allocation.SnapshotID, MaxSnapshotPages)
	if err != nil || len(inputs) != len(allocation.Labels) {
		return BatesAllocation{}, nil, ErrBatesPageCountMismatch
	}
	pages := make([]BatesArtifactPage, len(inputs))
	for index, input := range inputs {
		label := allocation.Labels[index]
		if label.OccurrenceID != input.OccurrenceID || label.SourcePage != input.SourcePage || label.OutputPage != index+1 {
			return BatesAllocation{}, nil, ErrBatesPageCountMismatch
		}
		pages[index] = BatesArtifactPage{Ordinal: index + 1, OccurrenceID: input.OccurrenceID,
			SourceBlobSHA256: input.UnstampedSHA256, SourcePage: input.SourcePage,
			OutputPage: label.OutputPage, Label: label.Label}
	}
	return allocation, pages, nil
}

// PublishBatesArtifact atomically roots already-durable verified PDF bytes,
// records their manifest and page receipts, and commits the allocation.
func (s *Store) PublishBatesArtifact(ctx context.Context, publication BatesArtifactPublication, physical BlobPhysical) (BatesArtifact, error) {
	var artifact BatesArtifact
	if validateUUIDv4(publication.ArtifactID) != nil || validateUUIDv4(publication.AllocationID) != nil ||
		!canonical.IsSHA256Hex(publication.BlobSHA256) || publication.Size < 1 || publication.PageCount < 1 ||
		len(publication.Pages) != publication.PageCount || len(publication.Pages) > MaxSnapshotPages || len(publication.RecipeJSON) == 0 {
		return artifact, ErrBatesReservationConflict
	}
	recipeSHA := digestCatalogJSON(publication.RecipeJSON)
	manifestSHA, err := batesArtifactManifestSHA(BatesArtifact{
		ArtifactID: publication.ArtifactID, AllocationID: publication.AllocationID,
		BlobSHA256: publication.BlobSHA256, Size: publication.Size, MediaType: "application/pdf",
		PageCount: publication.PageCount, RecipeSHA256: recipeSHA, Pages: publication.Pages,
	})
	if err != nil {
		return artifact, err
	}
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		prior, loadErr := loadBatesArtifactByAllocation(ctx, tx, publication.AllocationID)
		if loadErr == nil {
			// The verified first publisher wins. PDF container metadata may make
			// independently supervised executions byte-distinct even though the
			// sealed allocation, recipe, and page receipts are identical.
			if prior.ArtifactID != publication.ArtifactID || prior.PageCount != publication.PageCount ||
				prior.RecipeSHA256 != recipeSHA || !batesArtifactPagesEqual(prior.Pages, publication.Pages) {
				return ErrBatesReservationConflict
			}
			artifact = prior
			return nil
		}
		if !errors.Is(loadErr, sql.ErrNoRows) {
			return loadErr
		}
		allocation, loadErr := loadBatesAllocation(ctx, tx, publication.AllocationID)
		if loadErr != nil || allocation.State != batesAllocationStateReserved || allocation.RecipeSHA256 != recipeSHA || len(allocation.Labels) != len(publication.Pages) {
			return ErrBatesReservationConflict
		}
		for index, page := range publication.Pages {
			label := allocation.Labels[index]
			if page.Ordinal != index+1 || page.OutputPage != index+1 || page.OccurrenceID != label.OccurrenceID ||
				page.SourcePage != label.SourcePage || page.Label != label.Label || !canonical.IsSHA256Hex(page.SourceBlobSHA256) {
				return ErrBatesPageCountMismatch
			}
		}
		if err := s.EnsureBlobTx(tx, publication.BlobSHA256, publication.Size, physical); err != nil {
			return err
		}
		createdAt := nowRFC3339()
		if _, err := tx.ExecContext(ctx, `INSERT INTO bates_artifacts(artifact_id,allocation_id,blob_hash,size,media_type,page_count,recipe_json,manifest_sha256,state,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, publication.ArtifactID, publication.AllocationID, publication.BlobSHA256,
			publication.Size, batesArtifactMediaTypePDF, publication.PageCount, []byte(publication.RecipeJSON), manifestSHA, batesArtifactStateVerified, createdAt); err != nil {
			return err
		}
		for _, page := range publication.Pages {
			if _, err := tx.ExecContext(ctx, `INSERT INTO bates_artifact_pages(artifact_id,ordinal,occurrence_id,source_blob_sha256,source_page,output_page,label)
				VALUES(?,?,?,?,?,?,?)`, publication.ArtifactID, page.Ordinal, page.OccurrenceID, page.SourceBlobSHA256,
				page.SourcePage, page.OutputPage, page.Label); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE bates_allocations SET state='committed',committed_at=? WHERE allocation_id=? AND state='reserved'`, createdAt, publication.AllocationID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrBatesReservationConflict
		}
		artifact, err = loadBatesArtifact(ctx, tx, publication.ArtifactID)
		return err
	})
	return artifact, err
}

func batesArtifactManifestSHA(artifact BatesArtifact) (string, error) {
	manifest, err := canonical.Marshal(struct {
		Contract     string              `json:"contract"`
		AllocationID string              `json:"allocation_id"`
		ArtifactID   string              `json:"artifact_id"`
		BlobSHA256   string              `json:"blob_sha256"`
		Size         int64               `json:"size"`
		MediaType    string              `json:"media_type"`
		PageCount    int                 `json:"page_count"`
		RecipeSHA256 string              `json:"recipe_sha256"`
		Pages        []BatesArtifactPage `json:"pages"`
	}{"bates-artifact/v1", artifact.AllocationID, artifact.ArtifactID, artifact.BlobSHA256,
		artifact.Size, artifact.MediaType, artifact.PageCount, artifact.RecipeSHA256, artifact.Pages})
	if err != nil {
		return "", err
	}
	return digestCatalogJSON(manifest), nil
}

func (s *Store) BatesArtifact(ctx context.Context, allocationID string) (BatesArtifact, error) {
	artifact, err := loadBatesArtifactByAllocation(ctx, s.db, allocationID)
	if errors.Is(err, sql.ErrNoRows) {
		return BatesArtifact{}, ErrNotFound
	}
	return artifact, err
}

func (s *Store) BatesArtifacts(ctx context.Context, after string, limit int) ([]BatesArtifact, error) {
	if limit < 1 || limit > 250 || after != "" && validateUUIDv4(after) != nil {
		return nil, ErrInvalidBatesCursor
	}
	query := `SELECT artifact_id FROM bates_artifacts ORDER BY created_at,artifact_id LIMIT ?`
	args := []any{limit}
	if after != "" {
		var createdAt string
		if err := s.db.QueryRowContext(ctx, `SELECT created_at FROM bates_artifacts WHERE artifact_id=?`, after).Scan(&createdAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, err
		}
		query = `SELECT artifact_id FROM bates_artifacts
			WHERE created_at>? OR (created_at=? AND artifact_id>?) ORDER BY created_at,artifact_id LIMIT ?`
		args = []any{createdAt, createdAt, after, limit}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	artifacts := make([]BatesArtifact, 0, len(ids))
	for _, id := range ids {
		artifact, err := loadBatesArtifact(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, nil
}

func (s *Store) BatesArtifactCount(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bates_artifacts`).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting Bates artifacts: %w", err)
	}
	return count, nil
}

func loadBatesArtifactByAllocation(ctx context.Context, q metadataQuerier, allocationID string) (BatesArtifact, error) {
	var id string
	if err := q.QueryRowContext(ctx, `SELECT artifact_id FROM bates_artifacts WHERE allocation_id=?`, allocationID).Scan(&id); err != nil {
		return BatesArtifact{}, err
	}
	return loadBatesArtifact(ctx, q, id)
}

func loadBatesArtifact(ctx context.Context, q metadataQuerier, id string) (BatesArtifact, error) {
	var artifact BatesArtifact
	err := q.QueryRowContext(ctx, `SELECT a.artifact_id,a.allocation_id,a.blob_hash,a.size,a.media_type,a.page_count,a.recipe_json,
		a.manifest_sha256,a.state,a.created_at,l.recipe_sha256 FROM bates_artifacts a JOIN bates_allocations l USING(allocation_id)
		WHERE a.artifact_id=?`, id).Scan(&artifact.ArtifactID, &artifact.AllocationID, &artifact.BlobSHA256, &artifact.Size,
		&artifact.MediaType, &artifact.PageCount, &artifact.RecipeJSON, &artifact.ManifestSHA256, &artifact.State,
		&artifact.CreatedAt, &artifact.RecipeSHA256)
	if err != nil {
		return BatesArtifact{}, err
	}
	rows, err := q.QueryContext(ctx, `SELECT ordinal,occurrence_id,source_blob_sha256,source_page,output_page,label
		FROM bates_artifact_pages WHERE artifact_id=? ORDER BY ordinal`, id)
	if err != nil {
		return BatesArtifact{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var page BatesArtifactPage
		if err := rows.Scan(&page.Ordinal, &page.OccurrenceID, &page.SourceBlobSHA256, &page.SourcePage, &page.OutputPage, &page.Label); err != nil {
			return BatesArtifact{}, err
		}
		artifact.Pages = append(artifact.Pages, page)
	}
	return artifact, rows.Err()
}

func batesArtifactPagesEqual(left, right []BatesArtifactPage) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validateBatesArtifactState(ctx context.Context, q metadataQuerier) error {
	ids, err := pageMetadataKeys(ctx, q, `SELECT artifact_id FROM bates_artifacts ORDER BY artifact_id`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		artifact, err := loadBatesArtifact(ctx, q, id)
		if err != nil {
			return err
		}
		allocation, err := loadBatesAllocation(ctx, q, artifact.AllocationID)
		if err != nil || allocation.State != batesAllocationStateCommitted || artifact.State != batesArtifactStateVerified ||
			artifact.PageCount != len(artifact.Pages) || artifact.RecipeSHA256 != digestCatalogJSON(artifact.RecipeJSON) {
			return fmt.Errorf("validating Bates artifact %s: %w", id, ErrBatesReservationConflict)
		}
		manifestSHA, err := batesArtifactManifestSHA(artifact)
		if err != nil || manifestSHA != artifact.ManifestSHA256 || len(allocation.Labels) != len(artifact.Pages) {
			return fmt.Errorf("validating Bates artifact %s manifest: %w", id, ErrBatesReservationConflict)
		}
		for index, page := range artifact.Pages {
			label := allocation.Labels[index]
			if page.Ordinal != index+1 || page.OccurrenceID != label.OccurrenceID || page.SourcePage != label.SourcePage ||
				page.OutputPage != label.OutputPage || page.Label != label.Label || !canonical.IsSHA256Hex(page.SourceBlobSHA256) {
				return fmt.Errorf("validating Bates artifact %s page %d: %w", id, index+1, ErrBatesPageCountMismatch)
			}
		}
	}
	return nil
}
