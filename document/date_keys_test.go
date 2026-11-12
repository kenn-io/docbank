package document

import "testing"

func TestDateKeysPairCalendarSiblings(t *testing.T) {
	events := []DocumentEventV1{
		{SourceKey: "metadata/generation/calendar.event.e000001.start/0", DateKind: "started", DateValue: "2026-03-09", Precision: "date"},
		{SourceKey: "metadata/generation/calendar.event.e000001.end/1", DateKind: "ended", DateValue: "2026-03-11", Precision: "date"},
		{SourceKey: "metadata/generation/calendar.event.e000002.start/0", DateKind: "started", DateValue: "2026-04-01", Precision: "date"},
		{SourceKey: "metadata/generation/calendar.event.e000002.end/1", DateKind: "ended", DateValue: "2026-04-03", Precision: "date"},
		{SourceKey: "metadata/generation/calendar.start/0", DateKind: "started", DateValue: "2026-05-01", Precision: "date"},
		{SourceKey: "metadata/generation/calendar.end.raw/1", DateKind: "ended", DateValue: "2026-05-04", Precision: "date"},
		{SourceKey: "metadata/other/calendar.end/1", DateKind: "ended", DateValue: "2026-06-01", Precision: "date"},
		{SourceKey: "calendar.event.e000003.start", DateKind: "started", DateValue: "2026-03-09T12:00", Precision: "minute", Actors: []DocumentEventActorV1{{Sensitive: true}}},
	}
	keys, err := DateKeysFor(events)
	if err != nil {
		t.Fatal(err)
	}
	if keys[0].CivilEndDate != "2026-03-10" {
		t.Errorf("end = %q", keys[0].CivilEndDate)
	}
	if keys[1].CivilDate != "2026-03-11" {
		t.Errorf("ended date = %q", keys[1].CivilDate)
	}
	if keys[2].CivilEndDate != "2026-04-02" || keys[4].CivilEndDate != "2026-05-03" {
		t.Errorf("pairing = %+v", keys)
	}
	if !keys[7].Floating || !keys[7].Sensitive {
		t.Errorf("floating/sensitive = %+v", keys[7])
	}
}
