package fixes

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestHelpers(t *testing.T) {
	assert.True(t, true)
	assert.True(t, true) // want "test has 4 direct testify package calls"
	require.NoError(t, nil)
	require.NoError(t, nil) // want "test has 4 direct testify package calls"
}

func TestNestedPackageNeedsName(t *testing.T) {
	assert.True(t, true)
	assert.True(t, true)
	assert.True(t, true)
	assert.True(t, true)
	t.Run("inner", func(t *testing.T) {
		assert := assert.New(t)
		assert.True(true)
	})
}

func TestObjectNames(t *testing.T) {
	asrt := assert.New(t) // want "testify assertion object must be named assert"
	asrt.True(true)
	req := require.New(t) // want "testify assertion object must be named require"
	req.NoError(nil)
}

func TestUnrelatedNames(t *testing.T) {
	req := "value"
	asrt := 4
	assert.Equal(t, req, "value")
	assert.Equal(t, asrt, 4)
}

func TestParentHelper(t *testing.T) {
	parentRequire := require.New(t) // want "testify assertion object must be named require"
	parentRequire.NoError(nil)
	t.Run("inner", func(t *testing.T) {
		require := require.New(t)
		require.NoError(nil)
	})
}

func TestParentHelperWithShadowedT(t *testing.T) {
	parentRequire := require.New(t) // want "testify assertion object must be named require"
	t.Run("inner", func(t *testing.T) {
		parentRequire.NoError(nil)
		require.NoError(t, nil)
	})
}

func TestPackageConstantNeedsName(t *testing.T) {
	assert.Equal(t, assert.AnError, assert.AnError)
	assert.True(t, true)
	assert.True(t, true)
	assert.True(t, true)
}

func TestAliasedHelperUsesPackageConstant(t *testing.T) {
	asrt := assert.New(t) // want "testify assertion object must be named assert"
	asrt.Equal(assert.AnError, assert.AnError)
}

func TestPackageFunctionWithoutMatchingMethod(t *testing.T) {
	assert.True(t, true)
	assert.True(t, true)
	assert.True(t, true)
	assert.True(t, assert.ObjectsAreEqual(1, 1))
}

func TestMismatchedMethodSignature(t *testing.T) {
	assert.Mismatched(t, "one")
	assert.Mismatched(t, "two")
	assert.Mismatched(t, "three")
	assert.Mismatched(t, "four")
}
