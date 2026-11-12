package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

func TestDocumentDateScopeMatchesOneVisibleClaim(t *testing.T) {
	t.Parallel()
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
	coarse := authored
	coarse.SourceKey += "/month"
	coarse.DateKind, coarse.DateValue, coarse.Precision = "modified", "2020-03", "month"
	coarse.AxisKey = "2020-03-01T00:00:00.000000000"
	coarse.EventID = document.DocumentEventID(s.VaultID(), node.CurrentVersionID, coarse.SourceKey, coarse.DateKind, coarse.DateValue, coarse.Precision, coarse.TimezoneKind, coarse.EvidenceSHA256)
	record.Events = append(record.Events, coarse)
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
	compiled = compileFixtureQuery(t, s, "date_kind:modified date:2020-03-04 date:2020-03-05", query.Filters{})
	require.Empty(t, compiledFixtureIDs(t, s.db, compiled, ""))
	compiled = compileFixtureQuery(t, s, "date_kind:modified date:2020-03-04..2020-03-07 date:2020-03-05", query.Filters{})
	require.Equal(t, []int64{node.ID}, compiledFixtureIDs(t, s.db, compiled, ""))
}

func TestDocumentDateBoundaryVectors(t *testing.T) {
	t.Parallel()
	_, missingErr := (queryCompiler{dateZone: "Pacific/Apia"}).compileDatePredicate(dateScope{ranges: [][2]string{{"2011-12-30", "2011-12-30"}}})
	require.ErrorContains(t, missingErr, "date_not_in_zone")
	_, missingErr = (queryCompiler{dateZone: "Pacific/Apia"}).compileDatePredicate(dateScope{ranges: [][2]string{{"2011-12-28", "2011-12-28"}, {"2011-12-29", "2011-12-29"}, {"2011-12-30", "2011-12-30"}}})
	require.ErrorContains(t, missingErr, "date_not_in_zone")
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
	s, node := documentDateZoneFixture(t)
	ctx := t.Context()
	for _, tc := range []struct {
		zone, day string
		matches   bool
	}{
		{"America/Toronto", "2026-03-09", true}, {"America/Toronto", "2026-03-10", false},
		{"Asia/Singapore", "2026-03-09", false}, {"Asia/Singapore", "2026-03-10", true},
	} {
		input := `{"syntax":"advanced","text":"date:` + tc.day + `","timezone":"` + tc.zone + `"}`
		q, err := query.Parse([]byte(input))
		require.NoError(t, err)
		c, err := compileQuery(ctx, q, queryResolver{q: s.db})
		require.NoError(t, err)
		ids := compiledFixtureIDs(t, s.db, c, "")
		if tc.matches {
			require.Equal(t, []int64{node.ID}, ids)
		} else {
			require.Empty(t, ids)
		}
	}
}

func documentDateZoneFixture(t *testing.T) (*Store, Node) {
	t.Helper()
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
	return s, node
}

