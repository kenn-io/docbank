package processing

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestCalendarCreatorKeepsItsRole(t *testing.T) {
	require.Equal(t, document.EventRole("attendee"), personRoleForSourceField(document.SourceMetadataFieldV1{
		Key: "creators", Namespace: "calendar", SourceField: "ATTENDEE",
	}))
	require.Equal(t, document.EventRole("organizer"), personRoleForSourceField(document.SourceMetadataFieldV1{
		Key: "creators", Namespace: "calendar", SourceField: "ORGANIZER",
	}))
	require.Equal(t, document.EventRole("author"), personRoleForSourceField(document.SourceMetadataFieldV1{
		Key: "creators", Namespace: "pdf.info", SourceField: "Author",
	}))
	require.Empty(t, personRoleForSourceField(document.SourceMetadataFieldV1{Key: "filename"}))
}

func TestF10ActorsKeepEvidenceWithoutInventingDates(t *testing.T) {
	creator := document.SourceMetadataFieldV1{
		Key: "creators", Namespace: "pdf.info", SourceField: "Author",
		Value: document.SourceMetadataValueV1{
			Kind: document.SourceMetadataStringList, Strings: []string{"Ada Lovelace"},
		},
	}

	undated, _, err := DeriveDocumentEvents(documentEventInput("application/pdf", []document.SourceMetadataFieldV1{creator}))
	require.NoError(t, err)
	require.Len(t, undated.Events, 1)
	recorded := requireEventKind(t, undated.Events, "vault_recorded")
	require.Len(t, recorded.Actors, 1)
	require.Equal(t, document.EventEvidenceKind("source_metadata"), recorded.Actors[0].EvidenceKind)
	require.Equal(t, "meta-generation", recorded.Actors[0].EvidenceID)
	require.NotEqual(t, recorded.EvidenceKind, recorded.Actors[0].EvidenceKind)
	require.Empty(t, eventsWithKind(undated.Events, "authored"))

	datedInput := documentEventInput("application/pdf", []document.SourceMetadataFieldV1{
		creator,
		timestampField("created", "pdf.info", "CreationDate", false,
			"2020-01-02", "D:20200102", "date", "omitted"),
	})
	dated, _, err := DeriveDocumentEvents(datedInput)
	require.NoError(t, err)
	created := requireEventKind(t, dated.Events, "created")
	require.Len(t, created.Actors, 1)
	require.Equal(t, document.EventEvidenceKind("source_metadata"), created.Actors[0].EvidenceKind)
	require.Equal(t, "meta-generation", created.Actors[0].EvidenceID)
	require.Empty(t, requireEventKind(t, dated.Events, "vault_recorded").Actors,
		"a dated source claim must not be duplicated on the recorded fallback")
}

func eventsWithKind(events []document.DocumentEventV1, kind document.DateKind) []document.DocumentEventV1 {
	matched := make([]document.DocumentEventV1, 0)
	for _, event := range events {
		if event.DateKind == kind {
			matched = append(matched, event)
		}
	}
	return matched
}
