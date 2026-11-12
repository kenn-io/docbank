package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func checkTaxRecordLifecycle(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	filer, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Example Holdings Ltd.", Own: true})
	require.NoError(t, err)
	account, err := s.CreateTaxAccount(ctx, TaxAccountInput{FilerPartyID: filer.PartyID, Country: "ZZ", TaxKind: "example", Cadence: "annual", Currency: "USD"})
	require.NoError(t, err)
	input := TaxRecordInput{AccountID: &account.AccountID, Kind: "filing", Title: "Example filing", PeriodStart: new("2026-01-01"), PeriodEnd: new("2026-12-31")}
	record, err := s.CreateTaxRecord(ctx, input)
	require.NoError(t, err)
	assert.Equal(t, "expected", record.State)
	assert.Equal(t, "USD", record.Currency)
	require.NotNil(t, record.TaxYear)
	assert.Equal(t, int64(2026), *record.TaxYear)
	_, err = s.CreateTaxRecord(ctx, input)
	require.ErrorIs(t, err, ErrExists)
	input.Title = "Updated filing"
	updated, err := s.UpdateTaxRecord(ctx, record.RecordID, record.Revision, input)
	require.NoError(t, err)
	_, err = s.UpdateTaxRecord(ctx, record.RecordID, record.Revision, input)
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.RetireTaxRecord(ctx, record.RecordID, updated.Revision)
	require.NoError(t, err)
}
func TestTaxRecordDefaultsAndConsistency(t *testing.T) {
	t.Parallel()
	checkTaxRecordLifecycle(t, newTestStore)
}

func TestTaxCompletePeriodUniqueness(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Example", Own: true})
	require.NoError(t, err)
	a, err := s.CreateTaxAccount(ctx, TaxAccountInput{FilerPartyID: p.PartyID, Country: "ZZ", TaxKind: "example", Cadence: "annual", Currency: "USD"})
	require.NoError(t, err)
	input := TaxRecordInput{AccountID: &a.AccountID, Kind: "filing", Title: "Example", PeriodStart: new("2026-01-01")}
	_, err = s.CreateTaxRecord(ctx, input)
	require.NoError(t, err)
	_, err = s.CreateTaxRecord(ctx, input)
	require.NoError(t, err)
	input.PeriodEnd = new("2026-12-31")
	r, err := s.CreateTaxRecord(ctx, input)
	require.NoError(t, err)
	_, err = s.CreateTaxRecord(ctx, input)
	require.ErrorIs(t, err, ErrExists)
	_, err = s.RetireTaxRecord(ctx, r.RecordID, r.Revision)
	require.NoError(t, err)
	_, err = s.CreateTaxRecord(ctx, input)
	require.NoError(t, err)
}

func TestTaxListFiltersAndRetirement(t *testing.T) {
	t.Parallel()
	s, a, r := taxFixture(t)
	ctx := t.Context()
	other, err := s.CreateTaxRecord(ctx, TaxRecordInput{AccountID: &a.AccountID, Kind: "audit", Title: "Example review", TaxYear: new(int64(2025))})
	require.NoError(t, err)
	rows, err := s.ListTaxRecords(ctx, TaxRecordListOptions{FilerPartyID: a.FilerPartyID, Country: "ZZ", TaxKind: "EXAMPLE", TaxYear: new(int64(2026)), Kind: "filing", State: "expected", Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, r.RecordID, rows[0].RecordID)
	_, err = s.RetireTaxRecord(ctx, other.RecordID, other.Revision)
	require.NoError(t, err)
	rows, err = s.ListTaxRecords(ctx, TaxRecordListOptions{IncludeRetired: true, Limit: 100})
	require.NoError(t, err)
	assert.Len(t, rows, 2)
	_, err = s.ListTaxRecords(ctx, TaxRecordListOptions{Country: "EU", Limit: 100})
	require.Error(t, err)
	accounts, err := s.ListTaxAccounts(ctx, TaxAccountListOptions{FilerPartyID: a.FilerPartyID, Country: "ZZ", TaxKind: "EXAMPLE", Limit: 100})
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	assert.Equal(t, a.AccountID, accounts[0].AccountID)
}

func TestTaxRecordAccountDefaultsAreSnapshotAndDynamicLead(t *testing.T) {
	t.Parallel()
	s, a, r := taxFixture(t)
	ctx := t.Context()
	require.Nil(t, r.LeadDays)
	assert.Equal(t, "USD", r.Currency)
	lead := int64(30)
	input := TaxAccountInput{FilerPartyID: a.FilerPartyID, Country: a.Country, TaxKind: a.TaxKind, Cadence: a.Cadence, Currency: "JPY", LeadDays: &lead}
	a, err := s.UpdateTaxAccount(ctx, a.AccountID, a.Revision, input)
	require.NoError(t, err)
	got, err := s.GetTaxRecord(ctx, r.RecordID)
	require.NoError(t, err)
	assert.Equal(t, "USD", got.Currency)
	assert.Nil(t, got.LeadDays)
	assert.Equal(t, int64(30), a.LeadDays)
	next, err := s.CreateTaxRecord(ctx, TaxRecordInput{AccountID: &a.AccountID, Kind: "notice", Title: "Example"})
	require.NoError(t, err)
	assert.Equal(t, "JPY", next.Currency)
}

func TestTaxOneOffAndRetiredAccountReferences(t *testing.T) {
	t.Parallel()
	s, a, r := taxFixture(t)
	ctx := t.Context()
	p, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Another Example Ltd.", Own: true})
	require.NoError(t, err)
	one := TaxRecordInput{FilerPartyID: &a.FilerPartyID, Country: new("ZZ"), TaxKind: new("example"), Kind: "notice", Title: "One-off"}
	record, err := s.CreateTaxRecord(ctx, one)
	require.NoError(t, err)
	one.FilerPartyID = &p.PartyID
	record, err = s.UpdateTaxRecord(ctx, record.RecordID, record.Revision, one)
	require.NoError(t, err)
	assert.Equal(t, p.PartyID, record.FilerPartyID)
	_, err = s.RetireTaxAccount(ctx, a.AccountID, a.Revision)
	require.NoError(t, err)
	attached := TaxRecordInput{AccountID: &a.AccountID, Kind: "filing", Title: "Updated"}
	_, err = s.UpdateTaxRecord(ctx, r.RecordID, r.Revision, attached)
	require.NoError(t, err)
	_, err = s.CreateTaxRecord(ctx, TaxRecordInput{AccountID: &a.AccountID, Kind: "notice", Title: "New"})
	require.ErrorIs(t, err, ErrTaxRetired)
}
