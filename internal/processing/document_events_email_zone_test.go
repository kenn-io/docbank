package processing

import (
	"go.kenn.io/docbank/document"
	"testing"
)

func TestEmailDateNumericZeroZone(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		kind string
	}{
		{"10 Mar 2026 12:30 +0000", "utc"},
		{"10 Mar 2026 12:30 -0000", "omitted"},
	} {
		event, ok := rawEmailDateEvent(tc.raw)
		if !ok || string(event.TimezoneKind) != tc.kind {
			t.Errorf("%q: kind %q, parsed %v; want %q", tc.raw, event.TimezoneKind, ok, tc.kind)
		}
	}
}

func TestEmailDateNamedZones(t *testing.T) {
	for _, tc := range []struct{ raw, kind, utc string }{
		{"10 Mar 2026 12:30 EST", "rfc5322", "2026-03-10T17:30:00.000000000Z"},
		{"10 Mar 2026 12:30 Z", "omitted", ""},
		{"10 Mar 2026 12:30 CET", "named", ""},
	} {
		e, ok := rawEmailDateEvent(tc.raw)
		if !ok || string(e.TimezoneKind) != tc.kind || e.UTCKey != tc.utc {
			t.Errorf("%q: %+v, parsed %v", tc.raw, e, ok)
		}
	}
}

func TestEmailDateInvalidOffsetRemainsFloating(t *testing.T) {
	for _, zone := range []string{"+1930", "+1260"} {
		event, ok := rawEmailDateEvent("10 Mar 2026 12:30 " + zone)
		if !ok || event.TimezoneKind != "invalid" || event.ZoneText != zone || event.OffsetSeconds != nil || event.UTCKey != "" {
			t.Errorf("%s: %+v, parsed %v", zone, event, ok)
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
	event, err := metadataDateEvent(record.Fields[0])
	if err != nil || event.TimezoneKind != "invalid" || event.ZoneText != "+1930" || event.RawValue != raw || event.UTCKey != "" {
		t.Fatalf("event = %+v, err = %v", event, err)
	}
}
