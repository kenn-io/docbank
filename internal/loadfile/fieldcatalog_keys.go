package loadfile

import "slices"

var fieldCatalogKeys = []string{
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

// FieldCatalogKeys returns the closed canonical mapping target set.
func FieldCatalogKeys() []string { return slices.Clone(fieldCatalogKeys) }

// FieldCatalogKeyAllowed reports whether key is a canonical mapping target.
func FieldCatalogKeyAllowed(key string) bool {
	_, found := slices.BinarySearch(fieldCatalogKeys, key)
	return found
}
