package daemonconn

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPhotoOwnerRuntimeCompatibility(t *testing.T) {
	connection := New("http://127.0.0.1:1", "key")
	selected := connection.WithPhotoOwner("11111111-1111-4111-8111-111111111111")
	require.Empty(t, connection.PhotoOwner())
	require.Equal(t, "11111111-1111-4111-8111-111111111111", selected.PhotoOwner())
	record := NewRecord("127.0.0.1:1", "key", "shutdown", "127.0.0.1:2")
	require.Equal(t, "67", record.Metadata[metaProtocolVersion])
}
