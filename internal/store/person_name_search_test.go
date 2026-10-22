package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPersonNameSearchFollowsAuthority(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "people.db")
	s, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	person, err := s.CreatePerson(ctx, "Anne Smith", "operator")
	require.NoError(t, err)
	person, err = s.UpdatePerson(ctx, person.PersonID, person.Revision, "Jo Brown")
	require.NoError(t, err)
	current, err := s.PeopleByDisplayName(ctx, "Brown", "", "", 10, true, PeopleSearchOptions{})
	require.NoError(t, err)
	require.Len(t, current, 1)
	require.Equal(t, person.PersonID, current[0].PersonID)
	old, err := s.PeopleByDisplayName(ctx, "Smith", "", "", 10, true, PeopleSearchOptions{})
	require.NoError(t, err)
	require.Empty(t, old)
}

func BenchmarkPeopleByDisplayNameWordStart(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "people.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, s.Close()) })
	_, err = s.db.ExecContext(b.Context(), `WITH RECURSIVE people(i) AS (
	 VALUES(1) UNION ALL SELECT i+1 FROM people WHERE i<100000
	) INSERT INTO persons(person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at)
	 SELECT printf('00000000-0000-4000-8000-%012d',i),
	 CASE WHEN i%100=0 THEN 'Synthetic Smith' ELSE printf('Synthetic Person%05d',i) END,
	 CASE WHEN i%100=0 THEN 'synthetic smith' ELSE printf('synthetic person%05d',i) END,
	 'operator','curated',1,?,? FROM people`, nowRFC3339(), nowRFC3339())
	require.NoError(b, err)
	for b.Loop() {
		people, err := s.PeopleByDisplayName(b.Context(), "Smith", "", "", 100, true, PeopleSearchOptions{})
		require.NoError(b, err)
		require.Len(b, people, 100)
	}
}
