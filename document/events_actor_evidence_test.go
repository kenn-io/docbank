package document

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDocumentEventActorEvidenceKeepsLegacyCanonicalBytesDecodable(t *testing.T) {
	record := validDocumentEvents()
	record.Events[0].Actors = []DocumentEventActorV1{{
		ActorKey: "email:ada@example.test", Address: "ada@example.test",
		Claim: "Ada <ada@example.test>", DisplayName: "Ada", Ordinal: 0, Role: "author",
	}}
	legacy, _, err := MarshalDocumentEventsV1(record)
	require.NoError(t, err)
	require.NotContains(t, string(legacy), `"display_name":"Ada","evidence_id"`,
		"an absent actor evidence pair must retain the old canonical representation")
	decoded, _, err := DecodeDocumentEventsV1(legacy)
	require.NoError(t, err)
	require.Equal(t, record, decoded)
}

func TestDocumentEventActorEvidenceUsesTheEventVocabulary(t *testing.T) {
	record := validDocumentEvents()
	record.Events[0].Actors = []DocumentEventActorV1{{
		ActorKey: "email:ada@example.test", Address: "ada@example.test",
		Claim: "Ada <ada@example.test>", DisplayName: "Ada", Ordinal: 0, Role: "author",
		EvidenceKind: "operator_assertion", EvidenceID: "assertion-a",
	}}
	_, _, err := MarshalDocumentEventsV1(record)
	require.ErrorContains(t, err, "actor evidence")
}

func TestDocumentEventActorEvidenceRequiresRetainedSource(t *testing.T) {
	record := validDocumentEvents()
	record.Events[0].Actors = []DocumentEventActorV1{{
		ActorKey: "email:ada@example.test", Address: "ada@example.test",
		Claim: "Ada <ada@example.test>", DisplayName: "Ada", Ordinal: 0, Role: "author",
		EvidenceKind: "source_metadata", EvidenceID: "metadata/missing",
	}}
	_, _, err := MarshalDocumentEventsV1(record)
	require.ErrorContains(t, err, "actor evidence")
}
