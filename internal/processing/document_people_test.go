package processing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

func TestDocumentPeopleResolution(t *testing.T) {
	const version = "00000000-0000-4000-8000-000000000001"
	const ada = "00000000-0000-4000-8000-000000000002"
	const grace = "00000000-0000-4000-8000-000000000003"
	input := store.DocumentPeopleInputs{
		ContentVersionID: version,
		Actors: []store.DocumentPeopleActor{{
			Role: "sender", ActorKey: "email:ada@example.test",
			EvidenceKind: "email_generation", EvidenceID: "claim-a",
		}},
		Bindings: map[string][]store.Person{
			"email:ada@example.test": {{PersonID: ada, State: "curated"}},
		},
	}
	output, report := ResolveDocumentPeople(input)
	require.Len(t, output.Edges, 1)
	require.Equal(t, ada, output.Edges[0].PersonID)
	require.Zero(t, report.UnresolvedActors)

	input.Bindings["email:ada@example.test"] = append(
		input.Bindings["email:ada@example.test"], store.Person{PersonID: grace, State: "curated"})
	output, report = ResolveDocumentPeople(input)
	require.Empty(t, output.Edges)
	require.Equal(t, int64(1), report.UnresolvedActors)
	input.Actors = append(input.Actors, store.DocumentPeopleActor{Role: "author", DisplayName: "Display only"})
	output, report = ResolveDocumentPeople(input)
	require.Empty(t, output.Edges)
	require.Equal(t, int64(2), report.UnresolvedActors)
}

func TestDocumentPeopleResolutionBoundsKnownActorLabel(t *testing.T) {
	name := strings.Repeat("界", 67)
	input := store.DocumentPeopleInputs{
		ContentVersionID: "00000000-0000-4000-8000-000000000001", EventGenerationID: strings.Repeat("a", 64),
		Actors: []store.DocumentPeopleActor{{
			Role: "sender", ActorKey: "email:ada@example.test", DisplayName: name,
			EvidenceKind: "email_generation", EvidenceID: "claim-a",
		}},
		Bindings: map[string][]store.Person{
			"email:ada@example.test": {{PersonID: "00000000-0000-4000-8000-000000000002", State: "curated"}},
		},
	}
	output, report := ResolveDocumentPeople(input)
	require.Zero(t, report.UnresolvedActors)
	require.Len(t, output.Edges, 1)
	require.Equal(t, strings.Repeat("界", 66), output.Edges[0].RawLabel)
	require.Equal(t, input.Actors[0].ActorKey, output.Edges[0].ActorKey)
	_, _, err := document.MarshalDocumentPeopleV1(output)
	require.NoError(t, err)
	require.Equal(t, name, input.Actors[0].DisplayName)
}

func TestRebuildDocumentPeopleDrainsMultipleBatches(t *testing.T) {
	catalog := openDocumentEventTestStore(t)
	var versions []string
	for i := range 101 {
		name := fmt.Sprintf("people-%d.txt", i)
		file, err := catalog.CreateFile(t.Context(), catalog.RootID(), name, testDigest(name), 8, "text/plain")
		require.NoError(t, err)
		versions = append(versions, file.CurrentVersionID)
	}
	require.NoError(t, RebuildDocumentEvents(t.Context(), catalog))
	require.NoError(t, RebuildDocumentPeople(t.Context(), catalog))
	for _, version := range versions {
		_, head, err := catalog.DocumentPeopleForVersion(t.Context(), version)
		require.NoError(t, err)
		require.Equal(t, "published", head.State)
	}
}

func TestRebuildDocumentPeopleAcceptsUnavailableInputs(t *testing.T) {
	catalog := openDocumentEventTestStore(t)
	file, err := catalog.CreateFile(t.Context(), catalog.RootID(), "bounded.txt", testDigest("bounded"), 8, "text/plain")
	require.NoError(t, err)
	for i := range document.MaxPersonEdgesPerVersion + 2 {
		_, err := catalog.SetCustodian(t.Context(), store.CustodianRequest{
			Scope:    store.CustodianScope{Kind: "document", NodeID: file.ID, ContentVersionID: file.CurrentVersionID},
			RawLabel: fmt.Sprintf("Synthetic custodian %d", i), Rank: "additional", Basis: "operator_assigned", IfMatchRevision: 1,
		})
		require.NoError(t, err)
	}
	other, err := catalog.CreateFile(t.Context(), catalog.RootID(), "ordinary.txt", testDigest("ordinary"), 8, "text/plain")
	require.NoError(t, err)
	require.NoError(t, RebuildDocumentEvents(t.Context(), catalog))
	_, err = catalog.PrepareDocumentPeopleInputs(t.Context(), file.CurrentVersionID)
	require.ErrorIs(t, err, store.ErrPeopleInputsTooLarge)
	require.NoError(t, RebuildDocumentPeople(t.Context(), catalog))
	_, head, err := catalog.DocumentPeopleForVersion(t.Context(), file.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, "unavailable", head.State)
	require.Equal(t, "input_over_limit", head.FailureReason)
	_, head, err = catalog.DocumentPeopleForVersion(t.Context(), other.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, "published", head.State)
}

type documentPeopleCatalogStub struct {
	inputs      map[string]store.DocumentPeopleInputs
	loadErrs    map[string]error
	publishErrs map[string]error
	published   []string
	marked      []string
}

func (stub *documentPeopleCatalogStub) PrepareDocumentPeopleInputs(_ context.Context, version string) (store.DocumentPeopleInputs, error) {
	return stub.inputs[version], stub.loadErrs[version]
}

