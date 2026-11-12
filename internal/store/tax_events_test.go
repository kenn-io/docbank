package store

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func checkTaxEventsLifecycle(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	p, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Example Holdings Ltd.", Own: true})
	require.NoError(t, err)
	r, err := s.CreateTaxRecord(ctx, TaxRecordInput{FilerPartyID: &p.PartyID, Country: new("ZZ"), TaxKind: new("example"), Kind: "filing", Title: "Example"})
	require.NoError(t, err)
	first, err := s.AdvanceTaxRecord(ctx, r.RecordID, r.Revision, TaxEventInput{Kind: "filed", OccurredOn: "2025-01-01", Reference: "synthetic-secret"})
	require.NoError(t, err)
	assert.Equal(t, "filed", first.Record.State)
	assert.Equal(t, int64(2), first.Record.Revision)
	second, err := s.AdvanceTaxRecord(ctx, r.RecordID, first.Record.Revision, TaxEventInput{Kind: "assessed", OccurredOn: "2024-01-01", Amount: new("12.34"), Currency: "USD"})
	require.NoError(t, err)
	assert.Equal(t, new(int64(1234)), second.Event.AmountMinor)
	assert.Equal(t, r.AmountMinor, second.Record.AmountMinor)
	_, err = s.AddTaxEvent(ctx, r.RecordID, second.Record.Revision, TaxEventInput{Kind: "filed", OccurredOn: "2024-01-01"})
	require.Error(t, err)
	_, err = s.VoidTaxEvent(ctx, r.RecordID, first.Event.EventID, first.Record.Revision, "mistake")
	require.ErrorIs(t, err, ErrStaleRevision)
	voided, err := s.VoidTaxEvent(ctx, r.RecordID, first.Event.EventID, second.Record.Revision, "mistake")
	require.NoError(t, err)
	assert.Equal(t, "assessed", voided.Record.State)
	_, err = s.VoidTaxEvent(ctx, r.RecordID, first.Event.EventID, voided.Record.Revision, "again")
	require.ErrorIs(t, err, ErrTaxEventVoided)
	last, err := s.VoidTaxEvent(ctx, r.RecordID, second.Event.EventID, voided.Record.Revision, "mistake")
	require.NoError(t, err)
	assert.Equal(t, "expected", last.Record.State)
	detail, err := s.GetTaxRecordDetail(ctx, r.RecordID)
	require.NoError(t, err)
	require.Len(t, detail.Events, 2)
	assert.Equal(t, "synthetic-secret", detail.Events[0].Reference)
	assert.Equal(t, last.Record.Revision, detail.Record.Revision)
	_, err = s.VoidTaxEvent(ctx, r.RecordID, "11111111-1111-4111-8111-111111111111", last.Record.Revision, "mistake")
	require.ErrorIs(t, err, ErrNotFound)
	retired, err := s.RetireTaxRecord(ctx, r.RecordID, last.Record.Revision)
	require.NoError(t, err)
	_, err = s.AddTaxEvent(ctx, r.RecordID, retired.Revision, TaxEventInput{Kind: "note", OccurredOn: "2024-01-01"})
	require.ErrorIs(t, err, ErrTaxRetired)
}
func TestTaxEventAppendVoidAtomic(t *testing.T) { checkTaxEventsLifecycle(t, newTestStore) }

func TestTaxEventRevisionExhaustion(t *testing.T) {
	s, _, r := taxFixture(t)
	_, err := s.db.ExecContext(t.Context(), `UPDATE tax_records SET revision=$1 WHERE record_id=$2`, int64(math.MaxInt64), r.RecordID)
	require.NoError(t, err)
	_, err = s.AddTaxEvent(t.Context(), r.RecordID, math.MaxInt64, TaxEventInput{Kind: "note", OccurredOn: "2026-01-01"})
	require.ErrorIs(t, err, ErrTaxRevisionExhausted)
	d, err := s.GetTaxRecordDetail(t.Context(), r.RecordID)
	require.NoError(t, err)
	assert.Empty(t, d.Events)
	assert.Equal(t, int64(math.MaxInt64), d.Record.Revision)
}

func TestTaxEventMetadataAuthorityRoundTrip(t *testing.T) {
	s, _, r := taxFixture(t)
	ctx := t.Context()
	m, err := s.AdvanceTaxRecord(ctx, r.RecordID, r.Revision, TaxEventInput{Kind: "filed", OccurredOn: "2026-01-01", Reference: "synthetic-reference"})
	require.NoError(t, err)
	_, err = s.AddTaxEvent(ctx, r.RecordID, m.Record.Revision, TaxEventInput{Kind: "note", OccurredOn: "2025-01-01", Note: "example"})
	require.NoError(t, err)
	var data bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &data))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(data.Bytes())))
	var restored bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &restored))
	assert.Equal(t, data.String(), restored.String())
	for _, replacement := range []struct{ old, new string }{
		{`"kind":"filed"`, `"kind":"payment"`}, {`"sets_state":"filed"`, `"sets_state":"ready"`}, {`"state":"filed"`, `"state":"ready"`}, {`"record_revision":2`, `"record_revision":99`},
	} {
		corrupted := strings.Replace(data.String(), replacement.old, replacement.new, 1)
		require.NotEqual(t, data.String(), corrupted)
		dest := newTestStore(t)
		root := dest.RootID()
		require.Error(t, dest.ImportMetadata(ctx, strings.NewReader(corrupted)))
		assert.Equal(t, root, dest.RootID())
		_, err = dest.GetTaxRecord(ctx, r.RecordID)
		require.ErrorIs(t, err, ErrNotFound)
	}
}
func TestTaxEventExportRejectsInvalidAuthority(t *testing.T) {
	s, _, r := taxFixture(t)
	ctx := t.Context()
	_, err := s.AdvanceTaxRecord(ctx, r.RecordID, r.Revision, TaxEventInput{Kind: "filed", OccurredOn: "2026-01-01"})
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE tax_events SET kind='refund'`)
	require.NoError(t, err)
	var data bytes.Buffer
	require.Error(t, s.ExportMetadata(ctx, &data))
	assert.Empty(t, data.String())
}

func checkTaxEventsAuthority(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	p, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Example Holdings Ltd.", Own: true})
	require.NoError(t, err)
	r, err := s.CreateTaxRecord(ctx, TaxRecordInput{FilerPartyID: &p.PartyID, Country: new("ZZ"), TaxKind: new("example"), Kind: "filing", Title: "Example"})
	require.NoError(t, err)
	_, err = s.AdvanceTaxRecord(ctx, r.RecordID, r.Revision, TaxEventInput{Kind: "filed", OccurredOn: "2026-01-01"})
	require.NoError(t, err)
	require.NoError(t, validateTaxMetadataState(ctx, s.db))
	_, err = s.db.ExecContext(ctx, `UPDATE tax_records SET state='ready' WHERE record_id=$1`, r.RecordID)
	require.NoError(t, err)
	require.Error(t, validateTaxMetadataState(ctx, s.db))
}
