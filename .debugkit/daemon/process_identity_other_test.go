//go:build !linux

package daemon

import (
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProcessIdentityCompatibilityRejectsValuesOutsideEmissionDomain(t *testing.T) {
	assert.True(t, processIdentityCompatible(ProcessIdentity(strconv.FormatInt(math.MaxInt64, 10))))
	assert.False(t, processIdentityCompatible("9223372036854775808"))
}
