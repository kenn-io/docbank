package store

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/taxrecord"
)

func checkTaxRecordLifecycle(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	filer, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Example Holdings Ltd.", Own: true})
	require.NoError(t, err)
	authority, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Example Authority"})
	require.NoError(t, err)
	account, err := s.CreateTaxAccount(ctx, TaxAccountInput{FilerPartyID: filer.PartyID, AuthorityPartyID: &authority.PartyID, Country: "ZZ", TaxKind: "example", Cadence: "annual", Currency: "USD"})
	require.NoError(t, err)
	input := TaxRecordInput{AccountID: &account.AccountID, Kind: "filing", Title: "Example filing", PeriodStart: new("2026-01-01"), PeriodEnd: new("2026-12-31")}
	record, err := s.CreateTaxRecord(ctx, input)
	require.NoError(t, err)
	assert.Equal(t, "expected", record.State)
	assert.Equal(t, "USD", record.Currency)
	equal := TaxRecordInput{AccountID: &account.AccountID, FilerPartyID: &account.FilerPartyID, Country: &account.Country, Subdivision: &account.Subdivision, TaxKind: &account.TaxKind, AuthorityPartyID: account.AuthorityPartyID, Kind: "notice", Title: "Equal identity"}
	_, err = s.CreateTaxRecord(ctx, equal)
	require.NoError(t, err)
	require.NotNil(t, record.TaxYear)
	assert.Equal(t, int64(2026), *record.TaxYear)
	_, err = s.CreateTaxRecord(ctx, input)
	require.ErrorIs(t, err, ErrExists)
	input.TaxYear = record.TaxYear
	input.Title = "Updated filing"
	updated, err := s.UpdateTaxRecord(ctx, record.RecordID, record.Revision, input)
	require.NoError(t, err)
	assert.Equal(t, record.PeriodStart, updated.PeriodStart)
	assert.Equal(t, record.PeriodEnd, updated.PeriodEnd)
	assert.Equal(t, record.TaxYear, updated.TaxYear)
	_, err = s.UpdateTaxRecord(ctx, record.RecordID, record.Revision, input)
	require.ErrorIs(t, err, ErrStaleRevision)
	filter := TaxRecordListOptions{FilerPartyID: filer.PartyID, Country: "ZZ", TaxKind: "EXAMPLE", TaxYear: new(int64(2026)), State: "expected", Kind: "filing", Limit: 100}
	rows, err := s.ListTaxRecords(ctx, filter)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, record.RecordID, rows[0].RecordID)
	_, err = s.RetireTaxRecord(ctx, record.RecordID, updated.Revision)
	require.NoError(t, err)
	rows, err = s.ListTaxRecords(ctx, filter)
	require.NoError(t, err)
	assert.Empty(t, rows)
	filter.IncludeRetired = true
	rows, err = s.ListTaxRecords(ctx, filter)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, record.RecordID, rows[0].RecordID)
	_, err = s.UpdateTaxRecord(ctx, record.RecordID, updated.Revision+1, input)
	require.ErrorIs(t, err, ErrTaxRetired)
	_, err = s.CreateTaxRecord(ctx, input)
	require.NoError(t, err)
	incomplete := input
	incomplete.PeriodEnd = nil
	incomplete.TaxYear = nil
	_, err = s.CreateTaxRecord(ctx, incomplete)
	require.NoError(t, err)
	_, err = s.CreateTaxRecord(ctx, incomplete)
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
	for _, tc := range []struct {
		input  TaxRecordInput
		filter TaxRecordListOptions
	}{
		{TaxRecordInput{FilerPartyID: &a.FilerPartyID, Country: new("CA"), TaxKind: new("example"), Kind: "filing", Title: "Other country", TaxYear: new(int64(2026))}, TaxRecordListOptions{Country: "ZZ", Limit: 100}},
		{TaxRecordInput{FilerPartyID: &a.FilerPartyID, Country: new("ZZ"), TaxKind: new("other"), Kind: "filing", Title: "Other kind", TaxYear: new(int64(2026))}, TaxRecordListOptions{TaxKind: "example", Limit: 100}},
		{TaxRecordInput{FilerPartyID: &a.FilerPartyID, Country: new("ZZ"), TaxKind: new("example"), Kind: "filing", Title: "Other year", TaxYear: new(int64(2025))}, TaxRecordListOptions{TaxYear: new(int64(2026)), Limit: 100}},
		{TaxRecordInput{FilerPartyID: &a.FilerPartyID, Country: new("ZZ"), TaxKind: new("example"), Kind: "notice", Title: "Other record kind", TaxYear: new(int64(2026))}, TaxRecordListOptions{Kind: "filing", Limit: 100}},
	} {
		candidate, err := s.CreateTaxRecord(ctx, tc.input)
		require.NoError(t, err)
		filtered, err := s.ListTaxRecords(ctx, tc.filter)
		require.NoError(t, err)
		ids := make([]string, 0, len(filtered))
		for _, row := range filtered {
			ids = append(ids, row.RecordID)
		}
		assert.Contains(t, ids, r.RecordID)
		assert.NotContains(t, ids, candidate.RecordID)
	}
	_, err = s.RetireTaxRecord(ctx, other.RecordID, other.Revision)
	require.NoError(t, err)
	rows, err = s.ListTaxRecords(ctx, TaxRecordListOptions{IncludeRetired: true, Limit: 100})
	require.NoError(t, err)
	assert.Len(t, rows, 6)
	rows, err = s.ListTaxRecords(ctx, TaxRecordListOptions{State: "filed", Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, rows)
	_, err = s.ListTaxRecords(ctx, TaxRecordListOptions{State: "invalid", Limit: 100})
	require.ErrorIs(t, err, taxrecord.ErrInvalid)
	require.ErrorContains(t, err, "state: unknown state")
	_, err = s.ListTaxRecords(ctx, TaxRecordListOptions{Country: "EU", Limit: 100})
	require.ErrorIs(t, err, taxrecord.ErrInvalid)
	require.ErrorContains(t, err, "country")
	_, err = s.ListTaxAccounts(ctx, TaxAccountListOptions{FilerPartyID: "invalid", Limit: 100})
	require.ErrorIs(t, err, taxrecord.ErrInvalid)
	field, ok := errors.AsType[*taxrecord.FieldError](err)
	require.True(t, ok)
	assert.Equal(t, "filer", field.Field)
	assert.Equal(t, "invalid value", field.Reason)
	_, err = s.ListTaxRecords(ctx, TaxRecordListOptions{FilerPartyID: "invalid", Limit: 100})
	require.ErrorIs(t, err, taxrecord.ErrInvalid)
	field, ok = errors.AsType[*taxrecord.FieldError](err)
	require.True(t, ok)
	assert.Equal(t, "filer", field.Field)
	assert.Equal(t, "invalid value", field.Reason)
	_, err = s.ListTaxRecords(ctx, TaxRecordListOptions{TaxYear: new(int64(10000)), Limit: 100})
	require.ErrorIs(t, err, taxrecord.ErrInvalid)
	require.ErrorContains(t, err, "tax year selector")
	accounts, err := s.ListTaxAccounts(ctx, TaxAccountListOptions{FilerPartyID: a.FilerPartyID, Country: "ZZ", TaxKind: "EXAMPLE", Limit: 100})
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	assert.Equal(t, a.AccountID, accounts[0].AccountID)
}

