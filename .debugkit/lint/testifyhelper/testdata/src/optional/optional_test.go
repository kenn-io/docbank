package optional

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPackageCalls(t *testing.T) {
	assert.True(t, true)
	assert.True(t, true)
	assert.True(t, true)
	assert.True(t, true)
	require.NoError(t, nil)
	require.NoError(t, nil)
	require.NoError(t, nil)
	require.NoError(t, nil)
}

func TestHelperNames(t *testing.T) {
	asrt := assert.New(t) // want "testify assertion object must be named assert"
	req := require.New(t) // want "testify assertion object must be named require"
	asrt.True(true)
	req.NoError(nil)
}
