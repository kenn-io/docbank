package document

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDocumentPeopleCodec(t *testing.T) {
	value := DocumentPeopleV1{ContractVersion: PersonContractV1,
		ContentVersionID: "00000000-0000-4000-8000-000000000001", Edges: []DocumentPersonEdgeV1{}}
	raw, digest, err := MarshalDocumentPeopleV1(value)
	require.NoError(t, err)
	require.Len(t, digest, 64)
	decoded, again, err := DecodeDocumentPeopleV1(raw)
	require.NoError(t, err)
	require.Equal(t, value, decoded)
	require.Equal(t, digest, again)
	_, _, err = DecodeDocumentPeopleV1(append([]byte(" "), raw...))
	require.Error(t, err)
	value.Edges = []DocumentPersonEdgeV1{{PersonID: "00000000-0000-4000-8000-000000000002",
		Role: "invented", EvidenceKind: "source_metadata", EvidenceID: "claim-a", ClaimCount: 1}}
	_, _, err = MarshalDocumentPeopleV1(value)
	require.Error(t, err)
	require.False(t, bytes.Contains(raw, []byte("invented")))
	_, _, err = DecodeDocumentPeopleV1([]byte(strings.Repeat(" ", 8<<20)))
	require.Error(t, err)
}

func validDocumentPersonEdge() DocumentPersonEdgeV1 {
	return DocumentPersonEdgeV1{
		PersonID: "00000000-0000-4000-8000-000000000002", Role: "author",
		ActorKey: "email:Ada@example.test", EvidenceKind: "source_metadata", EvidenceID: "claim-a",
		Confidence: "exact_identifier", Basis: "identifier_match", RawLabel: "Ada", ClaimCount: 1,
		FirstAxisKey: "2020-01-02T03:04:05.000000000", LastAxisKey: "2020-01-02T03:04:05.000000000",
	}
}

func TestDocumentPeopleCodecRejectsOrderingAndDuplicateKeys(t *testing.T) {
	first := validDocumentPersonEdge()
	second := first
	second.PersonID = "00000000-0000-4000-8000-000000000003"
	value := DocumentPeopleV1{ContractVersion: PersonContractV1,
		ContentVersionID: "00000000-0000-4000-8000-000000000001", Edges: []DocumentPersonEdgeV1{second, first}}
	_, _, err := MarshalDocumentPeopleV1(value)
	require.Error(t, err)
	slices.Reverse(value.Edges)
	_, _, err = MarshalDocumentPeopleV1(value)
	require.NoError(t, err)
	value.Edges = []DocumentPersonEdgeV1{first, first}
	_, _, err = MarshalDocumentPeopleV1(value)
	require.Error(t, err)
}

func TestDocumentPeopleCodecRejectsUnknownMembersAndBounds(t *testing.T) {
	edge := validDocumentPersonEdge()
	value := DocumentPeopleV1{ContractVersion: PersonContractV1,
		ContentVersionID: "00000000-0000-4000-8000-000000000001", Edges: []DocumentPersonEdgeV1{edge}}
	raw, _, err := MarshalDocumentPeopleV1(value)
	require.NoError(t, err)
	unknown := bytes.Replace(raw, []byte(`"edges"`), []byte(`"extra":true,"edges"`), 1)
	_, _, err = DecodeDocumentPeopleV1(unknown)
	require.Error(t, err)

	value.Edges[0].EvidenceID = strings.Repeat("x", MaxPersonEvidenceIDBytes+1)
	_, _, err = MarshalDocumentPeopleV1(value)
	require.Error(t, err)
	value.Edges[0] = edge
	value.Edges[0].RawLabel = strings.Repeat("x", MaxPersonDisplayNameBytes+1)
	_, _, err = MarshalDocumentPeopleV1(value)
	require.Error(t, err)
	value.Edges = make([]DocumentPersonEdgeV1, MaxPersonEdgesPerVersion+1)
	_, _, err = MarshalDocumentPeopleV1(value)
	require.Error(t, err)
}

func TestDocumentPeopleCodecRejectsInvalidDateBounds(t *testing.T) {
	edge := validDocumentPersonEdge()
	value := DocumentPeopleV1{ContractVersion: PersonContractV1,
		ContentVersionID: "00000000-0000-4000-8000-000000000001", Edges: []DocumentPersonEdgeV1{edge}}
	for name, mutate := range map[string]func(*DocumentPersonEdgeV1){
		"missing last": func(edge *DocumentPersonEdgeV1) { edge.LastAxisKey = "" },
		"reverse":      func(edge *DocumentPersonEdgeV1) { edge.LastAxisKey = "2019-01-02T03:04:05.000000000" },
		"timezone":     func(edge *DocumentPersonEdgeV1) { edge.FirstAxisKey = "2020-01-02T03:04:05.000000000Z" },
		"invalid day":  func(edge *DocumentPersonEdgeV1) { edge.FirstAxisKey = "2020-02-31T03:04:05.000000000" },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := value
			mutated.Edges = slices.Clone(value.Edges)
			mutate(&mutated.Edges[0])
			_, _, err := MarshalDocumentPeopleV1(mutated)
			require.Error(t, err)
		})
	}
}
