package aliases

import (
	ASRT "github.com/stretchr/testify/assert" // want "testify import must be named assert"
	req "github.com/stretchr/testify/require" // want "testify import must be named require"
	"testing"
)

func TestAliases(t *testing.T) {
	ASRT.True(t, true)
	req.NoError(t, nil)
}
