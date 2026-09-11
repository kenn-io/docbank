package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuerySnapshotCachePagesAreOpaqueOwnedAndCallerIsolated(t *testing.T) {
	for _, driverCase := range walkTestDrivers() {
		t.Run(driverCase.name, func(t *testing.T) {
			s := newTestStoreWithDriver(t, driverCase.driver)
			for i := range 51 {
				name := strings.Repeat("0", 2-len(string(rune('0'+i/10)))) + string(rune('0'+i/10)) + string(rune('0'+i%10)) + ".txt"
				_, err := s.CreateFile(t.Context(), s.RootID(), name, fakeHash("cache-"+name), int64(i+1), "text/plain")
				require.NoError(t, err)
			}

			now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
			service := newQuerySnapshotService(s, querySnapshotServiceOptions{
				Now:     func() time.Time { return now },
				Rand:    bytes.NewReader(bytes.Repeat([]byte{0x11}, snapshotIDBytes)),
				HMACKey: bytes.Repeat([]byte{0x22}, 32),
			})
			t.Cleanup(func() { require.NoError(t, service.Close()) })

			first, err := service.Create(t.Context(), "owner-a", SnapshotRequest{
				Query: snapshotTestQuery(t, `{}`), PageSize: 50, Facets: []string{"size"},
			})
			require.NoError(t, err)
			assert.True(t, first.Snapshot)
			assert.Regexp(t, `^[0-9a-f]{32}$`, first.SnapshotID)
			assert.Equal(t, int64(51), first.Total)
			require.Len(t, first.Rows, 50)
			assert.Empty(t, first.PrevCursor)
			require.NotEmpty(t, first.NextCursor)
			assert.Equal(t, now.Add(querySnapshotIdleTTL), first.ExpiresAt)

			next, err := service.Page(t.Context(), "owner-a", first.SnapshotID, first.NextCursor)
			require.NoError(t, err)
			require.Len(t, next.Rows, 1)
			require.NotEmpty(t, next.PrevCursor)
			assert.Empty(t, next.NextCursor)

			first.Rows[0].Name = "mutated"
			first.Query.Text = "mutated"
			first.Facets[0].Dimension = "mutated"
			next.Rows[0].Tags = append(next.Rows[0].Tags, SnapshotTag{ID: "mutated"})
			again, err := service.Page(t.Context(), "owner-a", first.SnapshotID, next.PrevCursor)
			require.NoError(t, err)
			assert.NotEqual(t, "mutated", again.Rows[0].Name)
			assert.Empty(t, again.Query.Text)
			assert.Equal(t, "size", again.Facets[0].Dimension)

			members, err := service.CopyMembers(t.Context(), "owner-a", first.SnapshotID, first.MemberHash)
			require.NoError(t, err)
			require.Len(t, members, 51)
			members[0].BlobHash = "mutated"
			membersAgain, err := service.CopyMembers(t.Context(), "owner-a", first.SnapshotID, first.MemberHash)
			require.NoError(t, err)
			assert.NotEqual(t, "mutated", membersAgain[0].BlobHash)
			_, err = service.CopyMembers(t.Context(), "owner-a", first.SnapshotID, strings.Repeat("0", 64))
			require.ErrorIs(t, err, ErrSnapshotMemberHash)

			_, err = service.Page(t.Context(), "owner-b", first.SnapshotID, first.NextCursor)
			require.ErrorIs(t, err, ErrSnapshotGone)
			tampered := first.NextCursor[:len(first.NextCursor)-1] + "A"
			_, err = service.Page(t.Context(), "owner-a", first.SnapshotID, tampered)
			require.ErrorIs(t, err, ErrSnapshotCursor)
		})
	}
}

