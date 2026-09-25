package main

import (
	"bytes"
	"mime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPutSourceMIMEUsesBytesBeforeExtension(t *testing.T) {
	const extension = ".docbank561"
	require.NoError(t, mime.AddExtensionType(extension, "application/jpg"))
	require.Equal(t, "application/jpg", mime.TypeByExtension(extension))

	got, err := putSourceMIME(bytes.NewReader([]byte{0xff, 0xd8, 0xff}), "photo"+extension, "")
	require.NoError(t, err)
	require.Equal(t, "image/jpeg", got)
}

func TestPutSourceMIMEKeepsExplicitOverride(t *testing.T) {
	got, err := putSourceMIME(bytes.NewReader([]byte{0xff, 0xd8, 0xff}), "photo.jpg", "application/x-user; version=1")
	require.NoError(t, err)
	require.Equal(t, "application/x-user; version=1", got)
}
