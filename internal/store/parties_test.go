package store

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func checkPartyLifecycle(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	input := PartyInput{Kind: "organization", DisplayName: "Northwind Storage", Country: new("CA"), Own: true}
	created, err := s.CreateParty(ctx, input)
	require.NoError(t, err)
	assert.Equal(t, int64(1), created.Revision)
	got, err := s.GetParty(ctx, created.PartyID)
	require.NoError(t, err)
	assert.Equal(t, created, got)

	input.Country = new("CA") // Equal value, different pointer.
	unchanged, err := s.UpdateParty(ctx, created.PartyID, created.Revision, input)
	require.NoError(t, err)
	assert.Equal(t, created, unchanged)
	_, err = s.UpdateParty(ctx, created.PartyID, created.Revision+1, input)
	require.ErrorIs(t, err, ErrStaleRevision)

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
	_, err = s.UpdateParty(ctx, retired.PartyID, retired.Revision, input)
	require.ErrorIs(t, err, ErrPartyRetired)
	_, err = s.RetireParty(ctx, retired.PartyID, retired.Revision)
	require.ErrorIs(t, err, ErrPartyRetired)
	first, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Café"})
	require.NoError(t, err)
	_, err = s.CreateParty(ctx, PartyInput{Kind: "person", DisplayName: "CAFE\u0301"})
	var exists *PartyExistsError
	require.ErrorAs(t, err, &exists)
	assert.Equal(t, first.PartyID, exists.PartyID)
}

func TestPartyValidatesInputsBeforeWriting(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, input := range []PartyInput{
		{Kind: "unknown", DisplayName: "Example"},
		{Kind: "person", DisplayName: ""},
		{Kind: "person", DisplayName: "Example", Country: new("ZZ")},
		{Kind: "person", DisplayName: "Example", PersonID: new("not-a-uuid")},
		{Kind: "person", DisplayName: "Bad\x00name"},
		{Kind: "person", DisplayName: "Valid", LegalName: "Bad\x00name"},
		{Kind: "person", DisplayName: "Bad\nname"},
		{Kind: "person", DisplayName: "Bad\x1bname"},
		{Kind: "person", DisplayName: "Bad\tname"},
		{Kind: "person", DisplayName: "Bad\u0085name"},
		{Kind: "person", DisplayName: "Valid", LegalName: "Bad\nname"},
		{Kind: "person", DisplayName: "Valid", LegalName: "Bad\x1bname"},
		{Kind: "person", DisplayName: "Valid", LegalName: "Bad\tname"},
		{Kind: "person", DisplayName: "Valid", LegalName: "Bad\u0085name"},
		{Kind: "person", DisplayName: strings.Repeat("界", 201)},
		{Kind: "person", DisplayName: "Another", LegalName: strings.Repeat("界", 301)},
	} {
		_, err := s.CreateParty(t.Context(), input)
		require.Error(t, err)
	}
	parties, err := s.ListParties(t.Context(), PartyListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, parties)
	_, err = s.CreateParty(t.Context(), PartyInput{Kind: "person", DisplayName: strings.Repeat("界", 200), LegalName: strings.Repeat("界", 300)})
	require.NoError(t, err)
}

