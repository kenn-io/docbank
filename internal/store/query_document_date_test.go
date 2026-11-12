package store

import (
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/kit/civiltime"
)

func TestDocumentDateScopeMatchesOneVisibleClaim(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "fixture.txt", fakeHash("a1"), 10, "text/plain")
	require.NoError(t, err)
	target := requireDocumentEventTarget(t, s, node.CurrentVersionID)
	record := documentEventRecord(t, s.VaultID(), node.CurrentVersionID, "d1")
	authored := record.Events[0]
	authored.SourceKey += "/authored"
	authored.DateKind = "authored"
	authored.DateValue = "2020-03-04"
	authored.AxisKey = "2020-03-04T00:00:00.000000000"
	authored.EventID = document.DocumentEventID(s.VaultID(), node.CurrentVersionID, authored.SourceKey, authored.DateKind, authored.DateValue, authored.Precision, authored.TimezoneKind, authored.EvidenceSHA256)
	record.Events = append(record.Events, authored)
	record.Primaries = nil
	for _, disclosure := range []string{"safe", "full"} {
		primary, ok := document.SelectPrimaryEvent(record.Events, record.DocumentKind, "vault", disclosure)
		require.True(t, ok)
		record.Primaries = append(record.Primaries, primary)
	}
	_, err = s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
	require.NoError(t, err)
	compiled := compileFixtureQuery(t, s, "date_kind:authored date:2019-03-04", query.Filters{})
	require.Empty(t, compiledFixtureIDs(t, s.db, compiled, ""))
	compiled = compileFixtureQuery(t, s, "date_kind:authored date:2020-03-04 date_precision:day", query.Filters{})
	require.Equal(t, []int64{node.ID}, compiledFixtureIDs(t, s.db, compiled, ""))
	compiled = compileFixtureQuery(t, s, "date_kind:authored date:2020-03-04 date_precision:coarse", query.Filters{})
	require.Empty(t, compiledFixtureIDs(t, s.db, compiled, ""))
}

func TestDocumentDateBoundsRejectMissingDay(t *testing.T) {
	_, err := (queryCompiler{dateZone: "Pacific/Apia"}).compileDatePredicate(dateScope{ranges: [][2]string{{"2011-12-30", "2011-12-30"}}})
	require.ErrorContains(t, err, "date_not_in_zone")
}

func TestDocumentDateBoundaryVectors(t *testing.T) {
	for _, tc := range []struct{ zone, day, start, end string }{
		{"America/Santiago", "2026-09-06", "2026-09-06T04:00:00.000000000Z", "2026-09-07T03:00:00.000000000Z"},
		{"America/Havana", "2026-03-08", "2026-03-08T05:00:00.000000000Z", "2026-03-09T04:00:00.000000000Z"},
		{"America/Toronto", "2026-03-08", "2026-03-08T05:00:00.000000000Z", "2026-03-09T04:00:00.000000000Z"},
		{"America/Toronto", "2026-11-01", "2026-11-01T04:00:00.000000000Z", "2026-11-02T05:00:00.000000000Z"},
	} {
		part, err := (queryCompiler{dateZone: tc.zone}).compileDatePredicate(dateScope{ranges: [][2]string{{tc.day, tc.day}}})
		require.NoError(t, err)
		require.Equal(t, tc.start, part.args[0])
		require.Equal(t, tc.end, part.args[1])
	}
}

func TestSavedDateQueryUsesItsOwnTimezone(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "saved-zone.txt", fakeHash("a2"), 10, "text/plain")
	require.NoError(t, err)
	target := requireDocumentEventTarget(t, s, node.CurrentVersionID)
	record := documentEventRecord(t, s.VaultID(), node.CurrentVersionID, "e1")
	event := &record.Events[0]
	event.DateValue, event.Precision, event.TimezoneKind = "2026-03-10T03:00:00", "second", "utc"
	event.UTCKey, event.AxisKey = "2026-03-10T03:00:00.000000000Z", "2026-03-10T03:00:00.000000000"
	event.EventID = document.DocumentEventID(s.VaultID(), node.CurrentVersionID, event.SourceKey, event.DateKind, event.DateValue, event.Precision, event.TimezoneKind, event.EvidenceSHA256)
	record.Primaries = nil
	for _, disclosure := range []string{"safe", "full"} {
		primary, ok := document.SelectPrimaryEvent(record.Events, record.DocumentKind, "vault", disclosure)
		require.True(t, ok)
		record.Primaries = append(record.Primaries, primary)
	}
	_, err = s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Toronto", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"date:2026-03-09","timezone":"America/Toronto"}`))
	require.NoError(t, err)
	value, err := query.Parse([]byte(`{"syntax":"advanced","text":"saved:Toronto","timezone":"Asia/Singapore"}`))
	require.NoError(t, err)
	compiled, err := compileQuery(ctx, value, queryResolver{q: s.db})
	require.NoError(t, err)
	require.Equal(t, []int64{node.ID}, compiledFixtureIDs(t, s.db, compiled, ""))
}

