package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPhotoOwnerTools(t *testing.T) {
	names := map[string]bool{}
	for _, definition := range readToolDefinitions {
		names[definition.name] = true
	}
	for _, definition := range photoWriteToolDefinitions {
		names[definition.name] = true
	}
	for _, definition := range photoOwnerToolDefinitions {
		names[definition.name] = true
	}
	for _, name := range []string{"list_photo_owners", "add_photo_owner", "rename_photo_owner", "remove_photo_owner"} {
		assert.True(t, names[name], name)
	}
	assert.True(t, photoWriteTool("add_photo_owner"))
	assert.False(t, photoWriteTool("list_photo_owners"))
}