func (stub *documentPeopleCatalogStub) PublishDocumentPeople(_ context.Context, publication store.DocumentPeoplePublication) (store.DocumentPeopleHead, error) {
	version := publication.People.ContentVersionID
	stub.published = append(stub.published, version)
	return store.DocumentPeopleHead{}, stub.publishErrs[version]
}

func (stub *documentPeopleCatalogStub) MarkDocumentPeopleFailed(_ context.Context, input store.DocumentPeopleInputs, reason string) error {
	stub.marked = append(stub.marked, input.ContentVersionID+":"+reason)
	return nil
}

func TestBackfillDocumentPeopleTargetsContinuesAndPreservesStaleRetry(t *testing.T) {
	const loadFailure = "00000000-0000-4000-8000-000000000031"
	const stale = "00000000-0000-4000-8000-000000000032"
	const success = "00000000-0000-4000-8000-000000000033"
	stub := &documentPeopleCatalogStub{
		inputs: map[string]store.DocumentPeopleInputs{
			stale:   {ContentVersionID: stale},
			success: {ContentVersionID: success},
		},
		loadErrs:    map[string]error{loadFailure: errors.New("synthetic load failure")},
		publishErrs: map[string]error{stale: store.ErrPeopleInputsChanged},
	}
	targets := []store.DocumentPeopleTarget{{ContentVersionID: loadFailure}, {ContentVersionID: stale}, {ContentVersionID: success}}
	result, err := BackfillDocumentPeopleTargets(t.Context(), stub, targets)
	require.Error(t, err)
	require.Equal(t, DocumentPeopleBackfillResult{Published: 1, Failed: 2}, result)
	require.Equal(t, []string{stale, success}, stub.published)
	require.Empty(t, stub.marked, "load failures have no complete fence and stale publications stay retryable")
}

func TestDocumentPeopleResolutionAppliesEvidencePrivacyAndAssertions(t *testing.T) {
	const version = "00000000-0000-4000-8000-000000000011"
	const ada = "00000000-0000-4000-8000-000000000012"
	const grace = "00000000-0000-4000-8000-000000000013"
	input := store.DocumentPeopleInputs{
		ContentVersionID: version, EventGenerationID: "events",
		Actors: []store.DocumentPeopleActor{
			{Role: "blind_copy", ActorKey: "email:ada@example.test", DisplayName: "Ada", EvidenceKind: "email_generation", EvidenceID: "a", AxisKey: "2024-01-03T00:00:00.000000000"},
			{Role: "blind_copy", ActorKey: "email:ada@example.test", DisplayName: "Ada", EvidenceKind: "email_generation", EvidenceID: "a", AxisKey: "2024-01-01T00:00:00.000000000"},
			{Role: "sender", ActorKey: "external_uid:key", EvidenceKind: "transfer_record", EvidenceID: "b"},
		},
		Bindings: map[string][]store.Person{
			"email:ada@example.test": {{PersonID: ada, State: "curated"}},
			"external_uid:key":       {{PersonID: grace, State: "curated"}},
		},
		Persons: map[string]store.Person{
			ada: {PersonID: ada, State: "curated"},
		},
		Assertions: []store.PersonDocumentAssertion{
			{AssertionID: "assert-a", PersonID: ada, Role: "blind_copy", Action: "suppress"},
			{AssertionID: "assert-b", PersonID: ada, Role: "author", Action: "assert"},
		},
	}
	output, _ := ResolveDocumentPeople(input)
	require.Len(t, output.Edges, 2)
	require.Equal(t, "operator_assertion", output.Edges[0].EvidenceKind)
	require.Equal(t, "external_uid", output.Edges[1].Basis)
	require.Equal(t, "supplied_identity", output.Edges[1].Confidence)

	input.Assertions = nil
	output, _ = ResolveDocumentPeople(input)
	require.Len(t, output.Edges, 2)
	require.Equal(t, 2, output.Edges[0].ClaimCount)
	require.True(t, output.Edges[0].Sensitive)
	require.Equal(t, "2024-01-01T00:00:00.000000000", output.Edges[0].FirstAxisKey)
	require.Equal(t, "2024-01-03T00:00:00.000000000", output.Edges[0].LastAxisKey)
}

func TestDocumentPeopleResolutionBoundsCombinedEdges(t *testing.T) {
	input := store.DocumentPeopleInputs{
		ContentVersionID: "00000000-0000-4000-8000-000000000021",
		Persons:          map[string]store.Person{},
	}
	for index := 0; index <= document.MaxPersonEdgesPerVersion; index++ {
		personID := fmt.Sprintf("00000000-0000-4000-8000-%012x", index+1)
		input.Persons[personID] = store.Person{PersonID: personID, State: "curated"}
		input.Custodians = append(input.Custodians, store.CustodianAssignment{
			AssignmentID: fmt.Sprintf("assignment-%04d", index), PersonID: &personID,
			RawLabel: "Synthetic custodian", Basis: "operator_assigned",
		})
	}
	output, report := ResolveDocumentPeople(input)
	require.Empty(t, output.Edges)
	require.True(t, report.OverLimit)
}

func TestDocumentPeopleResolutionDoesNotAssertRetiredPerson(t *testing.T) {
	const version = "00000000-0000-4000-8000-000000000041"
	const retired = "00000000-0000-4000-8000-000000000042"
	input := store.DocumentPeopleInputs{
		ContentVersionID: version,
		Persons: map[string]store.Person{
			retired: {PersonID: retired, State: "retired"},
		},
		Assertions: []store.PersonDocumentAssertion{{
			AssertionID: "retired-assertion", PersonID: retired, Role: "author", Action: "assert",
		}},
	}
	output, _ := ResolveDocumentPeople(input)
	require.Empty(t, output.Edges)
}
