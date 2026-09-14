package document

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDateKindRegistryIsExactlySixteenClosedValues(t *testing.T) {
	want := []DateKind{"sent", "received", "document_date", "authored", "created", "modified",
		"captured", "started", "ended", "printed", "accessed", "produced", "exported",
		"imported", "observed", "vault_recorded"}
	assert.Equal(t, want, AllDateKinds())
	for _, kind := range want {
		assert.True(t, ValidDateKind(kind), string(kind))
	}
	assert.False(t, ValidDateKind("recorded"))
	assert.False(t, ValidDateKind(""))
	assert.Equal(t, []EventPrecision{"year", "month", "date", "hour", "minute", "second", "fraction"},
		AllEventPrecisions())
	assert.Equal(t, []EventTimezoneKind{"utc", "offset", "named", "omitted", "invalid"},
		AllEventTimezoneKinds())
	assert.Equal(t, []EventRole{"author", "last_saved_by", "custodian", "sender", "recipient",
		"copied", "blind_copy", "organizer", "attendee", "participant"}, AllEventRoles())
	assert.Equal(t, []EventEvidenceKind{"source_metadata", "email_generation",
		"provenance_binding", "content_version", "package_row", "output_receipt", "transfer_record",
		"takeout_claim", "media_source_version"}, AllEventEvidenceKinds())
	assert.Equal(t, []DocumentKind{"email", "message", "calendar", "image", "audio_video",
		"other"}, AllDocumentKinds())
}

func TestEventRegistriesRejectUnknownValues(t *testing.T) {
	assert.False(t, ValidEventPrecision("day"))
	assert.False(t, ValidEventTimezoneKind("local"))
	assert.False(t, ValidEventRole("owner"))
	assert.False(t, ValidEventEvidenceKind("unknown"))
	assert.False(t, ValidDocumentKind("pdf"))
}

func TestEventRegistrySlicesAreIndependent(t *testing.T) {
	dates := AllDateKinds()
	dates[0] = "changed"
	assert.Equal(t, DateKind("sent"), AllDateKinds()[0])

	precisions := AllEventPrecisions()
	precisions[0] = "changed"
	assert.Equal(t, EventPrecision("year"), AllEventPrecisions()[0])

	timezones := AllEventTimezoneKinds()
	timezones[0] = "changed"
	assert.Equal(t, EventTimezoneKind("utc"), AllEventTimezoneKinds()[0])

	roles := AllEventRoles()
	roles[0] = "changed"
	assert.Equal(t, EventRole("author"), AllEventRoles()[0])

	evidence := AllEventEvidenceKinds()
	evidence[0] = "changed"
	assert.Equal(t, EventEvidenceKind("source_metadata"), AllEventEvidenceKinds()[0])

	documents := AllDocumentKinds()
	documents[0] = "changed"
	assert.Equal(t, DocumentKind("email"), AllDocumentKinds()[0])
}

func TestDocumentEventBounds(t *testing.T) {
	assert.Equal(t, 1024, MaxDocumentEvents)
	assert.Equal(t, 4096, MaxDocumentEventActors)
	assert.Equal(t, 8<<20, MaxDocumentEventsEncodedBytes)
	assert.Equal(t, 64<<10, MaxDocumentEventRawValueBytes)
	assert.Equal(t, 4<<10, MaxDocumentEventLocatorBytes)
	assert.Equal(t, 4<<10, MaxDocumentEventActorClaimBytes)
	assert.Equal(t, 384, MaxActorKeyBytes)
}
