package store

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const savedQueryFullExpression = `{
  "text": "tag:urgent AND (budget OR forecast)",
  "syntax": "advanced",
  "mode": "hybrid",
  "filters": {
    "paths": ["/zeta", "/alpha"],
    "tag_ids": ["11111111-1111-4111-8111-111111111111"],
    "modified_after": "2026-01-02T03:04:05+02:00",
    "size_min": 42
  },
  "sort": {"field": "modified_at", "direction": "desc"}
}`

const savedQueryFullCanonical = `{"filters":{"modified_after":"2026-01-02T01:04:05Z","paths":["/alpha","/zeta"],"size_min":42,"tag_ids":["11111111-1111-4111-8111-111111111111"]},"mode":"hybrid","sort":{"direction":"desc","field":"modified_at"},"syntax":"advanced","text":"tag:urgent AND (budget OR forecast)","v":1}`

const savedHighlightPayload = `{"v":1,"terms":[{"text":"literal <mark>","color":"#aabbcc"}]}`

func oversizedSavedQueryPayload() []byte {
	return []byte(`{"filters":{"paths":["/` + strings.Repeat("x", 70_000) + `"]}}`)
}

func TestSavedQueryLifecycleCanonicalizesAndFencesRevisions(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	created, err := s.CreateSavedQuery(ctx, " Cafe\u0301 budget ", "Quarterly\nreview", SavedQueryKindQuery, []byte(savedQueryFullExpression))
	require.NoError(t, err)
	require.NoError(t, validateUUIDv4(created.ID))
	assert.Equal(t, " Café budget ", created.Name)
	assert.Equal(t, "Quarterly\nreview", created.Description)
	assert.Equal(t, SavedQueryKindQuery, created.Kind)
	if got := string(created.Payload); got != savedQueryFullCanonical {
		t.Fatalf("canonical payload = %s, want %s", got, savedQueryFullCanonical)
	}
	assert.Equal(t, "sha256:51714bec95fea104f4ba243c0944afadf224544fc6b0aee383f574f222eefa56", created.Fingerprint)
	assert.Equal(t, int64(1), created.Revision)
	assert.NotEmpty(t, created.CreatedAt)
	assert.Equal(t, created.CreatedAt, created.UpdatedAt)

	got, err := s.SavedQueryByID(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, created, got)

	equivalent := []byte(`{"sort":{"direction":"desc","field":"modified_at"},"filters":{"size_min":42,"modified_after":"2026-01-02T01:04:05Z","tag_ids":["11111111-1111-4111-8111-111111111111"],"paths":["/alpha","/zeta"]},"mode":"hybrid","syntax":"advanced","text":"tag:urgent AND (budget OR forecast)","v":1}`)
	unchanged, err := s.UpdateSavedQuery(ctx, created.ID, created.Revision, SavedQueryPatch{
		Name:        new(created.Name),
		Description: new(created.Description),
		Payload:     new(equivalent),
	})
	require.NoError(t, err)
	assert.Equal(t, created, unchanged, "a canonical no-op must preserve its revision and timestamps")

	updated, err := s.UpdateSavedQuery(ctx, created.ID, created.Revision, SavedQueryPatch{
		Description: new("Reviewed"),
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.Revision)
	assert.Equal(t, created.CreatedAt, updated.CreatedAt)
	assert.Equal(t, "Reviewed", updated.Description)
	updated, err = s.UpdateSavedQuery(ctx, created.ID, updated.Revision, SavedQueryPatch{
		Name:    new(" Cafe\u0301 forecast "),
		Payload: new([]byte(`{"text":"forecast","mode":"semantic"}`)),
	})
	require.NoError(t, err)
	assert.Equal(t, int64(3), updated.Revision)
	assert.Equal(t, " Café forecast ", updated.Name)
	const updatedCanonical = `{"filters":{},"mode":"semantic","sort":{"direction":"asc","field":"name"},"syntax":"simple","text":"forecast","v":1}`
	if got := string(updated.Payload); got != updatedCanonical {
		t.Fatalf("updated canonical payload = %s, want %s", got, updatedCanonical)
	}
	assert.NotEqual(t, created.Fingerprint, updated.Fingerprint)

	_, err = s.UpdateSavedQuery(ctx, created.ID, created.Revision, SavedQueryPatch{
		Name: new("stale mutation"),
	})
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.DeleteSavedQuery(ctx, created.ID, created.Revision)
	require.ErrorIs(t, err, ErrStaleRevision)
	stillCurrent, err := s.SavedQueryByID(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, updated, stillCurrent, "stale writes must leave authority unchanged")

	deleted, err := s.DeleteSavedQuery(ctx, created.ID, updated.Revision)
	require.NoError(t, err)
	assert.Equal(t, updated, deleted)
	_, err = s.SavedQueryByID(ctx, created.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSavedQueriesAreBoundedFilteredAndNameSorted(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, err := s.CreateSavedQuery(ctx, "beta", "", SavedQueryKindQuery, []byte(`{"text":"b"}`))
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "alpha", "", SavedQueryKindHighlightSet, []byte(savedHighlightPayload))
	require.NoError(t, err)

	page, total, err := s.SavedQueries(ctx, "", 1, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	require.Len(t, page, 1)
	assert.Equal(t, "alpha", page[0].Name)

	page, total, err = s.SavedQueries(ctx, SavedQueryKindQuery, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, page, 1)
	assert.Equal(t, "beta", page[0].Name)

	for _, test := range []struct {
		kind          string
		limit, offset int
	}{
		{limit: 0},
		{limit: maxSavedQueryPageSize + 1},
		{limit: 1, offset: -1},
		{kind: "unknown", limit: 1},
	} {
		_, _, err = s.SavedQueries(ctx, test.kind, test.limit, test.offset)
		require.ErrorIs(t, err, ErrInvalidSavedQuery)
	}
}

func TestSavedQueryValidationAndNameCollisions(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateSavedQuery(ctx, "café", "", SavedQueryKindQuery, []byte(`{"text":"one"}`))
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "cafe\u0301", "", SavedQueryKindQuery, []byte(`{"text":"two"}`))
	require.ErrorIs(t, err, ErrExists)
	second, err := s.CreateSavedQuery(ctx, "second", "", SavedQueryKindQuery, []byte(`{"text":"two"}`))
	require.NoError(t, err)
	_, err = s.UpdateSavedQuery(ctx, second.ID, second.Revision, SavedQueryPatch{Name: new(first.Name)})
	require.ErrorIs(t, err, ErrExists)
	secondAfter, err := s.SavedQueryByID(ctx, second.ID)
	require.NoError(t, err)
	assert.Equal(t, second, secondAfter)
	boundary, err := s.CreateSavedQuery(ctx, strings.Repeat("x", 256),
		strings.Repeat("y", 4094)+"\t\n", SavedQueryKindQuery, []byte(`{"text":"boundary"}`))
	require.NoError(t, err)
	assert.Len(t, boundary.Name, 256)
	assert.Len(t, boundary.Description, 4096)

	longName := strings.Repeat("é", 129)
	longDescription := strings.Repeat("x", 4097)
	for _, test := range []struct {
		name, description, kind, payload string
	}{
		{name: "", kind: SavedQueryKindQuery, payload: `{"text":"ok"}`},
		{name: " \t\n", kind: SavedQueryKindQuery, payload: `{"text":"ok"}`},
		{name: "bad\u0000name", kind: SavedQueryKindQuery, payload: `{"text":"ok"}`},
		{name: string([]byte{0xff}), kind: SavedQueryKindQuery, payload: `{"text":"ok"}`},
		{name: longName, kind: SavedQueryKindQuery, payload: `{"text":"ok"}`},
		{name: "description", description: longDescription, kind: SavedQueryKindQuery, payload: `{"text":"ok"}`},
		{name: "description-control", description: "bad\rvalue", kind: SavedQueryKindQuery, payload: `{"text":"ok"}`},
		{name: "unknown-kind", kind: "unknown", payload: `{"text":"ok"}`},
		{name: "invalid-query", kind: SavedQueryKindQuery, payload: `{"text":"a","text":"b"}`},
		{name: "query-kind-mismatch", kind: SavedQueryKindQuery, payload: savedHighlightPayload},
		{name: "highlight-kind-mismatch", kind: SavedQueryKindHighlightSet, payload: `{"text":"ok"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, createErr := s.CreateSavedQuery(ctx, test.name, test.description, test.kind, []byte(test.payload))
			require.ErrorIs(t, createErr, ErrInvalidSavedQuery)
		})
	}

	invalidName := string([]byte{0xff})
	_, err = s.UpdateSavedQuery(ctx, first.ID, first.Revision, SavedQueryPatch{Name: &invalidName})
	require.ErrorIs(t, err, ErrInvalidSavedQuery)
	current, err := s.SavedQueryByID(ctx, first.ID)
	require.NoError(t, err)
	assert.Equal(t, first, current)
}

func TestSavedQueryPolicyValidationDoesNotMutateAuthority(t *testing.T) {
	s := newTestStore(t)
	created, err := s.CreateSavedQuery(t.Context(), "stable", "before",
		SavedQueryKindQuery, []byte(`{"text":"stable"}`))
	require.NoError(t, err)

	_, err = s.CreateSavedQuery(t.Context(), "unknown", "", "future_kind", []byte(`{}`))
	require.ErrorIs(t, err, ErrInvalidSavedQuery)
	_, err = s.CreateSavedQuery(t.Context(), "oversized", "", SavedQueryKindQuery,
		oversizedSavedQueryPayload())
	require.ErrorIs(t, err, ErrInvalidSavedQuery)
	_, err = s.UpdateSavedQuery(t.Context(), created.ID, created.Revision,
		SavedQueryPatch{Payload: new(oversizedSavedQueryPayload())})
	require.ErrorIs(t, err, ErrInvalidSavedQuery)

	after, err := s.SavedQueryByID(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, created, after)
	items, total, err := s.SavedQueries(t.Context(), "", 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, items, 1)
	assert.Equal(t, created, items[0])
}

func TestSavedQueryConcurrentSameNameCreateHasOneWinner(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			_, err := s.CreateSavedQuery(ctx, "same", "", SavedQueryKindQuery, []byte(`{"text":"same"}`))
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)

	winners, collisions := 0, 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrExists):
			collisions++
		default:
			require.NoError(t, err)
		}
	}
	assert.Equal(t, 1, winners)
	assert.Equal(t, 1, collisions)
}

func TestSavedQueryConcurrentSameRevisionEditHasOneWinner(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	created, err := s.CreateSavedQuery(ctx, "race", "", SavedQueryKindQuery, []byte(`{"text":"before"}`))
	require.NoError(t, err)

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, description := range []string{"first", "second"} {
		wg.Go(func() {
			<-start
			_, err := s.UpdateSavedQuery(ctx, created.ID, created.Revision, SavedQueryPatch{
				Description: &description,
			})
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)

	winners, stale := 0, 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrStaleRevision):
			stale++
		default:
			require.NoError(t, err)
		}
	}
	assert.Equal(t, 1, winners)
	assert.Equal(t, 1, stale)
	current, err := s.SavedQueryByID(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), current.Revision)
}

func TestSavedQueryMutationsRejectActiveAuditAuthorityWithoutHidingReads(t *testing.T) {
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStoreWithDriver(t, test.driver)
			created, err := s.CreateSavedQuery(t.Context(), "existing", "before",
				SavedQueryKindQuery, []byte(`{"text":"existing"}`))
			require.NoError(t, err)
			seedInitialAuditAuthority(t, s, s.RootID())

			_, err = s.CreateSavedQuery(t.Context(), "blocked", "", SavedQueryKindQuery,
				[]byte(`{"text":"blocked"}`))
			require.ErrorIs(t, err, ErrAuditMutationUnsupported)
			_, err = s.UpdateSavedQuery(t.Context(), created.ID, created.Revision,
				SavedQueryPatch{Description: new("blocked")})
			require.ErrorIs(t, err, ErrAuditMutationUnsupported)
			_, err = s.DeleteSavedQuery(t.Context(), created.ID, created.Revision)
			require.ErrorIs(t, err, ErrAuditMutationUnsupported)

			read, err := s.SavedQueryByID(t.Context(), created.ID)
			require.NoError(t, err)
			assert.Equal(t, created, read)
			page, total, err := s.SavedQueries(t.Context(), "", 10, 0)
			require.NoError(t, err)
			assert.Equal(t, 1, total)
			require.Len(t, page, 1)
			assert.Equal(t, created, page[0])
		})
	}
}
