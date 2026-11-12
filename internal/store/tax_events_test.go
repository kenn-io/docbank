package store

import (
	"bytes"
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync"
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

func TestTaxEventAuditAndCanceledWrites(t *testing.T) {
	s, _, r := taxFixture(t)
	ctx := t.Context()
	note := TaxEventInput{Kind: "note", OccurredOn: "2026-01-01"}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := s.AddTaxEvent(canceled, r.RecordID, r.Revision, note)
	require.ErrorIs(t, err, context.Canceled)
	seedInitialAuditAuthority(t, s, s.RootID())
	_, err = s.AddTaxEvent(ctx, r.RecordID, r.Revision, note)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.AdvanceTaxRecord(ctx, r.RecordID, r.Revision, TaxEventInput{Kind: "filed", OccurredOn: "2026-01-01"})
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	detail, err := s.GetTaxRecordDetail(ctx, r.RecordID)
	require.NoError(t, err)
	assert.Equal(t, r, detail.Record)
	assert.Empty(t, detail.Events)
}
func TestTaxEventOrderingSameOrBackwardTimestamps(t *testing.T) {
	s, _, r := taxFixture(t)
	ctx := t.Context()
	const future = "9998-01-01T00:00:00.000000000Z"
	_, err := s.db.ExecContext(ctx, `UPDATE tax_records SET updated_at=$1 WHERE record_id=$2`, future, r.RecordID)
	require.NoError(t, err)
	one, err := s.AdvanceTaxRecord(ctx, r.RecordID, r.Revision, TaxEventInput{Kind: "filed", OccurredOn: "2026-01-01"})
	require.NoError(t, err)
	two, err := s.AdvanceTaxRecord(ctx, r.RecordID, one.Record.Revision, TaxEventInput{Kind: "ready", OccurredOn: "2025-01-01"})
	require.NoError(t, err)
	assert.Equal(t, future, one.Event.RecordedAt)
	assert.Equal(t, future, two.Event.RecordedAt)
	d, err := s.GetTaxRecordDetail(ctx, r.RecordID)
	require.NoError(t, err)
	require.Len(t, d.Events, 2)
	assert.Equal(t, []string{"filed", "ready"}, []string{d.Events[0].Kind, d.Events[1].Kind})
	assert.Equal(t, "ready", d.Record.State)
}

func TestTaxEventVoidMarkersAndRetiredMetadata(t *testing.T) {
	s, _, r := taxFixture(t)
	ctx := t.Context()
	event, err := s.AddTaxEvent(ctx, r.RecordID, r.Revision, TaxEventInput{Kind: "note", OccurredOn: "2026-01-01", Reference: "synthetic-reference"})
	require.NoError(t, err)
	event, err = s.VoidTaxEvent(ctx, r.RecordID, event.Event.EventID, event.Record.Revision, "example reason")
	require.NoError(t, err)
	_, err = s.RetireTaxRecord(ctx, r.RecordID, event.Record.Revision)
	require.NoError(t, err)
	var original bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &original))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(original.Bytes())))
	d, err := target.GetTaxRecordDetail(ctx, r.RecordID)
	require.NoError(t, err)
	require.NotNil(t, d.Record.RetiredAt)
	require.Len(t, d.Events, 1)
	assert.Equal(t, "example reason", d.Events[0].VoidReason)
	assert.Equal(t, "synthetic-reference", d.Events[0].Reference)
	require.NotNil(t, d.Events[0].VoidedAt)
	invalid := strings.Replace(original.String(), `"void_reason":"example reason"`, `"void_reason":""`, 1)
	require.NotEqual(t, original.String(), invalid)
	dest := newTestStore(t)
	require.Error(t, dest.ImportMetadata(ctx, strings.NewReader(invalid)))
	_, err = dest.GetTaxRecord(ctx, r.RecordID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestTaxEventTransactionFailureRollsBack(t *testing.T) {
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
func TestTaxEventConcurrentRevisionFence(t *testing.T) {
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

func TestTaxEventsSource48DoesNotQueryTaxCatalogs(t *testing.T) {
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			db, _ := newReleasedFixtureStore(t, filepath.Join(t.TempDir(), "source.db"), driver.driver, schemaV0170SQL, 48)
			var data bytes.Buffer
			require.NoError(t, exportReleasedMetadataSnapshot(t.Context(), db, &data, 48))
			assert.NotContains(t, data.String(), `"type":"tax_event"`)
		})
	}
}
