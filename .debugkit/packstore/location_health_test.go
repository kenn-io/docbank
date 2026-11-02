package packstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocationHealthPrefersTransientlyUnavailableOverKnownDamage(t *testing.T) {
	health := NewHealth()
	hash := hashForTest([]byte("health preference"))
	corrupt := ReadLocation{
		StoreID: "corrupt", Generation: "corrupt-1",
		Loose: &LooseLocation{Encoding: LooseEncodingRaw},
	}
	unavailable := ReadLocation{
		StoreID: "unavailable", Generation: "unavailable-1",
		Loose: &LooseLocation{Encoding: LooseEncodingRaw},
	}
	health.Observe(hash, corrupt, ErrPhysicalCorrupt)
	health.Observe(hash, unavailable, ErrStoreUnavailable)

	ordered := health.Order(hash, []ReadLocation{corrupt, unavailable})

	assert.Equal(t, []ReadLocation{unavailable, corrupt}, ordered)
}

func TestLooseLocationHealthIsScopedToContentHash(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	health := NewHealth()
	firstHash := hashForTest([]byte("first same-sized blob"))
	secondHash := hashForTest([]byte("other same-sized blob"))
	primary := ReadLocation{
		StoreID: "primary", Generation: "primary-1",
		Loose: &LooseLocation{
			Encoding: LooseEncodingRaw, LogicalSize: 20, StoredSize: 20,
		},
	}
	secondary := ReadLocation{
		StoreID: "secondary", Generation: "secondary-1",
		Loose: &LooseLocation{
			Encoding: LooseEncodingRaw, LogicalSize: 20, StoredSize: 20,
		},
	}
	health.Observe(firstHash, primary, ErrPhysicalCorrupt)

	ordered := health.Order(secondHash, []ReadLocation{primary, secondary})
	require.Len(ordered, 2)
	assert.Equal(primary, ordered[0])

	health.Clear(secondHash, primary)
	ordered = health.Order(firstHash, []ReadLocation{primary, secondary})
	require.Len(ordered, 2)
	assert.Equal(secondary, ordered[0])
}
