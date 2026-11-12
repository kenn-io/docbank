package store

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Omitting the transaction or revision fence would leave partial links or accept stale edits.
func checkContractLifecycle(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	own, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Owner", Own: true})
	require.NoError(t, err)
	other, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Counterparty"})
	require.NoError(t, err)
	input := ContractInput{Title: "Agreement", Status: "active", StartsOn: new("2024-02-29"), ExpiresOn: new("2024-03-01"), Parties: []ContractPartyInput{{PartyID: own.PartyID, Role: "own"}, {PartyID: other.PartyID, Role: "counterparty"}}}
	c, err := s.CreateContract(ctx, input)
	require.NoError(t, err)
	require.Len(t, c.Parties, 2)
	got, err := s.GetContract(ctx, c.ContractID, "2024-03-02")
	require.NoError(t, err)
	assert.Equal(t, "expired", got.EffectiveStatus)
	_, err = s.UpdateContract(ctx, c.ContractID, c.Revision-1, input)
	require.ErrorIs(t, err, ErrStaleRevision)
	require.NoError(t, s.DeleteContract(ctx, c.ContractID, c.Revision))
	_, err = s.GetContract(ctx, c.ContractID, "2024-03-02")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestContractLifecycle(t *testing.T) { t.Parallel(); checkContractLifecycle(t, newTestStore) }

// These cases catch invalid values being persisted and incorrect admission of an own party.
func TestContractRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreateParty(ctx, PartyInput{Kind: "person", DisplayName: "Other"})
	require.NoError(t, err)
	for _, input := range []ContractInput{
		{Title: ""},
		{Title: "Invalid date", StartsOn: new("2023-02-29")},
		{Title: "Reverse dates", StartsOn: new("2024-03-02"), ExpiresOn: new("2024-03-01")},
		{Title: "Long term", Summary: strings.Repeat("x", 8193)},
		{Title: "Wrong owner", Parties: []ContractPartyInput{{PartyID: p.PartyID, Role: "own"}}},
		{Title: "Duplicate", Parties: []ContractPartyInput{{PartyID: p.PartyID, Role: "counterparty"}, {PartyID: p.PartyID, Role: "counterparty"}}},
	} {
		_, err := s.CreateContract(ctx, input)
		require.Error(t, err)
	}
	items, err := s.ListContracts(ctx, ContractListOptions{AsOf: "2024-03-01", Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, items)
}

// Filters use effective status and literal title matching on both backends.
func checkContractFilters(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	for _, in := range []ContractInput{
		{Title: "Rate 100%", Status: "active", ExpiresOn: new("2024-03-01")},
		{Title: "Rate 100x", Status: "active", ExpiresOn: new("2024-03-02")},
		{Title: "Draft", Status: "draft", ExpiresOn: new("2024-02-29")},
	} {
		_, err := s.CreateContract(ctx, in)
		require.NoError(t, err)
	}
	got, err := s.ListContracts(ctx, ContractListOptions{AsOf: "2024-03-02", Status: "expired", Limit: 100})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Rate 100%", got[0].Title)
	got, err = s.ListContracts(ctx, ContractListOptions{AsOf: "2024-03-02", Q: "100%", Limit: 100})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Rate 100%", got[0].Title)
	got, err = s.ListContracts(ctx, ContractListOptions{AsOf: "2024-03-02", ExpiringBefore: "2024-03-01", Limit: 100})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Draft", got[0].Title)
}
func TestContractFilters(t *testing.T) { t.Parallel(); checkContractFilters(t, newTestStore) }

// Retiring a party does not invalidate a retained link or an idempotent PUT.
func checkContractPartyHistory(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	p, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Owner", Own: true})
	require.NoError(t, err)
	c, err := s.CreateContract(ctx, ContractInput{Title: "Historical", Parties: []ContractPartyInput{{PartyID: p.PartyID, Role: "own"}}})
	require.NoError(t, err)
	_, err = s.RetireParty(ctx, p.PartyID, p.Revision)
	require.NoError(t, err)
	same, err := s.AttachContractParty(ctx, c.ContractID, c.Revision, p.PartyID, "own")
	require.NoError(t, err)
	assert.Equal(t, c.Revision, same.Revision)
	_, err = s.AttachContractParty(ctx, c.ContractID, c.Revision, p.PartyID, "counterparty")
	require.ErrorIs(t, err, ErrPartyRetired)
	changed, err := s.DetachContractParty(ctx, c.ContractID, c.Revision, p.PartyID, "own")
	require.NoError(t, err)
	assert.Equal(t, c.Revision+1, changed.Revision)
	_, err = s.DetachContractParty(ctx, c.ContractID, changed.Revision, p.PartyID, "own")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.AttachContractParty(ctx, c.ContractID, c.Revision, p.PartyID, "own")
	require.ErrorIs(t, err, ErrStaleRevision)
}
func TestContractPartyHistory(t *testing.T) { t.Parallel(); checkContractPartyHistory(t, newTestStore) }

func TestContractMetadataRoundTrip(t *testing.T) {
	t.Parallel()
	source := newTestStore(t)
	ctx := t.Context()
	p, err := source.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Owner", Own: true})
	require.NoError(t, err)
	c, err := source.CreateContract(ctx, ContractInput{Title: "Agreement", Parties: []ContractPartyInput{{PartyID: p.PartyID, Role: "own"}}})
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &buf))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(buf.Bytes())))
	got, err := target.GetContract(ctx, c.ContractID, "2024-01-01")
	require.NoError(t, err)
	require.Len(t, got.Parties, 1)
	assert.Equal(t, p.PartyID, got.Parties[0].PartyID)
	malformed := strings.Replace(buf.String(), `"role":"own"`, `"role":"unknown"`, 1)
	require.NotEqual(t, buf.String(), malformed)
	empty := newTestStore(t)
	require.Error(t, empty.ImportMetadata(ctx, strings.NewReader(malformed)))
}

// Two writers with the same revision cannot both change the aggregate.
func TestContractConcurrentRevisionFence(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	c, err := s.CreateContract(ctx, ContractInput{Title: "Before"})
	require.NoError(t, err)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, title := range []string{"First", "Second"} {
		wg.Go(func() {
			<-start
			_, err := s.UpdateContract(ctx, c.ContractID, c.Revision, ContractInput{Title: title, Status: "draft"})
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	success, stale := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrStaleRevision) {
			stale++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, success)
	assert.Equal(t, 1, stale)
}
func TestContractAuditRefusal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	c, err := s.CreateContract(ctx, ContractInput{Title: "Before"})
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	_, err = s.CreateContract(ctx, ContractInput{Title: "After"})
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.UpdateContract(ctx, c.ContractID, c.Revision, ContractInput{Title: "Changed", Status: "draft"})
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	require.ErrorIs(t, s.DeleteContract(ctx, c.ContractID, c.Revision), ErrAuditMutationUnsupported)
}
