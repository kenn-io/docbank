package document

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDocumentPeoplePublicationContract(t *testing.T) {
	value := DocumentPeopleV1{
		ContractVersion: PersonContractV1, ContentVersionID: "00000000-0000-4000-8000-000000000001",
		EventGenerationID: strings.Repeat("a", 64),
		Edges: []DocumentPersonEdgeV1{{
			PersonID: "00000000-0000-4000-8000-000000000002", Role: "author",
			ActorKey: "email:ada@example.test", EvidenceKind: "source_metadata", EvidenceID: "claim-a",
			Confidence: "exact_identifier", Basis: "identifier_match", RawLabel: "Ada", ClaimCount: 1,
		}},
	}
	raw, digest, err := MarshalDocumentPeopleV1(value)
	require.NoError(t, err)
	decoded, again, err := DecodeDocumentPeopleV1(raw)
	require.NoError(t, err)
	require.Equal(t, value, decoded)
	require.Equal(t, digest, again)
	_, _, err = DecodeDocumentPeopleV1(append([]byte(" "), raw...))
	require.Error(t, err)
	for name, mutate := range map[string]func(*DocumentPeopleV1){
		"unknown role":   func(v *DocumentPeopleV1) { v.Edges[0].Role = "invented" },
		"duplicate edge": func(v *DocumentPeopleV1) { v.Edges = append(v.Edges, v.Edges[0]) },
		"unpaired date":  func(v *DocumentPeopleV1) { v.Edges[0].FirstAxisKey = "2024-01-01T00:00:00.000000000" },
		"invalid date": func(v *DocumentPeopleV1) {
			v.Edges[0].FirstAxisKey = "2024-02-31T00:00:00.000000000"
			v.Edges[0].LastAxisKey = v.Edges[0].FirstAxisKey
		},
		"label bound": func(v *DocumentPeopleV1) { v.Edges[0].RawLabel = strings.Repeat("x", MaxPersonDisplayNameBytes+1) },
		"edge bound":  func(v *DocumentPeopleV1) { v.Edges = make([]DocumentPersonEdgeV1, MaxPersonEdgesPerVersion+1) },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := value
			invalid.Edges = slices.Clone(value.Edges)
			mutate(&invalid)
			_, _, err := MarshalDocumentPeopleV1(invalid)
			require.Error(t, err)
		})
	}
}
