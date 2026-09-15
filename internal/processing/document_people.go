package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
)

var DocumentPeopleResolverFingerprint = document.PersonResolverFingerprint()

type DocumentPeopleCatalog interface {
	DocumentPeopleResolverInputs(ctx context.Context, versionID string) (store.DocumentPeopleInputs, error)
	PublishDocumentPeople(ctx context.Context, publication store.DocumentPeoplePublication) (store.DocumentPeopleHead, error)
	MarkDocumentPeopleFailed(ctx context.Context, input store.DocumentPeopleInputs, reason string) error
}

type DocumentPeopleBackfillCatalog interface {
	DocumentPeopleCatalog
	MissingDocumentPeopleTargetsAfter(ctx context.Context, fingerprint, after string, limit int) ([]store.DocumentPeopleTarget, error)
	RefreshDocumentPeopleBuilds(ctx context.Context) error
}

type DocumentPeopleBackfillResult struct {
	Published int
	Failed    int
}

func ResolveDocumentPeople(in store.DocumentPeopleInputs) (document.DocumentPeopleV1, store.DocumentPeopleResolution, error) {
	out := document.DocumentPeopleV1{
		ContractVersion: document.PersonContractV1, ContentVersionID: in.ContentVersionID,
		EventGenerationID: in.EventGenerationID, Edges: []document.DocumentPersonEdgeV1{},
	}
	report := store.DocumentPeopleResolution{CandidateOverflow: in.CandidateOverflow}
	actors := in.Actors
	if !in.PeopleAllowed {
		report.SuppressedActors += int64(len(actors))
		actors = nil
	}
	for _, claim := range actors {
		matches := in.Bindings[claim.ActorKey]
		if len(matches) != 1 {
			report.UnresolvedActors++
			continue
		}
		if matches[0].State == "retired" {
			report.SuppressedActors++
			continue
		}
		basis, confidence := "identifier_match", "exact_identifier"
		if strings.HasPrefix(claim.ActorKey, "external_uid:") {
			basis, confidence = "external_uid", "supplied_identity"
		}
		edge := document.DocumentPersonEdgeV1{
			PersonID: matches[0].PersonID, Role: claim.Role, ActorKey: claim.ActorKey,
			EvidenceKind: claim.EvidenceKind, EvidenceID: claim.EvidenceID,
			Confidence: confidence, Basis: basis, RawLabel: claim.DisplayName, ClaimCount: 1,
			FirstAxisKey: claim.AxisKey, LastAxisKey: claim.AxisKey,
			Sensitive: claim.Sensitive || claim.Role == "blind_copy",
		}
		out.Edges = append(out.Edges, edge)
	}
	for _, assignment := range in.Custodians {
		if assignment.PersonID == nil || assignment.RetiredAt != nil {
			continue
		}
		if !in.PeopleAllowed && assignment.Basis == "transfer_record" {
			continue
		}
		person, ok := in.Persons[*assignment.PersonID]
		if !ok || person.State == "retired" {
			continue
		}
		confidence := "supplied_identity"
		if assignment.Basis == "operator_assigned" {
			confidence = "operator_asserted"
		}
		out.Edges = append(out.Edges, document.DocumentPersonEdgeV1{
			PersonID: person.PersonID, Role: "custodian", EvidenceKind: "custodian_assignment",
			EvidenceID: assignment.AssignmentID, Confidence: confidence, Basis: assignment.Basis,
			RawLabel: assignment.RawLabel, ClaimCount: 1,
		})
	}
	for _, assertion := range in.Assertions {
		out.Edges = slices.DeleteFunc(out.Edges, func(edge document.DocumentPersonEdgeV1) bool {
			return edge.PersonID == assertion.PersonID && edge.Role == assertion.Role
		})
		if assertion.Action == "assert" {
			person, ok := in.Persons[assertion.PersonID]
			if !ok || person.State == "retired" {
				continue
			}
			out.Edges = append(out.Edges, document.DocumentPersonEdgeV1{
				PersonID: assertion.PersonID, Role: assertion.Role,
				EvidenceKind: "operator_assertion", EvidenceID: assertion.AssertionID,
				Basis: "operator_assigned", Confidence: "operator_asserted", ClaimCount: 1,
				Sensitive: assertion.Role == "blind_copy",
			})
		}
	}
	key := func(edge document.DocumentPersonEdgeV1) string {
		return strings.Join([]string{edge.PersonID, edge.Role, edge.ActorKey, edge.EvidenceKind, edge.EvidenceID}, "\x00")
	}
	slices.SortFunc(out.Edges, func(a, b document.DocumentPersonEdgeV1) int { return strings.Compare(key(a), key(b)) })
	folded := make([]document.DocumentPersonEdgeV1, 0, len(out.Edges))
	for _, edge := range out.Edges {
		if len(folded) > 0 && key(folded[len(folded)-1]) == key(edge) {
			previous := &folded[len(folded)-1]
			previous.ClaimCount += edge.ClaimCount
			previous.Sensitive = previous.Sensitive || edge.Sensitive
			if edge.FirstAxisKey != "" && (previous.FirstAxisKey == "" || edge.FirstAxisKey < previous.FirstAxisKey) {
				previous.FirstAxisKey = edge.FirstAxisKey
			}
			if edge.LastAxisKey > previous.LastAxisKey {
				previous.LastAxisKey = edge.LastAxisKey
			}
			continue
		}
		folded = append(folded, edge)
	}
	out.Edges = folded
	if len(out.Edges) > document.MaxPersonEdgesPerVersion {
		out.Edges = []document.DocumentPersonEdgeV1{}
		report.OverLimit = true
	}
	return out, report, nil
}

