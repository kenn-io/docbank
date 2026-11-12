package processing

import (
	"go.kenn.io/docbank/document"
	"testing"
)

func TestEmailDateZones(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ zone, kind string }{
		{"UT", "rfc5322"}, {"gMt", "rfc5322"}, {"est", "rfc5322"}, {"EDT", "rfc5322"}, {"CST", "rfc5322"}, {"CDT", "rfc5322"}, {"MST", "rfc5322"}, {"MDT", "rfc5322"}, {"PST", "rfc5322"}, {"PDT", "rfc5322"}, {"UTC", "utc"}, {"utc", "utc"}, {"A", "omitted"}, {"M", "omitted"}, {"z", "omitted"}, {"J", "named"}, {"CET", "named"}, {"+1800", "offset"}, {"+1801", "invalid"}, {"+0000", "utc"}, {"-0000", "omitted"}, {"Z", "omitted"}, {"+1930", "invalid"}, {"+1260", "invalid"},
	} {
		event, ok := rawEmailDateEvent("10 Mar 2026 12:30 " + tc.zone)
		if !ok || string(event.TimezoneKind) != tc.kind {
			t.Errorf("%s: %+v parsed %v", tc.zone, event, ok)
		}
		if tc.kind == "invalid" && (event.ZoneText != tc.zone || event.OffsetSeconds != nil || event.UTCKey != "") {
			t.Errorf("%s: %+v", tc.zone, event)
		}
	}
	for _, tc := range []struct {
		zone    string
		seconds int
	}{
		{"UT", 0}, {"gMt", 0}, {"est", -5 * 3600}, {"EDT", -4 * 3600}, {"CST", -6 * 3600}, {"CDT", -5 * 3600}, {"MST", -7 * 3600}, {"MDT", -6 * 3600}, {"PST", -8 * 3600}, {"PDT", -7 * 3600}, {"+1800", 18 * 3600},
	} {
		event, ok := rawEmailDateEvent("10 Mar 2026 12:30 " + tc.zone)
		if !ok || event.OffsetSeconds == nil || *event.OffsetSeconds != tc.seconds || event.UTCKey == "" {
			t.Errorf("%s: %+v parsed %v", tc.zone, event, ok)
		}
	}

}

func TestInvalidEmailOffsetRetainedThroughMetadata(t *testing.T) {
	t.Parallel()
	raw := "10 Mar 2026 12:30 +1930"
	record := document.SourceMetadataV1{}
	collector := metadataCollector{record: &record, seen: make(map[string]bool)}
	collector.emailTimestamp(document.EmailDateV1{State: document.EmailDateInvalid}, raw)
	if len(record.Fields) != 1 || record.Fields[0].Key != "email.sent.raw" {
		t.Fatalf("fields = %+v", record.Fields)
	}
}
