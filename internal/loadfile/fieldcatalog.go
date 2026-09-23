package loadfile

import (
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
)

// CatalogEntry describes a retained sender field. Package scope may prepend
// document_date to primary-rule/v1; vault scope does not. The resulting reasons
// are package_supplied_document_date, email_date_header, and
// docbank_recorded_fallback. Sender values remain separate from derived dates.
type CatalogEntry struct {
	Canonical string   `json:"canonical"`
	DateKind  string   `json:"date_kind"`
	Aliases   []string `json:"aliases"`
	Primary   bool     `json:"primary"`
}

var packageFieldAliases = map[string][]string{
	"loadfile.actor.attendee":       {"ATTENDEES"},
	"loadfile.actor.author":         {"AUTHOR", "Author"},
	"loadfile.actor.blind_copy":     {"BCC", "Blind Copy"},
	"loadfile.actor.copied":         {"CC", "Copied"},
	"loadfile.actor.last_saved_by":  {"LASTSAVEDBY", "Last Saved By"},
	"loadfile.actor.organizer":      {"ORGANIZER"},
	"loadfile.actor.participant":    {"PARTICIPANTS"},
	"loadfile.actor.recipient":      {"TO", "Recipients"},
	"loadfile.actor.sender":         {"FROM", "Sender"},
	"loadfile.calendar.end":         {"EVENTEND"},
	"loadfile.calendar.start":       {"EVENTSTART"},
	"loadfile.custodian":            {"CUSTODIAN"},
	"loadfile.custodian.additional": {"ADDITIONALCUSTODIANS", "Additional Custodian(s)"},
	"loadfile.date.accessed":        {"DATEACCESSED"},
	"loadfile.date.created":         {"DATECREATED", "Created Date/Time"},
	"loadfile.date.document":        {"DOCDATE", "Document Date"},
	"loadfile.date.family":          {"FAMILYDATE"},
	"loadfile.date.modified":        {"DATE_MOD", "LastSaveDate", "ModifyDate"},
	"loadfile.date.printed":         {"DATEPRINTED"},
	"loadfile.date.production":      {"PRODUCTIONDATE"},
	"loadfile.date.received":        {"DATERECEIVED", "Received Date/Time"},
	"loadfile.date.sent":            {"DATESENT", "Sent Date/Time"},
	"loadfile.date.sort":            {"SORTDATE"},
	"loadfile.document.description": {"DESCRIPTION", "Subject"},
	"loadfile.document.id":          {"DOCID", "Document ID"},
	"loadfile.document.kind":        {"DOCTYPE", "Document Type"},
	"loadfile.document.name":        {"FILENAME", "File Name"},
	"loadfile.family.children":      {"ATTACHMENTIDS", "Attachment IDs"},
	"loadfile.family.id":            {"FAMILYID", "Family ID"},
	"loadfile.family.parent":        {"PARENTID", "Parent ID"},
	"loadfile.file.native":          {"NATIVE", "Native Path", "Native Link"},
	"loadfile.file.produced_pdf":    {"PDF", "PDF Path", "Produced Document Link"},
	"loadfile.file.supplied_text":   {"TEXT", "Text Path", "Produced Text Link"},
	"loadfile.label.begin":          {"BEGBATES", "BEGDOC", "Begin Doc"},
	"loadfile.label.begin_attach":   {"BEGATTACH"},
	"loadfile.label.end":            {"ENDBATES", "ENDDOC", "End Doc"},
	"loadfile.label.end_attach":     {"ENDATTACH"},
	"loadfile.label.set":            {"LABELSET"},
	"loadfile.time.created":         {"TIMECREATED"},
	"loadfile.time.document":        {"DOCTIME"},
	"loadfile.time.modified":        {"TIMEMODIFIED"},
	"loadfile.time.received":        {"TIMERECEIVED"},
	"loadfile.time.sent":            {"TIMESENT"},
}

func FieldCatalog() []CatalogEntry {
	kinds := map[string]string{
		"document": "document_date", "sent": "sent", "received": "received",
		"created": "created", "modified": "modified", "printed": "printed",
		"accessed": "accessed", "production": "produced",
	}
	keys := FieldCatalogKeys()
	out := make([]CatalogEntry, 0, len(keys))
	for _, key := range keys {
		entry := CatalogEntry{Canonical: key, Aliases: slices.Clone(packageFieldAliases[key])}
		if suffix, ok := strings.CutPrefix(key, "loadfile.date."); ok {
			entry.DateKind = kinds[suffix]
		}
		switch key {
		case "loadfile.calendar.start":
			entry.DateKind = "started"
		case "loadfile.calendar.end":
			entry.DateKind = "ended"
		}
		switch entry.DateKind {
		case "document_date", "sent", "received", "created", "modified", "started":
			entry.Primary = true
		}
		out = append(out, entry)
	}
	return out
}

func ResolveColumn(header string) (CatalogEntry, bool) {
	fold := func(value string) string { return strings.ToLower(strings.Join(strings.Fields(value), " ")) }
	wanted := fold(header)
	for _, entry := range FieldCatalog() {
		if fold(entry.Canonical) == wanted {
			return entry, true
		}
		for _, alias := range entry.Aliases {
			if fold(alias) == wanted {
				return entry, true
			}
		}
	}
	return CatalogEntry{}, false
}

func ActorRoleForColumn(header string) (role string, sensitive, ok bool) {
	entry, ok := ResolveColumn(header)
	if !ok {
		return "", false, false
	}
	if entry.Canonical == "loadfile.custodian" || entry.Canonical == "loadfile.custodian.additional" {
		return "custodian", true, true
	}
	role, ok = strings.CutPrefix(entry.Canonical, "loadfile.actor.")
	return role, role == "blind_copy", ok
}

func ActorKey(kind, value string) (string, error) {
	identity, err := document.NormalizePersonIdentity(document.PersonIdentityKind(kind), value)
	if err != nil {
		return "", err
	}
	return document.ActorKey(identity)
}
