package api

import (
	"context"
	"fmt"

	"go.kenn.io/docbank/internal/pdfstamp"
	"go.kenn.io/docbank/internal/store"
)

type BatesNamespaceRequest struct {
	Prefix  string `json:"prefix"`
	Suffix  string `json:"suffix,omitzero"`
	Padding int    `json:"padding"`
}

type BatesNamespace struct {
	NamespaceID string `json:"namespace_id"`
	Prefix      string `json:"prefix"`
	Suffix      string `json:"suffix"`
	Padding     int    `json:"padding"`
	CreatedAt   string `json:"created_at"`
}

type BatesNamespacePage struct {
	Items      []BatesNamespace `json:"items"`
	Total      int              `json:"total"`
	NextCursor string           `json:"next_cursor,omitzero"`
}

type BatesPageInput struct {
	OccurrenceID      string `json:"occurrence_id"`
	UnstampedSHA256   string `json:"unstamped_sha256"`
	SourcePage        int    `json:"source_page"`
	VerifiedPageCount int    `json:"verified_page_count"`
}

type BatesPageLabel struct {
	Ordinal      int    `json:"ordinal"`
	OccurrenceID string `json:"occurrence_id"`
	SourcePage   int    `json:"source_page"`
	OutputPage   int    `json:"output_page"`
	Label        string `json:"label"`
}

// BatesPlanRequest previews labels. It never reserves, so it carries no
// operation ID or recipe.
type BatesPlanRequest struct {
	NamespaceID string           `json:"namespace_id,omitzero"`
	SnapshotID  string           `json:"snapshot_id"`
	Prefix      string           `json:"prefix,omitzero"`
	Suffix      string           `json:"suffix,omitzero"`
	Padding     int              `json:"padding,omitzero"`
	StartAt     int64            `json:"start_at" minimum:"0" doc:"First Bates number; 0 continues the namespace cursor."`
	Pages       []BatesPageInput `json:"pages,omitempty"`
}

// BatesReserveRequest reserves the range named by one reviewed stamp recipe.
// The recipe fixes the namespace, the first number, and the digest that
// publication must later match, so callers never send them separately.
type BatesReserveRequest struct {
	OperationID string           `json:"operation_id" format:"uuid"`
	SnapshotID  string           `json:"snapshot_id" format:"uuid"`
	Recipe      pdfstamp.Recipe  `json:"recipe"`
	Pages       []BatesPageInput `json:"pages,omitempty"`
}

// BindBatesReservation derives a store reservation from the recipe and
// requires the recipe's label format to match its namespace exactly.
func BindBatesReservation(ctx context.Context, s *store.Store, r BatesReserveRequest) (store.BatesPlanRequest, error) {
	if len(r.Pages) > store.MaxBatesExportPages {
		return store.BatesPlanRequest{}, store.ErrBatesPageLimit
	}
	recipe := r.Recipe.Normalized()
	if err := recipe.Validate(); err != nil {
		return store.BatesPlanRequest{}, fmt.Errorf("%w: %w", store.ErrInvalidBatesRequest, err)
	}
	if recipe.Restamp {
		return store.BatesPlanRequest{}, fmt.Errorf("%w: Bates exports cannot restamp source PDFs", store.ErrInvalidBatesRequest)
	}
	digest, err := recipe.SHA256()
	if err != nil {
		return store.BatesPlanRequest{}, fmt.Errorf("%w: %w", store.ErrInvalidBatesRequest, err)
	}
	namespace, err := s.BatesNamespace(ctx, recipe.NamespaceID, "", "", 0)
	if err != nil {
		return store.BatesPlanRequest{}, err
	}
	if namespace.Prefix != recipe.Prefix || namespace.Suffix != recipe.Suffix || namespace.Padding != recipe.Padding {
		return store.BatesPlanRequest{}, fmt.Errorf("%w: recipe prefix, suffix, or padding differs from namespace %s",
			store.ErrInvalidBatesRequest, namespace.NamespaceID)
	}
	request := store.BatesPlanRequest{OperationID: r.OperationID, NamespaceID: namespace.NamespaceID,
		SnapshotID: r.SnapshotID, RecipeSHA256: digest, StartAt: int64(recipe.StartAt), Pages: BatesPagesStore(r.Pages)}
	if len(request.Pages) == 0 {
		request.Pages, err = s.SnapshotBatesPages(ctx, r.SnapshotID)
		if err != nil {
			return store.BatesPlanRequest{}, err
		}
	}
	return request, nil
}

type BatesPlan struct {
	AllocationID   string           `json:"allocation_id,omitzero"`
	Namespace      BatesNamespace   `json:"namespace"`
	StartSequence  int64            `json:"start_sequence"`
	EndSequence    int64            `json:"end_sequence"`
	Labels         []BatesPageLabel `json:"labels"`
	StampedNothing bool             `json:"stamped_nothing"`
}

type BatesAllocation struct {
	AllocationID  string           `json:"allocation_id"`
	NamespaceID   string           `json:"namespace_id"`
	SnapshotID    string           `json:"snapshot_id"`
	RecipeSHA256  string           `json:"recipe_sha256"`
	State         string           `json:"state"`
	StartSequence int64            `json:"start_sequence"`
	EndSequence   int64            `json:"end_sequence"`
	Labels        []BatesPageLabel `json:"labels"`
	CreatedAt     string           `json:"created_at"`
	CommittedAt   string           `json:"committed_at,omitzero"`
}

func batesNamespaceDTO(value store.BatesNamespace) BatesNamespace {
	return BatesNamespace{value.NamespaceID, value.Prefix, value.Suffix, value.Padding, value.CreatedAt}
}

