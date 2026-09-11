package store

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestCollectionQualityCacheRefreshesAuthorityAndClones(t *testing.T) {
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "cli", "Synthetic cache")
	require.NoError(t, err)
	node, _, err := s.IngestFile(t.Context(), run, s.RootID(), "manual.txt", fakeHash("aa"), 4, "text/plain", "synthetic://manual.txt", "")
	require.NoError(t, err)
	service := NewCollectionQualityService(s)
	first, err := service.Read(t.Context(), run.ID(), CoverageSelection{}, nil)
	require.NoError(t, err)
	original := first.SourceFingerprint
	first.Collection.SourceDescription = "changed by caller"
	first.Dimensions[0].Values[0].Count = 123
	again, err := service.Read(t.Context(), run.ID(), CoverageSelection{}, nil)
	require.NoError(t, err)
	assert.Equal(t, "Synthetic cache", again.Collection.SourceDescription)
	assert.NotEqual(t, int64(123), again.Dimensions[0].Values[0].Count)
	_, err = s.db.ExecContext(t.Context(), "UPDATE nodes SET name=? WHERE id=?", "renamed.pdf", node.ID)
	require.NoError(t, err)
	changed, err := service.Read(t.Context(), run.ID(), CoverageSelection{}, nil)
	require.NoError(t, err)
	assert.NotEqual(t, original, changed.SourceFingerprint)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, readErr := service.Read(t.Context(), run.ID(), CoverageSelection{}, nil)
			assert.NoError(t, readErr)
		})
	}
	wg.Wait()
}

func TestCollectionQualityCacheEvictionAndExpiry(t *testing.T) {
	s := newTestStore(t)
	service := NewCollectionQualityService(s)
	now := time.Now()
	service.clock = func() time.Time { return now }
	for i := range 65 {
		value := CollectionQuality{SourceFingerprint: fmt.Sprintf("%064d", i)}
		service.remember(strconv.Itoa(i), value)
	}
	require.Len(t, service.entries, 64)
	_, ok := service.lookup("0")
	assert.False(t, ok)
	_, ok = service.lookup("64")
	require.True(t, ok)
	now = now.Add(11 * time.Second)
	_, ok = service.lookup("64")
	assert.False(t, ok)
	large := CollectionQuality{Collection: Collection{SourceDescription: string(make([]byte, 9<<20))}}
	service.remember("oversize", large)
	_, ok = service.lookup("oversize")
	assert.False(t, ok)
	assert.LessOrEqual(t, service.bytes, 8<<20)
}
