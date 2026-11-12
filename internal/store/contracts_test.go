package store

import (
	"bytes"
	"errors"
	"math"
	"strconv"
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
	got, err := s.GetContract(ctx, c.ContractID, "2024-03-01")
	require.NoError(t, err)
	assert.Equal(t, "active", got.EffectiveStatus)
	got, err = s.GetContract(ctx, c.ContractID, "2024-03-02")
	require.NoError(t, err)
	assert.Equal(t, "expired", got.EffectiveStatus)
	_, err = s.UpdateContract(ctx, c.ContractID, c.Revision-1, input)
	require.ErrorIs(t, err, ErrStaleRevision)
	same, err := s.UpdateContract(ctx, c.ContractID, c.Revision, input)
	require.NoError(t, err)
	assert.Equal(t, c.Revision, same.Revision)
	require.ErrorIs(t, s.DeleteContract(ctx, c.ContractID, c.Revision-1), ErrStaleRevision)
	require.NoError(t, s.DeleteContract(ctx, c.ContractID, c.Revision))
	var links int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM contract_parties WHERE contract_id=$1`, c.ContractID).Scan(&links))
	assert.Zero(t, links)
	_, err = s.GetContract(ctx, c.ContractID, "2024-03-02")
	require.ErrorIs(t, err, ErrNotFound)
}

// These cases catch invalid values being persisted and incorrect admission of an own party.
func TestContractRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreateParty(ctx, PartyInput{Kind: "person", DisplayName: "Other"})
	require.NoError(t, err)
	for _, input := range []ContractInput{
		{Title: ""},
		{Title: "bad\nname"},
		{Title: "Invalid date", StartsOn: new("2023-02-29")},
		{Title: "Reverse dates", StartsOn: new("2024-03-02"), ExpiresOn: new("2024-03-01")},
		{Title: "Long term", Summary: strings.Repeat("x", 8193)},
		{Title: "Wrong owner", Parties: []ContractPartyInput{{PartyID: p.PartyID, Role: "own"}}},
		{Title: "Duplicate", Parties: []ContractPartyInput{{PartyID: p.PartyID, Role: "counterparty"}, {PartyID: p.PartyID, Role: "counterparty"}}},
	} {
		_, err := s.CreateContract(ctx, input)
		want := ErrInvalidContract
		if input.Title == "Wrong owner" {
			want = ErrPartyNotOwn
		}
		require.ErrorIs(t, err, want)
	}
	_, err = s.CreateContract(ctx, ContractInput{Title: "Private term", Purpose: strings.Repeat("secret", 1400)})
	require.ErrorContains(t, err, "purpose")
	require.ErrorContains(t, err, "8192")
	require.NotContains(t, err.Error(), "secret")
	items, err := s.ListContracts(ctx, ContractListOptions{AsOf: "2024-03-01", Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, items)
}

// Filters use effective status and literal title matching on both backends.
func checkContractFilters(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	p, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Supplier"})
	require.NoError(t, err)
	for _, in := range []ContractInput{
		{Title: "Rate 100%", ContractType: "lease", Status: "active", ExpiresOn: new("2024-03-01"), Parties: []ContractPartyInput{{PartyID: p.PartyID, Role: "counterparty"}}},
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
	got, err = s.ListContracts(ctx, ContractListOptions{AsOf: "2024-03-02", ContractType: "lease", Limit: 100})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Rate 100%", got[0].Title)
	got, err = s.ListContracts(ctx, ContractListOptions{AsOf: "2024-03-02", PartyID: p.PartyID, Limit: 100})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Rate 100%", got[0].Title)
	got, err = s.ListContracts(ctx, ContractListOptions{AsOf: "2024-03-02", Limit: 1})
	require.NoError(t, err)
	require.Len(t, got, 1)
	got, err = s.ListContracts(ctx, ContractListOptions{AsOf: "2024-03-02", ContractType: "lease", PartyID: p.PartyID, Limit: 1})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Rate 100%", got[0].Title)
	got, err = s.ListContracts(ctx, ContractListOptions{AsOf: "2024-03-02", ContractType: "lease", PartyID: p.PartyID, Limit: 1, Offset: 1})
	require.NoError(t, err)
	assert.Empty(t, got)
}

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
	_, err = s.DetachContractParty(ctx, c.ContractID, changed.Revision, p.PartyID, "invalid")
	require.ErrorIs(t, err, ErrInvalidContract)
	_, err = s.DetachContractParty(ctx, c.ContractID, changed.Revision, p.PartyID, "own")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.AttachContractParty(ctx, c.ContractID, c.Revision, p.PartyID, "own")
	require.ErrorIs(t, err, ErrStaleRevision)
}

func TestContractMetadataInvalidImport(t *testing.T) {
	t.Parallel()
	source := newTestStore(t)
	ctx := t.Context()
	p, err := source.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Owner", Own: true})
	require.NoError(t, err)
	c, err := source.CreateContract(ctx, ContractInput{Title: "Agreement", Parties: []ContractPartyInput{{PartyID: p.PartyID, Role: "own"}}})
	require.NoError(t, err)
	_, err = source.db.ExecContext(ctx, `UPDATE contracts SET created_at=$1, updated_at=$2 WHERE contract_id=$3`, "2099-01-01T00:00:00.000000000Z", "2000-01-01T00:00:00.000000000Z", c.ContractID)
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &buf))
	lines := strings.Split(buf.String(), "\n")
	contractLine := -1
	for i, line := range lines {
		if strings.Contains(line, `"type":"contract"`) {
			contractLine = i
			break
		}
	}
	require.NotEqual(t, -1, contractLine)
	for _, tc := range []struct{ name, from, to, want string }{
		{"title control", `"title":"Agreement"`, `"title":"Bad\u001btitle"`, "invalid contract: title"},
		{"created encoding", `"created_at":"2099-01-01T00:00:00.000000000Z"`, `"created_at":"invalid"`, "invalid contract created_at"},
		{"updated encoding", `"updated_at":"2000-01-01T00:00:00.000000000Z"`, `"updated_at":"2000-01-01T00:00:00+00:00"`, "invalid contract updated_at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := append([]string(nil), lines...)
			mutated[contractLine] = strings.Replace(mutated[contractLine], tc.from, tc.to, 1)
			require.NotEqual(t, lines[contractLine], mutated[contractLine])
			empty := newTestStore(t)
			require.ErrorContains(t, empty.ImportMetadata(t.Context(), strings.NewReader(strings.Join(mutated, "\n"))), tc.want)
			items, err := empty.ListContracts(t.Context(), ContractListOptions{AsOf: "2024-01-01", Limit: 100})
			require.NoError(t, err)
			assert.Empty(t, items)
		})
	}
	malformed := strings.Replace(buf.String(), `"role":"own"`, `"role":"unknown"`, 1)
	require.NotEqual(t, buf.String(), malformed)
	empty := newTestStore(t)
	require.ErrorContains(t, empty.ImportMetadata(ctx, strings.NewReader(malformed)), "invalid contract party metadata")
}

// Two writers with the same revision cannot both change the aggregate.
func checkContractConcurrentRevisionFence(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
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
	n, err := s.CreateFile(ctx, s.RootID(), "agreement.txt", fakeHash("a1"), 12, "text/plain")
	require.NoError(t, err)
	c, err = s.LinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "signed", "")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	_, err = s.CreateContract(ctx, ContractInput{Title: "After"})
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.UpdateContract(ctx, c.ContractID, c.Revision, ContractInput{Title: "Changed", Status: "draft"})
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	require.ErrorIs(t, s.DeleteContract(ctx, c.ContractID, c.Revision), ErrAuditMutationUnsupported)
	_, err = s.LinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "draft", "")
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.UnlinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "signed", "")
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
}

// checkContractRevisionExhaustion exercises contracts restored at the integer limit.
func checkContractRevisionExhaustion(t *testing.T, postgres bool) {
	t.Helper()
	for _, action := range []string{"update", "attach", "detach", "noop", "stale", "boundary"} {
		t.Run(action, func(t *testing.T) {
			ctx := t.Context()
			source := newTestStore(t)
			linked, err := source.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Linked"})
			require.NoError(t, err)
			other, err := source.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Other"})
			require.NoError(t, err)
			input := ContractInput{Title: "Agreement", Status: "draft", Parties: []ContractPartyInput{{PartyID: linked.PartyID, Role: "counterparty"}}}
			contract, err := source.CreateContract(ctx, input)
			require.NoError(t, err)
			var original bytes.Buffer
			require.NoError(t, source.ExportMetadata(ctx, &original))
			lines := strings.Split(original.String(), "\n")
			revision := int64(math.MaxInt64)
			if action == "boundary" {
				revision--
			}
			found := false
			for i, line := range lines {
				if strings.Contains(line, `"type":"contract"`) && strings.Contains(line, contract.ContractID) {
					lines[i] = strings.Replace(line, `"revision":1`, `"revision":`+strconv.FormatInt(revision, 10), 1)
					require.NotEqual(t, line, lines[i])
					found = true
				}
			}
			require.True(t, found)
			s := newTestStore(t)
			require.NoError(t, s.ImportMetadata(ctx, strings.NewReader(strings.Join(lines, "\n"))))
			if postgres {
				s, _ = copyRetentionStore(t, s)
			}
			before, err := s.GetContract(ctx, contract.ContractID, "2024-01-01")
			require.NoError(t, err)
			require.Equal(t, revision, before.Revision)
			var snapshot bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &snapshot))
			switch action {
			case "update", "boundary":
				input.Title = "Changed"
				_, err = s.UpdateContract(ctx, contract.ContractID, revision, input)
			case "attach":
				_, err = s.AttachContractParty(ctx, contract.ContractID, revision, other.PartyID, "counterparty")
			case "detach":
				_, err = s.DetachContractParty(ctx, contract.ContractID, revision, linked.PartyID, "counterparty")
			case "noop":
				var same Contract
				same, err = s.UpdateContract(ctx, contract.ContractID, revision, input)
				require.NoError(t, err)
				require.Equal(t, before, same)
				same, err = s.AttachContractParty(ctx, contract.ContractID, revision, linked.PartyID, "counterparty")
				require.NoError(t, err)
				require.Equal(t, before, same)
			case "stale":
				input.Title = "Changed"
				_, err = s.UpdateContract(ctx, contract.ContractID, revision-1, input)
				require.ErrorIs(t, err, ErrStaleRevision)
			}
			if action == "boundary" {
				require.NoError(t, err)
				got, getErr := s.GetContract(ctx, contract.ContractID, "2024-01-01")
				require.NoError(t, getErr)
				assert.Equal(t, int64(math.MaxInt64), got.Revision)
				assert.Equal(t, "Changed", got.Title)
				var exported bytes.Buffer
				require.NoError(t, s.ExportMetadata(ctx, &exported))
				return
			}
			if action == "update" || action == "attach" || action == "detach" {
				require.ErrorIs(t, err, ErrInvalidContract)
			}
			got, getErr := s.GetContract(ctx, contract.ContractID, "2024-01-01")
			if assert.NoError(t, getErr) {
				assert.Equal(t, before, got)
			}
			var after bytes.Buffer
			if assert.NoError(t, s.ExportMetadata(ctx, &after)) {
				assert.Equal(t, snapshot.Bytes(), after.Bytes())
			}
		})
	}
}