func TestDocumentDateDayOfClockBack(t *testing.T) {
	loc, err := civiltime.LoadZone("America/St_Johns")
	require.NoError(t, err)
	instant, err := time.Parse(time.RFC3339, "2010-11-07T03:00:00Z")
	require.NoError(t, err)
	day, err := civiltime.ParseDate("2010-11-07")
	require.NoError(t, err)
	require.Equal(t, day, civiltime.DayOf(instant, loc))
}

func TestDocumentDateKindHidesSensitiveActor(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "bcc-fixture.eml", fakeHash("b2"), 10, "message/rfc822")
	require.NoError(t, err)
	target := requireDocumentEventTarget(t, s, node.CurrentVersionID)
	record := documentEventRecord(t, s.VaultID(), node.CurrentVersionID, "b3")
	record.DocumentKind = "email"
	event := &record.Events[0]
	event.DateKind = "sent"
	event.Actors[0].Role = "blind_copy"
	event.Actors[0].Sensitive = true
	event.EventID = document.DocumentEventID(s.VaultID(), node.CurrentVersionID, event.SourceKey, event.DateKind, event.DateValue, event.Precision, event.TimezoneKind, event.EvidenceSHA256)
	record.Primaries = nil
	primary, ok := document.SelectPrimaryEvent(record.Events, record.DocumentKind, "vault", "full")
	require.True(t, ok)
	record.Primaries = append(record.Primaries, primary)
	_, err = s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
	require.NoError(t, err)
	compiled := compileFixtureQuery(t, s, "date_kind:sent date:2019-03-04", query.Filters{})
	require.Empty(t, compiledFixtureIDs(t, s.db, compiled, ""))
}

func TestDocumentDateAllDayEventMatchesSecondDay(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "calendar.ics", fakeHash("c1"), 10, "text/calendar")
	require.NoError(t, err)
	target := requireDocumentEventTarget(t, s, node.CurrentVersionID)
	record := documentEventRecord(t, s.VaultID(), node.CurrentVersionID, "c2")
	record.DocumentKind = "calendar"
	start := record.Events[0]
	start.SourceKey = "metadata/c2/calendar.event.e000001.start/0"
	start.DateKind, start.DateValue = "started", "2026-03-09"
	start.AxisKey = "2026-03-09T00:00:00.000000000"
	start.EventID = document.DocumentEventID(s.VaultID(), node.CurrentVersionID, start.SourceKey, start.DateKind, start.DateValue, start.Precision, start.TimezoneKind, start.EvidenceSHA256)
	end := start
	end.SourceKey = "metadata/c2/calendar.event.e000001.end/1"
	end.DateKind, end.DateValue = "ended", "2026-03-11"
	end.AxisKey = "2026-03-11T00:00:00.000000000"
	end.EventID = document.DocumentEventID(s.VaultID(), node.CurrentVersionID, end.SourceKey, end.DateKind, end.DateValue, end.Precision, end.TimezoneKind, end.EvidenceSHA256)
	record.Events = []document.DocumentEventV1{start, end}
	record.Primaries = nil
	for _, disclosure := range []string{"safe", "full"} {
		primary, ok := document.SelectPrimaryEvent(record.Events, record.DocumentKind, "vault", disclosure)
		require.True(t, ok)
		record.Primaries = append(record.Primaries, primary)
	}
	_, err = s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
	require.NoError(t, err)
	compiled := compileFixtureQuery(t, s, "date:2026-03-10 date_precision:day", query.Filters{})
	require.Equal(t, []int64{node.ID}, compiledFixtureIDs(t, s.db, compiled, ""))
}
