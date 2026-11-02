package openssh

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersistentManagerRejectsInvalidConnectionOptionsAtConstruction(t *testing.T) {
	options := DefaultConnectionOptions()
	options.ServerAliveCountMax = 0

	manager, err := NewPersistentManager(t.TempDir(), PersistentConfig{
		ConnectionOptions: &options,
	})

	assert.Nil(t, manager)
	var configErr *ConfigError
	require.ErrorAs(t, err, &configErr)
}
