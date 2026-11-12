package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/taxrecord"
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
	require.ErrorIs(t, err, taxrecord.ErrInvalid)
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
func TestTaxEventMetadataAuthorityRoundTrip(t *testing.T) {
	t.Parallel()
	s, _, r := taxFixture(t)
	ctx := t.Context()
	m, err := s.AdvanceTaxRecord(ctx, r.RecordID, r.Revision, TaxEventInput{Kind: "filed", OccurredOn: "2026-01-01", Reference: "synthetic-reference"})
	require.NoError(t, err)
	_, err = s.AddTaxEvent(ctx, r.RecordID, m.Record.Revision, TaxEventInput{Kind: "note", OccurredOn: "2025-01-01", Note: "example"})
	require.NoError(t, err)
	other, err := s.CreateTaxRecord(ctx, TaxRecordInput{FilerPartyID: &r.FilerPartyID, Country: new("ZZ"), TaxKind: new("example"), Kind: "audit", Title: "Other"})
	require.NoError(t, err)
	closed, err := s.AdvanceTaxRecord(ctx, other.RecordID, other.Revision, TaxEventInput{Kind: "closed", OccurredOn: "2026-01-01"})
	require.NoError(t, err)
	note, err := s.AddTaxEvent(ctx, other.RecordID, closed.Record.Revision, TaxEventInput{Kind: "note", OccurredOn: "2026-01-01", Reference: "synthetic-reference"})
	require.NoError(t, err)
	voided, err := s.VoidTaxEvent(ctx, other.RecordID, note.Event.EventID, note.Record.Revision, "example reason")
	require.NoError(t, err)
	_, err = s.RetireTaxRecord(ctx, other.RecordID, voided.Record.Revision)
	require.NoError(t, err)
	var data bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &data))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(data.Bytes())))
	var restored bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &restored))
	assert.Equal(t, data.String(), restored.String())
	detail, err := target.GetTaxRecordDetail(ctx, other.RecordID)
	require.NoError(t, err)
	require.NotNil(t, detail.Record.RetiredAt)
	require.Len(t, detail.Events, 2)
	require.NotNil(t, detail.Events[1].VoidedAt)
	assert.Equal(t, "example reason", detail.Events[1].VoidReason)
	assert.Equal(t, "synthetic-reference", detail.Events[1].Reference)
	invalid := strings.Replace(data.String(), `"void_reason":"example reason"`, `"void_reason":""`, 1)
	require.NotEqual(t, data.String(), invalid)
	dest := newTestStore(t)
	require.Error(t, dest.ImportMetadata(ctx, strings.NewReader(invalid)))
	_, err = dest.GetTaxRecord(ctx, other.RecordID)
	require.ErrorIs(t, err, ErrNotFound)
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
	t.Parallel()
	for _, tc := range []struct {
		update, cause string
		invalid       bool
	}{
		{`UPDATE tax_events SET kind='refund'`, "sets_state: invalid state", true},
		{`UPDATE tax_events SET voided_at='0001-01-01T00:00:00.000000000Z', void_reason='Correction'`, "tax event void precedes recording", false},
	} {
		t.Run(tc.cause, func(t *testing.T) {
			s, _, r := taxFixture(t)
			ctx := t.Context()
			_, err := s.AdvanceTaxRecord(ctx, r.RecordID, r.Revision, TaxEventInput{Kind: "filed", OccurredOn: "2026-01-01"})
			require.NoError(t, err)
			_, err = s.db.ExecContext(ctx, tc.update)
			require.NoError(t, err)
			var data bytes.Buffer
			err = s.ExportMetadata(ctx, &data)
			require.ErrorContains(t, err, tc.cause)
			if tc.invalid {
				require.ErrorIs(t, err, taxrecord.ErrInvalid)
			}
			assert.Empty(t, data.String())
		})
	}
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
	err = validateTaxMetadataState(ctx, s.db)
	require.ErrorIs(t, err, taxrecord.ErrInvalid)
	require.ErrorContains(t, err, "state: unsupported state")
}