func batesLabelsDTO(values []store.BatesPageLabel) []BatesPageLabel {
	result := make([]BatesPageLabel, len(values))
	for i, value := range values {
		result[i] = BatesPageLabel{value.Ordinal, value.OccurrenceID, value.SourcePage, value.OutputPage, value.Label}
	}
	return result
}

// BatesPagesStore converts caller-supplied page inputs; nil stays nil so the
// store reads the sealed page order instead.
func BatesPagesStore(values []BatesPageInput) []store.BatesPageInput {
	if len(values) == 0 {
		return nil
	}
	pages := make([]store.BatesPageInput, len(values))
	for i, page := range values {
		pages[i] = store.BatesPageInput{OccurrenceID: page.OccurrenceID, UnstampedSHA256: page.UnstampedSHA256,
			SourcePage: page.SourcePage, VerifiedPageCount: page.VerifiedPageCount}
	}
	return pages
}

func batesPlanRequestStore(value BatesPlanRequest) store.BatesPlanRequest {
	return store.BatesPlanRequest{NamespaceID: value.NamespaceID, SnapshotID: value.SnapshotID,
		StartAt: value.StartAt, Pages: BatesPagesStore(value.Pages)}
}

func batesAllocationDTO(value store.BatesAllocation) BatesAllocation {
	return BatesAllocation{AllocationID: value.AllocationID, NamespaceID: value.NamespaceID,
		SnapshotID: value.SnapshotID, RecipeSHA256: value.RecipeSHA256, State: value.State,
		StartSequence: value.StartSequence, EndSequence: value.EndSequence, Labels: batesLabelsDTO(value.Labels),
		CreatedAt: value.CreatedAt, CommittedAt: value.CommittedAt}
}

type BatesExportRequest struct {
	AllocationID string          `json:"allocation_id" format:"uuid"`
	Recipe       pdfstamp.Recipe `json:"recipe"`
}

type BatesArtifactPage struct {
	Ordinal          int    `json:"ordinal"`
	OccurrenceID     string `json:"occurrence_id"`
	SourceBlobSHA256 string `json:"source_blob_sha256"`
	SourcePage       int    `json:"source_page"`
	OutputPage       int    `json:"output_page"`
	Label            string `json:"label"`
}

type BatesExport struct {
	ArtifactID     string              `json:"artifact_id"`
	AllocationID   string              `json:"allocation_id"`
	BlobSHA256     string              `json:"blob_sha256"`
	Size           int64               `json:"size"`
	MediaType      string              `json:"media_type"`
	PageCount      int                 `json:"page_count"`
	RecipeSHA256   string              `json:"recipe_sha256"`
	ManifestSHA256 string              `json:"manifest_sha256"`
	State          string              `json:"state"`
	CreatedAt      string              `json:"created_at"`
	Pages          []BatesArtifactPage `json:"pages"`
}

type BatesExportPage struct {
	Items     []BatesExport `json:"items"`
	Total     int           `json:"total"`
	NextAfter string        `json:"next_after,omitzero"`
}

type BatesCandidateEvidence struct {
	Kind          string `json:"kind"`
	OccurrenceID  string `json:"occurrence_id"`
	Label         string `json:"label,omitzero"`
	OutputPage    int    `json:"output_page,omitzero"`
	AssignmentID  string `json:"assignment_id,omitzero"`
	ScopeKind     string `json:"scope_kind,omitzero"`
	RawLabel      string `json:"raw_label,omitzero"`
	PersonID      string `json:"person_id,omitzero"`
	Rank          string `json:"rank,omitzero"`
	Basis         string `json:"basis,omitzero"`
	PackageID     string `json:"package_id,omitzero"`
	PackageRecord string `json:"package_record_id,omitzero"`
}

type BatesCandidate struct {
	ArtifactID        string                   `json:"artifact_id"`
	AllocationID      string                   `json:"allocation_id"`
	SnapshotID        string                   `json:"snapshot_id"`
	BlobSHA256        string                   `json:"blob_sha256"`
	Size              int64                    `json:"size"`
	MediaType         string                   `json:"media_type"`
	PageCount         int                      `json:"page_count"`
	ManifestSHA256    string                   `json:"manifest_sha256"`
	State             string                   `json:"state"`
	CreatedAt         string                   `json:"created_at"`
	Evidence          []BatesCandidateEvidence `json:"evidence"`
	EvidenceTruncated bool                     `json:"evidence_truncated"`
	EvidenceCursor    string                   `json:"evidence_cursor,omitzero"`
}

type BatesCandidatePage struct {
	Items      []BatesCandidate `json:"items"`
	NextCursor string           `json:"next_cursor,omitzero"`
}

type BatesDownloadTicket struct {
	URL          string `json:"url"`
	Name         string `json:"name"`
	AllocationID string `json:"allocation_id"`
	BlobSHA256   string `json:"blob_sha256"`
	Size         int64  `json:"size"`
}

func batesExportDTO(value store.BatesArtifact) BatesExport {
	pages := make([]BatesArtifactPage, len(value.Pages))
	for index, page := range value.Pages {
		pages[index] = BatesArtifactPage{Ordinal: page.Ordinal, OccurrenceID: page.OccurrenceID,
			SourceBlobSHA256: page.SourceBlobSHA256, SourcePage: page.SourcePage,
			OutputPage: page.OutputPage, Label: page.Label}
	}
	return BatesExport{ArtifactID: value.ArtifactID, AllocationID: value.AllocationID,
		BlobSHA256: value.BlobSHA256, Size: value.Size, MediaType: value.MediaType,
		PageCount: value.PageCount, RecipeSHA256: value.RecipeSHA256,
		ManifestSHA256: value.ManifestSHA256, State: value.State, CreatedAt: value.CreatedAt, Pages: pages}
}
