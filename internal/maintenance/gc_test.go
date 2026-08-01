package maintenance

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

func TestRetireLooseCandidatesContinuesAfterMissingLocation(t *testing.T) {
	locations := []packstore.ReadLocation{
		{
			StoreID: "missing", Loose: &packstore.LooseLocation{
				Encoding: packstore.LooseEncodingRaw,
			},
		},
		{
			StoreID: "present", Loose: &packstore.LooseLocation{
				Encoding: packstore.LooseEncodingZstd,
			},
		},
	}
	var retired []packstore.StoreID
	count, err := retireLooseCandidates(locations, func(location packstore.ReadLocation) error {
		retired = append(retired, location.StoreID)
		if location.StoreID == "missing" {
			return fs.ErrNotExist
		}
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.Equal(t, []packstore.StoreID{"missing", "present"}, retired)
}