func BackfillDocumentPeopleTargets(ctx context.Context, catalog DocumentPeopleCatalog, targets []store.DocumentPeopleTarget) (DocumentPeopleBackfillResult, error) {
	result := DocumentPeopleBackfillResult{}
	var failures []error
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(append(failures, err)...)
		}
		input, err := catalog.DocumentPeopleResolverInputs(ctx, target.ContentVersionID)
		if err == nil {
			var raw []byte
			raw, err = canonical.Marshal(input)
			if err == nil {
				var people document.DocumentPeopleV1
				var report store.DocumentPeopleResolution
				people, report, err = ResolveDocumentPeople(input)
				if err == nil {
					digest := sha256.Sum256(raw)
					_, err = catalog.PublishDocumentPeople(ctx, store.DocumentPeoplePublication{
						People: people, Resolution: report, InputsSHA256: hex.EncodeToString(digest[:]),
						ResolverFingerprint: DocumentPeopleResolverFingerprint, NodeID: input.NodeID,
						BindingEpoch: input.BindingEpoch, DirtyRevision: input.DirtyRevision,
					})
				}
			}
		}
		if err != nil {
			result.Failed++
			failures = append(failures, fmt.Errorf("people version %s: %w", target.ContentVersionID, err))
			if input.ContentVersionID != "" && !errors.Is(err, store.ErrPeopleInputsChanged) && !errors.Is(err, context.Canceled) {
				reason := "derivation_failed"
				if errors.Is(err, store.ErrPeopleInputsTooLarge) {
					reason = "input_over_limit"
				}
				if markErr := catalog.MarkDocumentPeopleFailed(ctx, input, reason); markErr != nil {
					failures = append(failures, markErr)
				}
			}
		} else {
			result.Published++
		}
	}
	return result, errors.Join(failures...)
}

func NewDocumentPeopleBackfill(
	catalog DocumentPeopleBackfillCatalog,
	mutate func(context.Context, func() error) error,
	logger *slog.Logger,
) *Backfill[store.DocumentPeopleTarget] {
	underGate := func(ctx context.Context, fn func() error) error {
		if mutate == nil {
			return fn()
		}
		return mutate(ctx, fn)
	}
	backfill := &Backfill[store.DocumentPeopleTarget]{
		Name: "document-people", Page: 25, IdleDelay: time.Second,
		Mutate: mutate, Logger: logger,
		Key: func(target store.DocumentPeopleTarget) string { return target.ContentVersionID },
		Process: func(ctx context.Context, target store.DocumentPeopleTarget) error {
			_, err := BackfillDocumentPeopleTargets(ctx, catalog, []store.DocumentPeopleTarget{target})
			return err
		},
	}
	var lastBuildRefresh time.Time
	backfill.List = func(ctx context.Context, after string, limit int) ([]store.DocumentPeopleTarget, error) {
		targets, err := catalog.MissingDocumentPeopleTargetsAfter(
			ctx, DocumentPeopleResolverFingerprint, after, limit,
		)
		now := backfill.now()
		if err == nil && (len(targets) == 0 || now.Sub(lastBuildRefresh) >= time.Second) {
			err = underGate(ctx, func() error { return catalog.RefreshDocumentPeopleBuilds(ctx) })
			if err == nil {
				lastBuildRefresh = now
			}
		}
		return targets, err
	}
	return backfill
}

// RebuildDocumentPeople synchronously invalidates and drains every retained
// version. Restore callers only succeed after the durable receipt completes.
func RebuildDocumentPeople(ctx context.Context, catalog *store.Store) error {
	build, err := catalog.RebuildDocumentPeople(ctx, uuid.NewString())
	if err != nil {
		return err
	}
	backfill := NewDocumentPeopleBackfill(catalog, nil, nil)
	backfill.DrainOnce = true
	if err := backfill.Run(ctx); err != nil {
		return err
	}
	build, err = catalog.DocumentPeopleBuild(ctx, build.OperationID)
	if err != nil {
		return err
	}
	if build.State != "completed" {
		return errors.New("document people rebuild is incomplete")
	}
	return nil
}
