package processing

import (
	"context"
	"errors"
	"fmt"
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
		PeopleAllowed:    true,
		Actors: []store.DocumentEventActorClaim{{
			Role: "sender", ActorKey: "email:ada@example.test",
			EvidenceKind: "email_generation", EvidenceID: "claim-a",
		}},
		Bindings: map[string][]store.Person{
			"email:ada@example.test": {{PersonID: ada, State: "curated"}},
		},
	}
	output, report, err := ResolveDocumentPeople(input)
	require.NoError(t, err)
	require.Len(t, output.Edges, 1)
	require.Equal(t, ada, output.Edges[0].PersonID)
	require.Zero(t, report.UnresolvedActors)

	input.Bindings["email:ada@example.test"] = append(
		input.Bindings["email:ada@example.test"], store.Person{PersonID: grace, State: "curated"})
	output, report, err = ResolveDocumentPeople(input)
	require.NoError(t, err)
	require.Empty(t, output.Edges)
	require.Equal(t, int64(1), report.UnresolvedActors)

	input.PeopleAllowed = false
	output, _, err = ResolveDocumentPeople(input)
	require.NoError(t, err)
	require.Empty(t, output.Edges)
}

type documentPeopleCatalogStub struct {
	inputs      map[string]store.DocumentPeopleInputs
	loadErrs    map[string]error
	publishErrs map[string]error
	published   []string
	marked      []string
}

func (stub *documentPeopleCatalogStub) DocumentPeopleResolverInputs(_ context.Context, version string) (store.DocumentPeopleInputs, error) {
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
			stale:   {ContentVersionID: stale, PeopleAllowed: true},
			success: {ContentVersionID: success, PeopleAllowed: true},
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
		ContentVersionID: version, EventGenerationID: "events", PeopleAllowed: true,
		Actors: []store.DocumentEventActorClaim{
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
	output, _, err := ResolveDocumentPeople(input)
	require.NoError(t, err)
	require.Len(t, output.Edges, 2)
	require.Equal(t, "operator_assertion", output.Edges[0].EvidenceKind)
	require.Equal(t, "external_uid", output.Edges[1].Basis)
	require.Equal(t, "supplied_identity", output.Edges[1].Confidence)

	input.Assertions = nil
	output, _, err = ResolveDocumentPeople(input)
	require.NoError(t, err)
	require.Len(t, output.Edges, 2)
	require.Equal(t, 2, output.Edges[0].ClaimCount)
	require.True(t, output.Edges[0].Sensitive)
	require.Equal(t, "2024-01-01T00:00:00.000000000", output.Edges[0].FirstAxisKey)
	require.Equal(t, "2024-01-03T00:00:00.000000000", output.Edges[0].LastAxisKey)
}

func TestDocumentPeopleResolutionBoundsCombinedEdges(t *testing.T) {
	input := store.DocumentPeopleInputs{
		ContentVersionID: "00000000-0000-4000-8000-000000000021",
		PeopleAllowed:    true,
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
	output, report, err := ResolveDocumentPeople(input)
	require.NoError(t, err)
	require.Empty(t, output.Edges)
	require.True(t, report.OverLimit)
}

func TestDocumentPeopleResolutionDoesNotAssertRetiredPerson(t *testing.T) {
	const version = "00000000-0000-4000-8000-000000000041"
	const retired = "00000000-0000-4000-8000-000000000042"
	input := store.DocumentPeopleInputs{
		ContentVersionID: version,
		PeopleAllowed:    true,
		Persons: map[string]store.Person{
			retired: {PersonID: retired, State: "retired"},
		},
		Assertions: []store.PersonDocumentAssertion{{
			AssertionID: "retired-assertion", PersonID: retired, Role: "author", Action: "assert",
		}},
	}
	output, _, err := ResolveDocumentPeople(input)
	require.NoError(t, err)
	require.Empty(t, output.Edges)
}
