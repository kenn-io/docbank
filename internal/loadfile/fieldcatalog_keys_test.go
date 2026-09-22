package loadfile

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestFieldCatalogKeysAreTheClosedSpecCatalog(t *testing.T) {
	want := []string{
		"loadfile.actor.attendee",
		"loadfile.actor.author",
		"loadfile.actor.blind_copy",
		"loadfile.actor.copied",
		"loadfile.actor.last_saved_by",
		"loadfile.actor.organizer",
		"loadfile.actor.participant",
		"loadfile.actor.recipient",
		"loadfile.actor.sender",
		"loadfile.calendar.end",
		"loadfile.calendar.start",
		"loadfile.custodian",
		"loadfile.custodian.additional",
		"loadfile.date.accessed",
		"loadfile.date.created",
		"loadfile.date.document",
		"loadfile.date.family",
		"loadfile.date.modified",
		"loadfile.date.printed",
		"loadfile.date.production",
		"loadfile.date.received",
		"loadfile.date.sent",
		"loadfile.date.sort",
		"loadfile.document.description",
		"loadfile.document.id",
		"loadfile.document.kind",
		"loadfile.document.name",
		"loadfile.family.children",
		"loadfile.family.id",
		"loadfile.family.parent",
		"loadfile.file.native",
		"loadfile.file.produced_pdf",
		"loadfile.file.supplied_text",
		"loadfile.label.assigned.begin",
		"loadfile.label.assigned.end",
		"loadfile.label.assigned.set",
		"loadfile.label.begin",
		"loadfile.label.begin_attach",
		"loadfile.label.end",
		"loadfile.label.end_attach",
		"loadfile.label.set",
		"loadfile.time.created",
		"loadfile.time.document",
		"loadfile.time.modified",
		"loadfile.time.received",
		"loadfile.time.sent",
	}

	keys := fieldCatalogKeys
	require.Len(t, keys, 46)
	assert.Equal(t, want, keys)
	assert.True(t, sort.StringsAreSorted(keys))
	seen := map[string]bool{}
	for _, key := range keys {
		assert.True(t, strings.HasPrefix(key, "loadfile."), key)
		assert.False(t, seen[key], key)
		assert.False(t, document.SourceMetadataCanonicalKeyAllowed(key), key)
		seen[key] = true
		assert.True(t, FieldCatalogKeyAllowed(key), key)
	}
	assert.False(t, FieldCatalogKeyAllowed("email.sent"))
	assert.False(t, FieldCatalogKeyAllowed("loadfile.not.a.key"))
}