func checkPartyConcurrentNameAdmission(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
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
	// A clock correction may put creation or retirement after the latest update.
	_, err = source.db.ExecContext(ctx, `UPDATE parties SET created_at=$1, retired_at=$2 WHERE party_id=$3`,
		"2099-01-01T00:00:00.000000000Z", "2098-01-01T00:00:00.000000000Z", p.PartyID)
	require.NoError(t, err)
	p, err = source.GetParty(ctx, p.PartyID)
	require.NoError(t, err)
	var backup bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &backup))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	got, err := target.GetParty(ctx, p.PartyID)
	require.NoError(t, err)
	assert.Equal(t, p, got)
	lines := strings.Split(backup.String(), "\n")
	partyLine := -1
	for i, line := range lines {
		if strings.Contains(line, `"type":"party"`) {
			partyLine = i
			break
		}
	}
	require.NotEqual(t, -1, partyLine)
	duplicate := append([]string(nil), lines...)
	duplicate[partyLine] = strings.Replace(duplicate[partyLine], `"retired_at":"2098-01-01T00:00:00.000000000Z"`, `"retired_at":null`, 1)
	duplicateLine := strings.Replace(duplicate[partyLine], p.PartyID, uuid.NewString(), 1)
	require.NotEqual(t, duplicate[partyLine], duplicateLine)
	duplicate = append(duplicate[:partyLine+1], append([]string{duplicateLine}, duplicate[partyLine+1:]...)...)
	empty := newTestStore(t)
	require.Error(t, empty.ImportMetadata(ctx, strings.NewReader(strings.Join(duplicate, "\n"))))
	for _, tc := range []struct{ name, from, to string }{
		{"country", `"country":"CA"`, `"country":"ZZ"`},
		{"created time", `"created_at":"2099-01-01T00:00:00.000000000Z"`, `"created_at":"invalid"`},
		{"retired time", `"retired_at":"2098-01-01T00:00:00.000000000Z"`, `"retired_at":"invalid"`},
		{"NUL", `"display_name":"Example"`, `"display_name":"Exam\u0000ple"`},
		{"control", `"legal_name":""`, `"legal_name":"Bad\u001bname"`},
		{"own", `"own":1`, `"own":2`},
		{"folded", `"display_name_folded":"example"`, `"display_name_folded":"wrong"`},
		{"missing person", `"person_id":null`, `"person_id":"11111111-1111-4111-8111-111111111111"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := append([]string(nil), lines...)
			mutated[partyLine] = strings.Replace(mutated[partyLine], tc.from, tc.to, 1)
			require.NotEqual(t, lines[partyLine], mutated[partyLine])
			empty := newTestStore(t)
			require.Error(t, empty.ImportMetadata(t.Context(), strings.NewReader(strings.Join(mutated, "\n"))))
			items, err := empty.ListParties(t.Context(), PartyListOptions{Limit: 100})
			require.NoError(t, err)
			assert.Empty(t, items)
		})
	}
}

func checkPartyFiltersAndReferences(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Synthetic Person", "operator")
	require.NoError(t, err)
	own := true
	linked, err := s.CreateParty(ctx, PartyInput{Kind: "person", DisplayName: "Linked Person", PersonID: &person.PersonID, Own: true})
	require.NoError(t, err)
	other, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Other"})
	require.NoError(t, err)
	items, err := s.ListParties(ctx, PartyListOptions{Kind: "person", Own: &own, Limit: 100})
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, linked.PartyID, items[0].PartyID)
	items, err = s.ListParties(ctx, PartyListOptions{Limit: 1, Offset: 1})
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, other.PartyID, items[0].PartyID)
	_, err = s.CreateParty(ctx, PartyInput{Kind: "person", DisplayName: "Missing", PersonID: new("11111111-1111-4111-8111-111111111111")})
	require.ErrorIs(t, err, ErrInvalidParty)
}
func checkPartyPersonLifecycle(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	survivor, err := s.CreatePerson(ctx, "Survivor", "operator")
	require.NoError(t, err)
	absorbed, err := s.CreatePerson(ctx, "Absorbed", "operator")
	require.NoError(t, err)
	input := PartyInput{Kind: "person", DisplayName: "Linked", PersonID: &absorbed.PersonID}
	p, err := s.CreateParty(ctx, input)
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, survivor.PersonID, absorbed.PersonID, uuid.NewString(), survivor.Revision, absorbed.Revision)
	require.NoError(t, err)
	moved, err := s.GetParty(ctx, p.PartyID)
	require.NoError(t, err)
	require.Equal(t, &survivor.PersonID, moved.PersonID)
	require.Equal(t, p.Revision+1, moved.Revision)
	_, err = s.UpdateParty(ctx, p.PartyID, p.Revision, input)
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.RetirePerson(ctx, survivor.PersonID, survivor.Revision+1)
	require.NoError(t, err)
	input.PersonID = &survivor.PersonID
	input.LegalName = "Historical link"
	_, err = s.UpdateParty(ctx, p.PartyID, moved.Revision, input)
	require.NoError(t, err)
	_, err = s.CreateParty(ctx, PartyInput{Kind: "person", DisplayName: "New link", PersonID: &survivor.PersonID})
	require.ErrorIs(t, err, ErrInvalidParty)

}
