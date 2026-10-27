package processing

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"uuid"

	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
)

func (s *Service) ReserveNumberedPDF(ctx context.Context, r NumberedReservationRequest,
	runtime *pagerender.Runtime,
) (result NumberedReservation, resultErr error) {
	if !r.RetainOriginal || r.StartAt < 1 || canonical.ValidateUUIDv4(r.NamespaceID.String()) != nil {
		return result, ErrRedactionInvalidRequest
	}
	if s == nil {
		return result, ErrRedactionPreparationUnavailable
	}
	review, err := BuildRedactionReview(r.Preparation, r.Masks, r.Preparation.Selection.Revision)
	if err != nil {
		return result, err
	}
	if review.SHA256 != r.ReviewSHA256 {
		return result, ErrRedactionReviewChanged
	}
	ns, err := s.catalog.BatesNamespace(ctx, r.NamespaceID.String(), "", "", 0)
	if errors.Is(err, store.ErrNotFound) {
		return result, ErrNumberedNamespaceNotFound
	}
	if err != nil {
		return result, err
	}
	recipe, err := BuildNumberedRecipe(uuid.MustParse(s.catalog.VaultID()), ns, r.StartAt, review)
	if err != nil {
		return result, err
	}
	request, err := recipe.PlanRequest(uuid.Nil())
	if err != nil {
		return result, err
	}
	prior, err := s.catalog.ReplaySnapshotBatesRange(ctx, request)
	if err == nil {
		return s.numberedReservation(ctx, recipe, prior)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return result, err
	}
	if runtime == nil || !pagerender.SupportedPlatform() {
		return result, ErrRedactionPreparationUnavailable
	}
	op, err := s.beginNativePDFOperation(ctx, "reserve")
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, op.Close()) }()
	admitted, err := s.freshRedactionReview(op.ctx, op, RedactionResolveRequest{Preparation: r.Preparation, Masks: r.Masks}, runtime)
	if err != nil {
		return result, err
	}
	defer clear(admitted.data)
	if err := recipe.MatchReview(admitted.review); err != nil {
		return result, err
	}
	snapshot, err := recipe.snapshot(uuid.New())
	if err != nil {
		return result, err
	}
	request.SnapshotID, request.OperationID = snapshot.SnapshotID, uuid.New().String()
	var allocation store.BatesAllocation
	err = s.gate.MutateContext(op.ctx, func() error {
		var err error
		allocation, err = s.catalog.ReserveSnapshotBatesRange(op.ctx, snapshot, request)
		return err
	})
	if err != nil {
		return result, err
	}
	return s.numberedReservation(op.ctx, recipe, allocation)
}

func (r NumberedRecipe) snapshot(id uuid.UUID) (store.SnapshotSealRequest, error) {
	source := r.Selection.Source
	occurrence, err := r.occurrence()
	if err != nil {
		return store.SnapshotSealRequest{}, err
	}
	pages := make([]int, r.PageCount)
	for i := range pages {
		pages[i] = i + 1
	}
	return store.SnapshotSealRequest{SnapshotID: id.String(), Members: []store.CollectionSnapshotMember{{
		Ordinal: 1, OccurrenceID: occurrence, NodeID: r.Selection.NodeID, ContentVersionID: source.VersionID,
		BlobSHA256: source.SHA256, Size: source.Size, FamilyID: occurrence, FamilyOrder: 1, DisplayName: "original.pdf",
		FrozenFieldsJSON: "{}", DocumentKind: "other", SelectedSourcePages: pages, SelectedPDFSHA256: source.SHA256, SourcePageCount: r.PageCount,
		Representations: []store.CollectionSnapshotRepresentation{{OccurrenceID: occurrence, Role: "native", Status: "available", TextAuthority: "none",
			ContentVersionID: source.VersionID, BlobSHA256: source.SHA256, MediaType: "application/pdf", Size: source.Size, VerifiedPageCount: r.PageCount}},
	}}}, nil
}

func (s *Service) numberedReservation(ctx context.Context, recipe NumberedRecipe,
	a store.BatesAllocation,
) (NumberedReservation, error) {
	allocation, pages, err := s.catalog.BatesPublicationPlan(ctx, a.AllocationID)
	if err != nil {
		return NumberedReservation{}, err
	}
	result := NumberedReservation{Contract: "native-pdf-numbered-reservation/v1", Recipe: recipe, RecipeSHA256: a.RecipeSHA256,
		AllocationID: uuid.MustParse(a.AllocationID), SnapshotID: uuid.MustParse(a.SnapshotID), RequestSHA256: a.RequestSHA256, State: allocation.State, Pages: pages}
	if err := ValidateNumberedReservation(result, nil); err != nil {
		return result, err
	}
	return result, s.checkNumberedSnapshot(ctx, result)
}

func (s *Service) checkNumberedSnapshot(ctx context.Context, r NumberedReservation) error {
	header, err := s.catalog.CollectionSnapshot(ctx, r.SnapshotID.String())
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRedactionPreparationIntegrity, err)
	}
	members, err := s.catalog.SnapshotMembers(ctx, r.SnapshotID.String(), 0, 2)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRedactionPreparationIntegrity, err)
	}
	snapshot, err := r.Recipe.snapshot(r.SnapshotID)
	if err != nil {
		return err
	}
	expected := snapshot.Members[0]
	if header.MemberCount != 1 || header.PageCount != r.Recipe.PageCount || len(members) != 1 {
		return ErrRedactionPreparationIntegrity
	}
	actual := members[0]
	// Names and provenance are frozen metadata, not source authority.
	if actual.NodeID != expected.NodeID || actual.ContentVersionID != expected.ContentVersionID || actual.BlobSHA256 != expected.BlobSHA256 ||
		actual.Size != expected.Size || actual.OccurrenceID != expected.OccurrenceID || actual.SelectedPDFSHA256 != expected.SelectedPDFSHA256 ||
		actual.SourcePageCount != expected.SourcePageCount || !reflect.DeepEqual(actual.SelectedSourcePages, expected.SelectedSourcePages) ||
		!reflect.DeepEqual(actual.Representations, expected.Representations) {
		return ErrRedactionPreparationIntegrity
	}
	return nil
}

func (s *Service) captureNumberedSource(ctx context.Context, r NumberedReservation) ([]byte, error) {
	var data []byte
	err := s.gate.CaptureContext(ctx, func() error {
		if err := s.checkNumberedSnapshot(ctx, r); err != nil {
			return err
		}
		var err error
		data, err = s.readNativePDFBlob(ctx, r.Recipe.Selection.Source)
		return err
	})
	return data, err
}
