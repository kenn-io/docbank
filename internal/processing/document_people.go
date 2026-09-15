package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
)

var DocumentPeopleResolverFingerprint = document.PersonResolverFingerprint()

type DocumentPeopleCatalog interface {
	PrepareDocumentPeopleInputs(ctx context.Context, versionID string) (store.DocumentPeopleInputs, error)
	PublishDocumentPeople(ctx context.Context, publication store.DocumentPeoplePublication) (store.DocumentPeopleHead, error)
	MarkDocumentPeopleFailed(ctx context.Context, input store.DocumentPeopleInputs, reason string) error
}

type DocumentPeopleBackfillResult struct {
	Published int
	Failed    int
}

func ResolveDocumentPeople(in store.DocumentPeopleInputs) (document.DocumentPeopleV1, store.DocumentPeopleResolution) {
	out := document.DocumentPeopleV1{
		ContractVersion: document.PersonContractV1, ContentVersionID: in.ContentVersionID,
		EventGenerationID: in.EventGenerationID, Edges: []document.DocumentPersonEdgeV1{},
	}
	report := store.DocumentPeopleResolution{CandidateOverflow: in.CandidateOverflow}
	for _, claim := range in.Actors {
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
			Confidence: confidence, Basis: basis, RawLabel: claim.DisplayLabel(), ClaimCount: 1,
			FirstAxisKey: claim.AxisKey, LastAxisKey: claim.AxisKey,
			Sensitive: claim.Sensitive || claim.Role == "blind_copy",
		}
		out.Edges = append(out.Edges, edge)
	}
	for _, assignment := range in.Custodians {
		if assignment.PersonID == nil || assignment.RetiredAt != nil {
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
	return out, report
}

func BackfillDocumentPeopleTargets(ctx context.Context, catalog DocumentPeopleCatalog, targets []store.DocumentPeopleTarget) (DocumentPeopleBackfillResult, error) {
	result := DocumentPeopleBackfillResult{}
	var failures []error
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(append(failures, err)...)
		}
		if err := backfillDocumentPeopleTarget(ctx, catalog, target.ContentVersionID); err != nil {
			result.Failed++
			failures = append(failures, fmt.Errorf("people version %s: %w", target.ContentVersionID, err))
		} else {
			result.Published++
		}
	}
	return result, errors.Join(failures...)
}

func backfillDocumentPeopleTarget(ctx context.Context, catalog DocumentPeopleCatalog, versionID string) error {
	input, err := catalog.PrepareDocumentPeopleInputs(ctx, versionID)
	if err == nil {
		var raw []byte
		raw, err = canonical.Marshal(input)
		if err == nil {
			people, report := ResolveDocumentPeople(input)
			digest := sha256.Sum256(raw)
			_, err = catalog.PublishDocumentPeople(ctx, store.DocumentPeoplePublication{
				People: people, Resolution: report, InputsSHA256: hex.EncodeToString(digest[:]),
				ResolverFingerprint: DocumentPeopleResolverFingerprint, NodeID: input.NodeID,
				BindingEpoch: input.BindingEpoch, NodeRevision: input.NodeRevision,
			})
		}
	}
	if err == nil || input.ContentVersionID == "" || errors.Is(err, store.ErrPeopleInputsChanged) || errors.Is(err, context.Canceled) {
		return err
	}
	reason := "derivation_failed"
	if errors.Is(err, store.ErrPeopleInputsTooLarge) {
		reason = "input_over_limit"
	}
	return errors.Join(err, catalog.MarkDocumentPeopleFailed(ctx, input, reason))
}

// RebuildDocumentPeople rebuilds missing derived attribution after metadata import.
func RebuildDocumentPeople(ctx context.Context, catalog *store.Store) error {
	cursor := ""
	for {
		targets, err := catalog.MissingDocumentPeopleTargetsAfter(ctx, document.PersonResolverFingerprint(), cursor, 100)
		if err != nil {
			return err
		}
		if len(targets) == 0 {
			return nil
		}
		if _, err := BackfillDocumentPeopleTargets(ctx, catalog, targets); err != nil {
			return err
		}
		cursor = targets[len(targets)-1].ContentVersionID
	}
}
