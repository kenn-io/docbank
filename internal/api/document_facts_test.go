package api

import (
	"testing"

	"go.kenn.io/docbank/document"
)

func TestSafeDocumentEventVisibility(t *testing.T) {
	tests := []struct {
		name    string
		event   document.DocumentEventV1
		visible bool
	}{
		{name: "ordinary", visible: true},
		{name: "sensitive event", event: document.DocumentEventV1{Sensitive: true}},
		{name: "mixed recipients including bcc", event: document.DocumentEventV1{Actors: []document.DocumentEventActorV1{{Role: "sender"}, {Role: "blind_copy", Sensitive: true}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeDocumentEvent(tt.event); got != tt.visible {
				t.Errorf("visibility = %v, want %v", got, tt.visible)
			}
		})
	}
}

func TestWhenFromClaimNormalizesValues(t *testing.T) {
	tests := []struct {
		name                   string
		claim                  document.DocumentEventV1
		state, local, at, zone string
		offset                 *int
	}{
		{name: "minute offset", claim: document.DocumentEventV1{DateKind: "sent", Precision: "minute", DateValue: "2026-03-10T09:12", TimezoneKind: "offset", ZoneText: "-04:00", UTCKey: "2026-03-10T13:12:00.000000000Z", OffsetSeconds: new(-14400)}, state: "exact", local: "2026-03-10T09:12:00", at: "2026-03-10T13:12:00Z", zone: "offset", offset: new(-14400)},
		{name: "utc", claim: document.DocumentEventV1{DateKind: "created", Precision: "hour", DateValue: "2026-03-10T09", TimezoneKind: "utc", UTCKey: "2026-03-10T09:00:00.000000000Z"}, state: "exact", local: "2026-03-10T09:00:00", at: "2026-03-10T09:00:00Z", zone: "utc", offset: new(0)},
		{name: "omitted", claim: document.DocumentEventV1{DateKind: "sent", Precision: "minute", DateValue: "2026-03-10T09:12", TimezoneKind: "omitted"}, state: "floating", local: "2026-03-10T09:12:00", zone: "omitted"},
		{name: "date", claim: document.DocumentEventV1{DateKind: "created", Precision: "date", DateValue: "2026-03-10"}, state: "date"},
		{name: "month", claim: document.DocumentEventV1{DateKind: "created", Precision: "month", DateValue: "2026-03"}, state: "coarse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := whenFromClaim(tt.claim)
			if w.State != tt.state || w.Local != tt.local || w.At != tt.at || w.ZoneState != tt.zone {
				t.Errorf("when = %+v", w)
			}
			if (w.SourceOffsetSeconds == nil) != (tt.offset == nil) || tt.offset != nil && *w.SourceOffsetSeconds != *tt.offset {
				t.Errorf("offset = %v, want %v", w.SourceOffsetSeconds, tt.offset)
			}
			if w.State == "date" && w.Date != tt.claim.DateValue || w.State == "coarse" && w.Period != tt.claim.DateValue {
				t.Errorf("calendar value = %+v", w)
			}
		})
	}
}
