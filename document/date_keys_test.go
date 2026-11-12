package document

import "testing"

func TestDateKeysPairCalendarSiblings(t *testing.T) {
	events := []DocumentEventV1{
		{SourceKey: "metadata/generation/calendar.event.e000001.start/0", DateKind: "started", DateValue: "2026-03-09", Precision: "date"},
		{SourceKey: "metadata/generation/calendar.event.e000001.end/1", DateKind: "ended", DateValue: "2026-03-11", Precision: "date"},
		{SourceKey: "calendar.event.e000002.start", DateKind: "started", DateValue: "2026-03-09T12:00", Precision: "minute", Actors: []DocumentEventActorV1{{Sensitive: true}}},
	}
	keys, err := DateKeysFor(events)
	if err != nil {
		t.Fatal(err)
	}
	if keys[0].CivilEndDate != "2026-03-10" {
		t.Errorf("end = %q", keys[0].CivilEndDate)
	}
	if !keys[2].Floating || !keys[2].Sensitive {
		t.Errorf("floating/sensitive = %+v", keys[2])
	}
}