func TestTaxRecordCurrencySnapshotAndNullableLead(t *testing.T) {
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
	attached := TaxRecordInput{AccountID: &a.AccountID, Kind: "filing", Title: "Updated", PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd, TaxYear: r.TaxYear}
	got, err := s.UpdateTaxRecord(ctx, r.RecordID, r.Revision, attached)
	require.NoError(t, err)
	assert.Equal(t, r.PeriodStart, got.PeriodStart)
	assert.Equal(t, r.PeriodEnd, got.PeriodEnd)
	assert.Equal(t, r.TaxYear, got.TaxYear)
	_, err = s.CreateTaxRecord(ctx, TaxRecordInput{AccountID: &a.AccountID, Kind: "notice", Title: "New"})
	require.ErrorIs(t, err, ErrTaxRetired)
}

func TestTaxRecordAuthorityAndReplacement(t *testing.T) {
	t.Parallel()
	s, a, r := taxFixture(t)
	ctx := t.Context()
	authority, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Example Revenue Agency"})
	require.NoError(t, err)
	input := TaxRecordInput{FilerPartyID: &a.FilerPartyID, Country: new("ZZ"), TaxKind: new("example"), AuthorityPartyID: &authority.PartyID, Kind: "notice", Title: "Example", Currency: new("USD"), Direction: "payable", Amount: new("12.34"), PeriodEnd: new("2026-12-31"), DueDate: new("2027-01-31"), LeadDays: new(int64(7)), FormCode: "EXAMPLE"}
	one, err := s.CreateTaxRecord(ctx, input)
	require.NoError(t, err)
	require.Equal(t, &authority.PartyID, one.AuthorityPartyID)
	require.NotNil(t, one.AmountMinor)
	require.Equal(t, int64(1234), *one.AmountMinor)
	other, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Other Example Agency"})
	require.NoError(t, err)
	input.AuthorityPartyID = &other.PartyID
	input.TaxYear = one.TaxYear
	one, err = s.UpdateTaxRecord(ctx, one.RecordID, one.Revision, input)
	require.NoError(t, err)
	assert.Equal(t, &other.PartyID, one.AuthorityPartyID)
	input.AuthorityPartyID = nil
	input.AuthoritySet = true
	input.Direction = "unknown"
	input.Amount = nil
	input.PeriodEnd = nil
	input.DueDate = nil
	input.LeadDays = nil
	input.FormCode = ""
	input.TaxYear = nil
	one, err = s.UpdateTaxRecord(ctx, one.RecordID, one.Revision, input)
	require.NoError(t, err)
	assert.Nil(t, one.AuthorityPartyID)
	assert.Nil(t, one.AmountMinor)
	assert.Nil(t, one.PeriodEnd)
	assert.Nil(t, one.DueDate)
	assert.Nil(t, one.LeadDays)
	assert.Empty(t, one.FormCode)
	assert.Nil(t, one.TaxYear)
	attached := TaxRecordInput{AccountID: &a.AccountID, AuthorityPartyID: &authority.PartyID, Kind: "notice", Title: "Mismatch"}
	_, err = s.CreateTaxRecord(ctx, attached)
	require.ErrorIs(t, err, taxrecord.ErrInvalid)
	require.ErrorContains(t, err, "account identity mismatch")
	attached.Kind = r.Kind
	_, err = s.UpdateTaxRecord(ctx, r.RecordID, r.Revision, attached)
	require.ErrorIs(t, err, taxrecord.ErrInvalid)
	field, ok := errors.AsType[*taxrecord.FieldError](err)
	require.True(t, ok)
	assert.Equal(t, "authority_party_id", field.Field)
	assert.Equal(t, "account identity mismatch", field.Reason)
	unchanged, err := s.GetTaxRecord(ctx, r.RecordID)
	require.NoError(t, err)
	assert.Equal(t, r.Revision, unchanged.Revision)
}

