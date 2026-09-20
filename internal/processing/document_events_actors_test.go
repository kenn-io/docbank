package processing

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestCalendarOrganizerParametersKeepActor(t *testing.T) {
	for _, sourceField := range []string{"ORGANIZER", "ORGANIZER;CN=Alice", "organizer;cn=Alice"} {
		t.Run(sourceField, func(t *testing.T) {
			payload := []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\n" + sourceField +
				":mailto:alice@example.test\r\nEND:VEVENT\r\nEND:VCALENDAR")
			metadata, err := ExtractSourceMetadata(t.Context(), sourceMetadataTestSpool(t), payload)
			require.NoError(t, err)
			require.Len(t, metadata.Fields, 1)
			require.Equal(t, sourceField, metadata.Fields[0].SourceField)

			record, _, err := DeriveDocumentEvents(documentEventInput("text/calendar", metadata.Fields))
			require.NoError(t, err)
			actors := requireEventKind(t, record.Events, "vault_recorded").Actors
			require.Len(t, actors, 1)
			require.Equal(t, document.EventRole("organizer"), actors[0].Role)
			require.Equal(t, "mailto:alice@example.test", actors[0].Claim)
		})
	}
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
	require.Equal(t, document.EventRole("author"), recorded.Actors[0].Role)
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