func TestTaxEventTransactionFailureRollsBack(t *testing.T) {
	t.Parallel()
	s, _, r := taxFixture(t)
	ctx := t.Context()
	_, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_tax_event_parent BEFORE UPDATE OF state ON tax_records BEGIN SELECT RAISE(ABORT, 'synthetic failure'); END`)
	require.NoError(t, err)
	_, err = s.AdvanceTaxRecord(ctx, r.RecordID, r.Revision, TaxEventInput{Kind: "filed", OccurredOn: "2026-01-01"})
	require.Error(t, err)
	d, err := s.GetTaxRecordDetail(ctx, r.RecordID)
	require.NoError(t, err)
	assert.Equal(t, r, d.Record)
	assert.Empty(t, d.Events)
}
func TestTaxEventConcurrentWritersRejectStaleRevision(t *testing.T) {
	t.Parallel()
	s, _, r := taxFixture(t)
	ctx := t.Context()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := s.AddTaxEvent(ctx, r.RecordID, r.Revision, TaxEventInput{Kind: "note", OccurredOn: "2026-01-01"})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	var success, stale int
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrStaleRevision) {
			stale++
		} else {
			t.Errorf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, success)
	assert.Equal(t, 1, stale)
	d, err := s.GetTaxRecordDetail(ctx, r.RecordID)
	require.NoError(t, err)
	assert.Len(t, d.Events, 1)
	assert.Equal(t, r.Revision+1, d.Record.Revision)
}

func checkTaxEventsAuthorityAndBalance(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	p, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Example", Own: true})
	require.NoError(t, err)
	in := TaxRecordInput{FilerPartyID: &p.PartyID, Country: new("ZZ"), TaxKind: new("example"), Kind: "filing", Title: "Example", Direction: "payable", Amount: new("10"), Currency: new("USD")}
	r, err := s.CreateTaxRecord(ctx, in)
	require.NoError(t, err)
	first, err := s.AddTaxEvent(ctx, r.RecordID, r.Revision, TaxEventInput{Kind: "payment", OccurredOn: "2026-01-01", Amount: new("2"), Currency: "USD"})
	require.NoError(t, err)
	other, err := s.AddTaxEvent(ctx, r.RecordID, first.Record.Revision, TaxEventInput{Kind: "refund", OccurredOn: "2026-01-01", Amount: new("1"), Currency: "EUR"})
	require.NoError(t, err)
	d, err := s.GetTaxRecordDetail(ctx, r.RecordID)
	require.NoError(t, err)
	assert.Equal(t, new(int64(800)), d.Balance.OutstandingMinor)
	assert.Equal(t, int64(1), d.Balance.OtherCurrencyEvents)
	require.NoError(t, validateTaxMetadataState(ctx, s.db))
	voided, err := s.VoidTaxEvent(ctx, r.RecordID, first.Event.EventID, other.Record.Revision, "correction")
	require.NoError(t, err)
	d, err = s.GetTaxRecordDetail(ctx, r.RecordID)
	require.NoError(t, err)
	assert.Equal(t, new(int64(1000)), d.Balance.OutstandingMinor)
	assert.Equal(t, voided.Record.Revision, d.Record.Revision)
}

func TestTaxCashMetadataRoundTripAndRollback(t *testing.T) {
	s, _, r := taxFixture(t)
	ctx := t.Context()
	m, err := s.AddTaxEvent(ctx, r.RecordID, r.Revision, TaxEventInput{Kind: "payment", OccurredOn: "2026-01-01", Amount: new("12.34"), Currency: "USD"})
	require.NoError(t, err)
	_, err = s.VoidTaxEvent(ctx, r.RecordID, m.Event.EventID, m.Record.Revision, "correction")
	require.NoError(t, err)
	var data, backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &data))
	snap, err := s.BeginMetadataSnapshot(ctx)
	require.NoError(t, err)
	require.NoError(t, snap.ExportBackup(ctx, &backup))
	require.NoError(t, snap.Close())
	for _, raw := range [][]byte{data.Bytes(), backup.Bytes()} {
		target := newTestStore(t)
		require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(raw)))
		detail, err := target.GetTaxRecordDetail(ctx, r.RecordID)
		require.NoError(t, err)
		require.Len(t, detail.Events, 1)
		assert.Equal(t, "payment", detail.Events[0].Kind)
		assert.NotNil(t, detail.Events[0].VoidedAt)
	}
	for _, replacement := range []struct{ old, new string }{
		{`"amount_minor":1234`, `"amount_minor":0`}, {`"amount_minor":1234,"currency":"USD"`, `"amount_minor":1234,"currency":""`}, {`"sets_state":""`, `"sets_state":"filed"`},
	} {
		corrupted := strings.Replace(data.String(), replacement.old, replacement.new, 1)
		require.NotEqual(t, data.String(), corrupted)
		dest := newTestStore(t)
		require.Error(t, dest.ImportMetadata(ctx, strings.NewReader(corrupted)))
		_, err = dest.GetTaxRecord(ctx, r.RecordID)
		require.ErrorIs(t, err, ErrNotFound)
	}
}

func TestTaxRecordMoneyEditChecksBalance(t *testing.T) {
	s, _, r := taxFixture(t)
	ctx := t.Context()
	cash := TaxEventInput{Kind: "payment", OccurredOn: "2026-01-01", Currency: "USD", Amount: new("92233720368547758.07")}
	a, err := s.AddTaxEvent(ctx, r.RecordID, r.Revision, cash)
	require.NoError(t, err)
	cash.Amount = new("0.01")
	b, err := s.AddTaxEvent(ctx, r.RecordID, a.Record.Revision, cash)
	require.NoError(t, err)
	in := TaxRecordInput{AccountID: r.AccountID, Kind: r.Kind, Title: r.Title, PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd, TaxYear: r.TaxYear, Direction: "payable", Amount: new("1"), Currency: new("USD")}
	_, err = s.UpdateTaxRecord(ctx, r.RecordID, b.Record.Revision, in)
	require.ErrorIs(t, err, taxrecord.ErrInvalid)
	d, err := s.GetTaxRecordDetail(ctx, r.RecordID)
	require.NoError(t, err)
	assert.Equal(t, b.Record.Revision, d.Record.Revision)
	assert.Len(t, d.Events, 2)
}

func TestTaxCashAppendVoidAtomic(t *testing.T) {
	s, _, r := taxFixture(t)
	ctx := t.Context()
	in := TaxRecordInput{AccountID: r.AccountID, Kind: r.Kind, Title: r.Title, PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd, Direction: "payable", Amount: new("1"), Currency: new("USD")}
	r, err := s.UpdateTaxRecord(ctx, r.RecordID, r.Revision, in)
	require.NoError(t, err)
	cash := TaxEventInput{Kind: "payment", OccurredOn: "2026-01-01", Currency: "USD", Amount: new("92233720368547758.07")}
	first, err := s.AddTaxEvent(ctx, r.RecordID, r.Revision, cash)
	require.NoError(t, err)
	cash.Amount = new("0.01")
	_, err = s.AddTaxEvent(ctx, r.RecordID, first.Record.Revision, cash)
	require.ErrorIs(t, err, taxrecord.ErrInvalid)
	d, err := s.GetTaxRecordDetail(ctx, r.RecordID)
	require.NoError(t, err)
	assert.Len(t, d.Events, 1)
	assert.Equal(t, first.Record.Revision, d.Record.Revision)
	_, err = s.VoidTaxEvent(ctx, r.RecordID, first.Event.EventID, r.Revision, "correction")
	require.ErrorIs(t, err, ErrStaleRevision)
	voided, err := s.VoidTaxEvent(ctx, r.RecordID, first.Event.EventID, first.Record.Revision, "correction")
	require.NoError(t, err)
	d, err = s.GetTaxRecordDetail(ctx, r.RecordID)
	require.NoError(t, err)
	assert.Equal(t, first.Record.Revision+1, voided.Record.Revision)
	assert.Equal(t, new(int64(100)), d.Balance.OutstandingMinor)
}

func TestTaxCashMetadataRejectsOverflow(t *testing.T) {
	s, _, r := taxFixture(t)
	ctx := t.Context()
	cash := TaxEventInput{Kind: "payment", OccurredOn: "2026-01-01", Currency: "USD", Amount: new("92233720368547758.07")}
	a, err := s.AddTaxEvent(ctx, r.RecordID, r.Revision, cash)
	require.NoError(t, err)
	cash.Amount = new("0.01")
	_, err = s.AddTaxEvent(ctx, r.RecordID, a.Record.Revision, cash)
	require.NoError(t, err)
	var valid bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &valid))
	invalid := strings.Replace(valid.String(), `"direction":"unknown","amount_minor":null`, `"direction":"payable","amount_minor":100`, 1)
	require.NotEqual(t, valid.String(), invalid)
	dest := newTestStore(t)
	require.ErrorIs(t, dest.ImportMetadata(ctx, strings.NewReader(invalid)), taxrecord.ErrInvalid)
	_, err = dest.GetTaxRecord(ctx, r.RecordID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.db.ExecContext(ctx, `UPDATE tax_records SET direction='payable',amount_minor=100 WHERE record_id=$1`, r.RecordID)
	require.NoError(t, err)
	var output bytes.Buffer
	require.ErrorIs(t, s.ExportMetadata(ctx, &output), taxrecord.ErrInvalid)
	assert.Empty(t, output.String())
}

func TestTaxCashCancellationAndWriteFailure(t *testing.T) {
	s, _, r := taxFixture(t)
	ctx := t.Context()
	cash := TaxEventInput{Kind: "payment", OccurredOn: "2026-01-01", Amount: new("1.00"), Currency: "USD"}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := s.AddTaxEvent(canceled, r.RecordID, r.Revision, cash)
	require.ErrorIs(t, err, context.Canceled)
	for _, ddl := range []string{
		`CREATE TABLE cash_test_parent (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE cash_test_child (id INTEGER REFERENCES cash_test_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TRIGGER fail_cash_commit AFTER INSERT ON tax_events BEGIN INSERT INTO cash_test_child VALUES (1); END`,
	} {
		_, err = s.db.ExecContext(ctx, ddl)
		require.NoError(t, err)
	}
	_, err = s.AddTaxEvent(ctx, r.RecordID, r.Revision, cash)
	require.Error(t, err)
	d, err := s.GetTaxRecordDetail(ctx, r.RecordID)
	require.NoError(t, err)
	assert.Equal(t, r, d.Record)
	assert.Empty(t, d.Events)
}
