package main

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

func TestPhotoOwnerCommandRouting(t *testing.T) {
	assert.NotNil(t, photosCmd.PersistentFlags().Lookup("owner"))
	for _, command := range []*cobra.Command{photoOwnerAddCmd, photoOwnerListCmd, photoOwnerRenameCmd, photoOwnerRemoveCmd} {
		assert.NotNil(t, command)
	}
}
