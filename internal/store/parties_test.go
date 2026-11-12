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

func checkPartyLifecycle(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	input := PartyInput{Kind: "organization", DisplayName: "Northwind Storage", Own: true}
	created, err := s.CreateParty(ctx, input)
	require.NoError(t, err)
	assert.Equal(t, int64(1), created.Revision)
	got, err := s.GetParty(ctx, created.PartyID)
	require.NoError(t, err)
	assert.Equal(t, created, got)

	input.LegalName = "Northwind Storage Ltd"
	updated, err := s.UpdateParty(ctx, created.PartyID, created.Revision, input)
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.Revision)
	_, err = s.UpdateParty(ctx, created.PartyID, created.Revision, input)
	require.ErrorIs(t, err, ErrStaleRevision)

	retired, err := s.RetireParty(ctx, created.PartyID, updated.Revision)
	require.NoError(t, err)
	require.NotNil(t, retired.RetiredAt)
	listed, err := s.ListParties(ctx, PartyListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, listed)
	listed, err = s.ListParties(ctx, PartyListOptions{Limit: 100, IncludeRetired: true})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, retired, listed[0])
}

func TestPartyLifecycle(t *testing.T) {
	t.Parallel()
	checkPartyLifecycle(t, newTestStore)
}

func TestPartyDuplicateFoldedName(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	first, err := s.CreateParty(t.Context(), PartyInput{Kind: "organization", DisplayName: "Café"})
	require.NoError(t, err)
	_, err = s.CreateParty(t.Context(), PartyInput{Kind: "person", DisplayName: "CAFE\u0301"})
	require.Error(t, err)
	assert.ErrorContains(t, err, first.PartyID)
}

func TestPartyValidatesInputsBeforeWriting(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, input := range []PartyInput{
		{Kind: "unknown", DisplayName: "Example"},
		{Kind: "person", DisplayName: ""},
		{Kind: "person", DisplayName: "Example", Country: new("ZZ")},
		{Kind: "person", DisplayName: "Example", PersonID: new("not-a-uuid")},
	} {
		_, err := s.CreateParty(t.Context(), input)
		require.Error(t, err)
	}
	parties, err := s.ListParties(t.Context(), PartyListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, parties)
}

func TestPartyConcurrentNameAdmission(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, name := range []string{"Example Ltd", "EXAMPLE LTD"} {
		wg.Go(func() {
			<-start
			_, err := s.CreateParty(t.Context(), PartyInput{Kind: "organization", DisplayName: name})
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	success, duplicate := 0, 0
	for err := range results {
		var exists *PartyExistsError
		if err == nil {
			success++
		} else if errors.As(err, &exists) {
			duplicate++
		} else {
			t.Fatalf("unexpected create error: %v", err)
		}
	}
	assert.Equal(t, 1, success)
	assert.Equal(t, 1, duplicate)
}

func TestPartyUnicodeBounds(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.CreateParty(t.Context(), PartyInput{Kind: "person", DisplayName: strings.Repeat("界", 200), LegalName: strings.Repeat("界", 300)})
	require.NoError(t, err)
	_, err = s.CreateParty(t.Context(), PartyInput{Kind: "person", DisplayName: strings.Repeat("界", 201)})
	require.ErrorIs(t, err, ErrInvalidParty)
	_, err = s.CreateParty(t.Context(), PartyInput{Kind: "person", DisplayName: "Another", LegalName: strings.Repeat("界", 301)})
	require.ErrorIs(t, err, ErrInvalidParty)
}

func TestPartyAuditRefusalPreservesAuthority(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	input := PartyInput{Kind: "organization", DisplayName: "Example"}
	p, err := s.CreateParty(ctx, input)
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	_, err = s.CreateParty(ctx, PartyInput{Kind: "person", DisplayName: "Another"})
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.UpdateParty(ctx, p.PartyID, p.Revision, input)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.RetireParty(ctx, p.PartyID, p.Revision)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	got, err := s.GetParty(ctx, p.PartyID)
	require.NoError(t, err)
	assert.Equal(t, p, got)
}

func TestPartyMetadataRoundTripAndMalformedInput(t *testing.T) {
	t.Parallel()
	source := newTestStore(t)
	ctx := t.Context()
	p, err := source.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Example", Country: new("CA"), Own: true})
	require.NoError(t, err)
	var backup bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &backup))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	got, err := target.GetParty(ctx, p.PartyID)
	require.NoError(t, err)
	assert.Equal(t, p, got)
	malformed := strings.Replace(backup.String(), `"country":"CA"`, `"country":"ZZ"`, 1)
	require.NotEqual(t, backup.String(), malformed)
	empty := newTestStore(t)
	require.Error(t, empty.ImportMetadata(ctx, strings.NewReader(malformed)))
	items, err := empty.ListParties(ctx, PartyListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, items)
}