func TestSavedDateQueryUsesItsOwnTimezone(t *testing.T) {
	t.Parallel()
	s, node := documentDateZoneFixture(t)
	ctx := t.Context()
	_, err := s.CreateSavedQuery(ctx, "Toronto", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"date:2026-03-09","timezone":"America/Toronto"}`))
	require.NoError(t, err)
	value, err := query.Parse([]byte(`{"syntax":"advanced","text":"saved:Toronto","timezone":"Asia/Singapore"}`))
	require.NoError(t, err)
	compiled, err := compileQuery(ctx, value, queryResolver{q: s.db})
	require.NoError(t, err)
	require.Equal(t, []int64{node.ID}, compiledFixtureIDs(t, s.db, compiled, ""))
	require.Equal(t, "America/Toronto", compiled.Timezone)
	require.False(t, compiled.TimezoneDefaulted)
	service := NewQuerySnapshotService(s)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	page, err := service.Create(ctx, "owner", SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Equal(t, "America/Toronto", page.Timezone)
	require.False(t, page.TimezoneDefaulted)
}

func TestDocumentDateAllDayEventMatchesSecondDay(t *testing.T) {
	t.Parallel()
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
	// Both days overlap the event, but their intersection is empty.
	for _, text := range []string{
		"date:2026-03-09 date:2026-03-10",
		"date:2026-03-09..2026-03-10 date:2026-03-11",
	} {
		compiled := compileFixtureQuery(t, s, text, query.Filters{})
		require.Empty(t, compiledFixtureIDs(t, s.db, compiled, ""))
	}
	value, err := query.Parse([]byte(`{"syntax":"advanced","text":"date:2026-03-09..2026-03-10","filters":{"document_date":{"from":"2026-03-10","to":"2026-03-11"}}}`))
	require.NoError(t, err)
	intersection, err := compileQuery(ctx, value, queryResolver{q: s.db})
	require.NoError(t, err)
	require.Equal(t, []int64{node.ID}, compiledFixtureIDs(t, s.db, intersection, ""))
	compiled := compileFixtureQuery(t, s, "date:2026-03-10 date_precision:day", query.Filters{})
	require.Equal(t, []int64{node.ID}, compiledFixtureIDs(t, s.db, compiled, ""))
	compiled = compileFixtureQuery(t, s, "date_kind:ended date:2026-03-10", query.Filters{})
	require.Empty(t, compiledFixtureIDs(t, s.db, compiled, ""))
	compiled = compileFixtureQuery(t, s, "date_kind:ended date:2026-03-11", query.Filters{})
	require.Equal(t, []int64{node.ID}, compiledFixtureIDs(t, s.db, compiled, ""))
}

func TestDocumentDateDisclosureUsesFullPrimary(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "private-date.txt", fakeHash("a7"), 10, "text/plain")
	require.NoError(t, err)
	target := requireDocumentEventTarget(t, s, node.CurrentVersionID)
	record := documentEventRecord(t, s.VaultID(), node.CurrentVersionID, "d7")
	record.Events[0].Actors[0].Sensitive = true
	record.Primaries = nil
	for _, disclosure := range []string{"safe", "full"} {
		if primary, ok := document.SelectPrimaryEvent(record.Events, record.DocumentKind, "vault", disclosure); ok {
			record.Primaries = append(record.Primaries, primary)
		}
	}
	_, err = s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
	require.NoError(t, err)
	for _, text := range []string{"date:2019-03-04", "date_kind:created date:2019-03-04"} {
		value, err := query.Parse([]byte(`{"syntax":"advanced","text":"` + text + `"}`))
		require.NoError(t, err)
		for _, tc := range []struct {
			full  bool
			count int
		}{{false, 0}, {true, 1}} {
			compiled, err := (queryCompiler{fullDisclosure: tc.full}).compile(ctx, value, queryResolver{q: s.db})
			require.NoError(t, err)
			require.Len(t, compiledFixtureIDs(t, s.db, compiled, ""), tc.count)
		}
	}
}

func TestDocumentDateSnapshotWaitsForIndex(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.CreateFile(t.Context(), s.RootID(), "pending.txt", fakeHash("a8"), 1, "text/plain")
	require.NoError(t, err)
	service := NewQuerySnapshotService(s)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	value := snapshotTestQuery(t, `{"syntax":"advanced","text":"date:2019-03-04"}`)
	_, err = service.Create(t.Context(), "owner", SnapshotRequest{Query: value})
	require.ErrorIs(t, err, ErrDocumentDatesPending)
	driveDocumentEventWorker(t, s)
	_, err = service.Create(t.Context(), "owner", SnapshotRequest{Query: value})
	require.NoError(t, err)
}

func TestNonDateSnapshotDoesNotReportDefaultedTimezone(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	service := NewQuerySnapshotService(s)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	page, err := service.Create(t.Context(), "owner", SnapshotRequest{Query: snapshotTestQuery(t, `{}`)})
	require.NoError(t, err)
	require.Empty(t, page.Timezone)
	require.False(t, page.TimezoneDefaulted)
}
