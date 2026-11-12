package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func checkTaxAccountLifecycle(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	filer, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Example Holdings Ltd.", Own: true})
	require.NoError(t, err)
	authority, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Example Revenue Agency"})
	require.NoError(t, err)
	input := TaxAccountInput{FilerPartyID: filer.PartyID, AuthorityPartyID: &authority.PartyID, Country: "ZZ", TaxKind: "example", Cadence: "annual", Currency: "USD"}
	account, err := s.CreateTaxAccount(ctx, input)
	require.NoError(t, err)
	assert.Equal(t, int64(14), account.LeadDays)
	input.Note = "Updated"
	updated, err := s.UpdateTaxAccount(ctx, account.AccountID, account.Revision, input)
	require.NoError(t, err)
	_, err = s.UpdateTaxAccount(ctx, account.AccountID, account.Revision, input)
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.RetireTaxAccount(ctx, account.AccountID, updated.Revision)
	require.NoError(t, err)
	rows, err := s.ListTaxAccounts(ctx, TaxAccountListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, rows)
}
func TestTaxAccountLifecycleDefaultsAndIdentity(t *testing.T) {
	t.Parallel()
	checkTaxAccountLifecycle(t, newTestStore)
}

func TestTaxMutationsRefuseAuditAuthority(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	filer, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Example Holdings Ltd.", Own: true})
	require.NoError(t, err)
	ai := TaxAccountInput{FilerPartyID: filer.PartyID, Country: "ZZ", TaxKind: "example", Cadence: "annual", Currency: "USD"}
	a, err := s.CreateTaxAccount(ctx, ai)
	require.NoError(t, err)
	ri := TaxRecordInput{AccountID: &a.AccountID, Kind: "filing", Title: "Example"}
	r, err := s.CreateTaxRecord(ctx, ri)
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	_, err = s.CreateTaxAccount(ctx, ai)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.UpdateTaxAccount(ctx, a.AccountID, a.Revision, ai)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.RetireTaxAccount(ctx, a.AccountID, a.Revision)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.CreateTaxRecord(ctx, ri)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.UpdateTaxRecord(ctx, r.RecordID, r.Revision, ri)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.RetireTaxRecord(ctx, r.RecordID, r.Revision)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	gotA, err := s.GetTaxAccount(ctx, a.AccountID)
	require.NoError(t, err)
	assert.Equal(t, a, gotA)
	gotR, err := s.GetTaxRecord(ctx, r.RecordID)
	require.NoError(t, err)
	assert.Equal(t, r, gotR)
}

func TestTaxNewAttachmentsAndHistoricalPartyChanges(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	pi := PartyInput{Kind: "organization", DisplayName: "Example Holdings Ltd.", Own: true}
	p, err := s.CreateParty(ctx, pi)
	require.NoError(t, err)
	ai := TaxAccountInput{FilerPartyID: p.PartyID, Country: "ZZ", TaxKind: "example", Cadence: "annual", Currency: "USD"}
	a, err := s.CreateTaxAccount(ctx, ai)
	require.NoError(t, err)
	ri := TaxRecordInput{AccountID: &a.AccountID, Kind: "filing", Title: "Example"}
	r, err := s.CreateTaxRecord(ctx, ri)
	require.NoError(t, err)
	pi.Own = false
	p, err = s.UpdateParty(ctx, p.PartyID, p.Revision, pi)
	require.NoError(t, err)
	_, err = s.CreateTaxRecord(ctx, ri)
	require.Error(t, err)
	ai.Note = "retained"
	_, err = s.UpdateTaxAccount(ctx, a.AccountID, a.Revision, ai)
	require.NoError(t, err)
	ri.Title = "retained"
	_, err = s.UpdateTaxRecord(ctx, r.RecordID, r.Revision, ri)
	require.NoError(t, err)
	_, err = s.RetireParty(ctx, p.PartyID, p.Revision)
	require.NoError(t, err)
	_, err = s.CreateTaxAccount(ctx, ai)
	require.Error(t, err)
}