func TestQuerySnapshotCacheReservationsEvictionAndExpiry(t *testing.T) {
	t.Run("incremental row admission releases failed build", func(t *testing.T) {
		s := newTestStore(t)
		for _, name := range []string{"a.txt", "b.txt"} {
			_, err := s.CreateFile(t.Context(), s.RootID(), name, fakeHash("admit-"+name), 1, "text/plain")
			require.NoError(t, err)
		}
		service := newQuerySnapshotService(s, querySnapshotServiceOptions{
			Limits: snapshotCacheLimits{MaxRows: 1}, HMACKey: bytes.Repeat([]byte{1}, 32),
		})
		t.Cleanup(func() { require.NoError(t, service.Close()) })
		_, err := service.Create(t.Context(), "owner", SnapshotRequest{Query: snapshotTestQuery(t, `{}`)})
		require.ErrorIs(t, err, ErrSnapshotAdmission)
		assert.Equal(t, snapshotCacheStats{}, service.stats())
	})

	t.Run("complete least recently used handle is evicted", func(t *testing.T) {
		s := newTestStore(t)
		_, err := s.CreateFile(t.Context(), s.RootID(), "a.txt", fakeHash("lru"), 1, "text/plain")
		require.NoError(t, err)
		ids := append(bytes.Repeat([]byte{1}, snapshotIDBytes), bytes.Repeat([]byte{2}, snapshotIDBytes)...)
		service := newQuerySnapshotService(s, querySnapshotServiceOptions{
			Limits: snapshotCacheLimits{MaxHandles: 1}, Rand: bytes.NewReader(ids), HMACKey: bytes.Repeat([]byte{3}, 32),
		})
		t.Cleanup(func() { require.NoError(t, service.Close()) })
		first, err := service.Create(t.Context(), "owner-a", SnapshotRequest{Query: snapshotTestQuery(t, `{}`)})
		require.NoError(t, err)
		second, err := service.Create(t.Context(), "owner-b", SnapshotRequest{Query: snapshotTestQuery(t, `{}`)})
		require.NoError(t, err)
		_, err = service.CopyMembers(t.Context(), "owner-a", first.SnapshotID, first.MemberHash)
		require.ErrorIs(t, err, ErrSnapshotGone)
		_, err = service.CopyMembers(t.Context(), "owner-b", second.SnapshotID, second.MemberHash)
		require.NoError(t, err)
	})

	t.Run("idle and absolute deadlines are checked without sleeping", func(t *testing.T) {
		s := newTestStore(t)
		_, err := s.CreateFile(t.Context(), s.RootID(), "a.txt", fakeHash("ttl"), 1, "text/plain")
		require.NoError(t, err)
		now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
		service := newQuerySnapshotService(s, querySnapshotServiceOptions{
			Now: func() time.Time { return now }, HMACKey: bytes.Repeat([]byte{4}, 32),
		})
		t.Cleanup(func() { require.NoError(t, service.Close()) })
		page, err := service.Create(t.Context(), "owner", SnapshotRequest{Query: snapshotTestQuery(t, `{}`)})
		require.NoError(t, err)
		now = now.Add(querySnapshotIdleTTL + time.Nanosecond)
		_, err = service.CopyMembers(t.Context(), "owner", page.SnapshotID, page.MemberHash)
		require.ErrorIs(t, err, ErrSnapshotGone)
		assert.Equal(t, int64(0), service.stats().CachedRows)

		now = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
		absolute, err := service.Create(t.Context(), "owner", SnapshotRequest{Query: snapshotTestQuery(t, `{}`)})
		require.NoError(t, err)
		for range 2 {
			now = now.Add(10 * time.Minute)
			_, err = service.CopyMembers(t.Context(), "owner", absolute.SnapshotID, absolute.MemberHash)
			require.NoError(t, err)
		}
		now = now.Add(10*time.Minute + time.Nanosecond)
		_, err = service.CopyMembers(t.Context(), "owner", absolute.SnapshotID, absolute.MemberHash)
		require.ErrorIs(t, err, ErrSnapshotGone)
	})
}

func TestQuerySnapshotCacheRevokeAndShutdownCancelBuilders(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CreateFile(t.Context(), s.RootID(), "a.txt", fakeHash("cancel"), 1, "text/plain")
	require.NoError(t, err)

	entered := make(chan string, 2)
	release := make(chan struct{})
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{
		HMACKey: bytes.Repeat([]byte{5}, 32),
		ChargeHook: func(ctx context.Context, owner string, rows, _ int64) error {
			if rows == 0 {
				return nil
			}
			entered <- owner
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	value := snapshotTestQuery(t, `{}`)

	result := make(chan error, 1)
	go func() {
		_, createErr := service.Create(context.Background(), "owner-a", SnapshotRequest{Query: value})
		result <- createErr
	}()
	require.Equal(t, "owner-a", <-entered)
	assert.Positive(t, service.stats().ReservedBytes)
	service.Revoke("owner-a")
	require.ErrorIs(t, <-result, context.Canceled)
	assert.Equal(t, int64(0), service.stats().ReservedBytes)

	go func() {
		_, createErr := service.Create(context.Background(), "owner-b", SnapshotRequest{Query: value})
		result <- createErr
	}()
	require.Equal(t, "owner-b", <-entered)
	deadline, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, service.Shutdown(deadline), context.Canceled)
	close(release)
	require.ErrorIs(t, <-result, context.Canceled)
	require.NoError(t, service.Close())
	require.NoError(t, service.Close())
	assert.Equal(t, snapshotCacheStats{}, service.stats())
}

func TestQuerySnapshotCacheRejectsBusyAndCanceledAdmission(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CreateFile(t.Context(), s.RootID(), "a.txt", fakeHash("busy"), 1, "text/plain")
	require.NoError(t, err)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{
		HMACKey: bytes.Repeat([]byte{6}, 32),
		ChargeHook: func(ctx context.Context, _ string, rows, _ int64) error {
			if rows > 0 {
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		},
	})
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	value := snapshotTestQuery(t, `{}`)
	var wg sync.WaitGroup
	for i := range querySnapshotMaxBuilders {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			_, _ = service.Create(context.Background(), owner, SnapshotRequest{Query: value})
		}(string(rune('a' + i)))
	}
	for range querySnapshotMaxBuilders {
		<-entered
	}
	_, err = service.Create(t.Context(), "third", SnapshotRequest{Query: snapshotTestQuery(t, `{}`)})
	require.ErrorIs(t, err, ErrSnapshotBusy)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = service.Create(canceled, "canceled", SnapshotRequest{Query: snapshotTestQuery(t, `{}`)})
	require.ErrorIs(t, err, context.Canceled)
	close(release)
	wg.Wait()
}