func TestTaxRecordUpdatePeriodCollision(t *testing.T) {
	t.Parallel()
	s, a, r := taxFixture(t)
	ctx := t.Context()
	input := TaxRecordInput{AccountID: &a.AccountID, Kind: "filing", Title: "Another", PeriodStart: new("2025-01-01"), PeriodEnd: new("2025-12-31")}
	other, err := s.CreateTaxRecord(ctx, input)
	require.NoError(t, err)
	input.PeriodStart, input.PeriodEnd = r.PeriodStart, r.PeriodEnd
	_, err = s.UpdateTaxRecord(ctx, other.RecordID, other.Revision, input)
	require.ErrorIs(t, err, ErrExists)
	got, err := s.GetTaxRecord(ctx, other.RecordID)
	require.NoError(t, err)
	assert.Equal(t, other, got)
}

func TestTaxSuccessfulNoOpUpdatesAdvanceRevision(t *testing.T) {
	t.Parallel()
	s, a, r := taxFixture(t)
	ctx := t.Context()
	ai := TaxAccountInput{FilerPartyID: a.FilerPartyID, Country: a.Country, TaxKind: a.TaxKind, Cadence: a.Cadence, Currency: a.Currency}
	updatedA, err := s.UpdateTaxAccount(ctx, a.AccountID, a.Revision, ai)
	require.NoError(t, err)
	assert.Equal(t, a.Revision+1, updatedA.Revision)
	ri := TaxRecordInput{AccountID: &a.AccountID, Kind: r.Kind, Title: r.Title, PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd, TaxYear: r.TaxYear}
	updatedR, err := s.UpdateTaxRecord(ctx, r.RecordID, r.Revision, ri)
	require.NoError(t, err)
	assert.Equal(t, r.Revision+1, updatedR.Revision)
}
