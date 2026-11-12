package processing

import (
	"go.kenn.io/docbank/document"
	"testing"
)

func TestEmailDateZones(t *testing.T) {
	for _, tc := range []struct{ zone, kind string }{
		{"+0000", "utc"}, {"-0000", "omitted"}, {"Z", "omitted"}, {"+1930", "invalid"}, {"+1260", "invalid"},
	} {
		event, ok := rawEmailDateEvent("10 Mar 2026 12:30 " + tc.zone)
		if !ok || string(event.TimezoneKind) != tc.kind {
			t.Errorf("%s: %+v parsed %v", tc.zone, event, ok)
		}
		if tc.kind == "invalid" && (event.ZoneText != tc.zone || event.OffsetSeconds != nil || event.UTCKey != "") {
			t.Errorf("%s: %+v", tc.zone, event)
		}
	}
}

func TestInvalidEmailOffsetRetainedThroughMetadata(t *testing.T) {
	raw := "10 Mar 2026 12:30 +1930"
	record := document.SourceMetadataV1{}
	collector := metadataCollector{record: &record, seen: make(map[string]bool)}
	collector.emailTimestamp(document.EmailDateV1{State: document.EmailDateInvalid}, raw)
	if len(record.Fields) != 1 || record.Fields[0].Key != "email.sent.raw" {
		t.Fatalf("fields = %+v", record.Fields)
	}
}
