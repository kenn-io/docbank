package config

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"testing"
)

func TestLocalFaces(t *testing.T) {
	profile, descriptor, err := LocalFaces()
	require.NoError(t, err)
	require.False(t, descriptor.SupportsTextQuery)
	require.Equal(t, []document.EmbeddingInputKind{document.EmbeddingInputFace}, descriptor.InputKinds)
	require.Nil(t, profile.Rendition)
	require.Len(t, profile.Embeddings, 1)
	_, _, err = document.CanonicalProfile(profile)
	require.NoError(t, err)
}
