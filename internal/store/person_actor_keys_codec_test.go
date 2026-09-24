package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPersonActorKeysAreAcceptedByDocumentEvents(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	person, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	_, err = s.AddPersonIdentity(t.Context(), person.PersonID, person.Revision, PersonIdentity{
		Kind: "handle", ValueDisplay: "chat/user-a", ScopeKind: "workspace", ScopeValue: "synthetic",
		Origin: "operator", EvidenceKind: "operator_assertion", EvidenceID: "handle-claim", Confidence: "operator_asserted",
	})
	require.NoError(t, err)
	person, _, err = s.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err)
	_, err = s.LinkExternalIdentity(t.Context(), PersonExternalIdentity{
		PersonID: person.PersonID, System: "msgvault", ArchiveID: "synthetic", UID: "person-a", UIDKind: "vcard_uid", UIDState: "current",
	}, person.Revision)
	require.NoError(t, err)
	page, err := s.ActorKeysForPerson(t.Context(), PersonActorKeysRequest{PersonID: person.PersonID})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	for _, key := range page.Items {
		t.Run(strings.SplitN(key, ":", 2)[0], func(t *testing.T) {
			record := documentEventRecord(t, s.VaultID(), "synthetic-version", "a")
			record.Events[0].Actors[0].ActorKey = key
			raw, _, err := document.MarshalDocumentEventsV1(record)
			require.NoError(t, err)
			decoded, _, err := document.DecodeDocumentEventsV1(raw)
			require.NoError(t, err)
			require.Equal(t, key, decoded.Events[0].Actors[0].ActorKey)
		})
	}
}