func TestQuerySnapshotCachePreparationPanicReleasesReservation(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CreateFile(t.Context(), s.RootID(), "a.txt", fakeHash("snapshot-panic"), 1, "text/plain")
	require.NoError(t, err)
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{
		HMACKey: bytes.Repeat([]byte{0x77}, 32),
		ChargeHook: func(context.Context, string, int64, int64) error {
			panic("synthetic snapshot preparation panic")
		},
	})
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = service.Create(t.Context(), "owner", SnapshotRequest{Query: snapshotTestQuery(t, `{}`)})
	}()
	assert.Equal(t, "synthetic snapshot preparation panic", recovered)
	stats := service.stats()
	assert.Equal(t, snapshotCacheStats{}, stats)
	if stats == (snapshotCacheStats{}) {
		require.NoError(t, service.Close())
	}
}

func TestQuerySnapshotCursorRejectsValidlySignedWrongBounds(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CreateFile(t.Context(), s.RootID(), "a.txt", fakeHash("cursor"), 1, "text/plain")
	require.NoError(t, err)
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{HMACKey: bytes.Repeat([]byte{7}, 32)})
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	page, err := service.Create(t.Context(), "owner", SnapshotRequest{Query: snapshotTestQuery(t, `{}`)})
	require.NoError(t, err)
	cursor, err := service.encodeCursor("owner", snapshotCursorPayload{
		SnapshotID: page.SnapshotID, QueryFingerprint: page.QueryFingerprint,
		SortField: page.Query.Sort.Field, SortDirection: page.Query.Sort.Direction,
		PageDirection: "next", PageSize: page.PageSize, Offset: 1,
	})
	require.NoError(t, err)
	payloadPart, _, ok := strings.Cut(cursor, ".")
	require.True(t, ok)
	rawPayload, err := base64.RawURLEncoding.DecodeString(payloadPart)
	require.NoError(t, err)
	assert.NotContains(t, string(rawPayload), "owner")
	assert.NotContains(t, string(rawPayload), base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	var payload snapshotCursorPayload
	require.NoError(t, json.Unmarshal(rawPayload, &payload))
	assert.Equal(t, page.SnapshotID, payload.SnapshotID)
	_, err = service.Page(t.Context(), "owner", page.SnapshotID, cursor)
	require.ErrorIs(t, err, ErrSnapshotCursor)
	_, err = service.Page(t.Context(), "owner", page.SnapshotID, strings.Repeat("x", maxSnapshotCursorBytes+1))
	require.ErrorIs(t, err, ErrSnapshotCursor)
}

func TestQuerySnapshotCacheEmptyPopulationHasOneCursorlessPage(t *testing.T) {
	s := newTestStore(t)
	service := NewQuerySnapshotService(s)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	page, err := service.Create(t.Context(), "owner", SnapshotRequest{Query: snapshotTestQuery(t, `{}`)})
	require.NoError(t, err)
	assert.True(t, page.Snapshot)
	assert.Zero(t, page.Total)
	assert.Empty(t, page.Rows)
	assert.Empty(t, page.PrevCursor)
	assert.Empty(t, page.NextCursor)
}

func TestQuerySnapshotCacheConcurrentPageAndRevoke(t *testing.T) {
	s := newTestStore(t)
	for i := range 51 {
		name := string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".txt"
		_, err := s.CreateFile(t.Context(), s.RootID(), name, fakeHash("race-"+name), 1, "text/plain")
		require.NoError(t, err)
	}
	service := NewQuerySnapshotService(s)
	page, err := service.Create(t.Context(), "owner", SnapshotRequest{Query: snapshotTestQuery(t, `{}`), PageSize: 50})
	require.NoError(t, err)
	require.NotEmpty(t, page.NextCursor)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			_, pageErr := service.Page(context.Background(), "owner", page.SnapshotID, page.NextCursor)
			if pageErr != nil {
				assert.ErrorIs(t, pageErr, ErrSnapshotGone)
			}
		})
	}
	service.Revoke("owner")
	wg.Wait()
	require.NoError(t, service.Close())
}

func TestQuerySnapshotCacheRejectsNilStoreAtOperationTime(t *testing.T) {
	service := NewQuerySnapshotService(nil)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	_, err := service.Create(t.Context(), "owner", SnapshotRequest{Query: snapshotTestQuery(t, `{}`)})
	require.Error(t, err)
}
