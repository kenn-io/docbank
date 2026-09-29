package daemonconn

import (
	"net/http"
	"net/http/httptest"
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

func TestPhotoOwnerTransportHeader(t *testing.T) {
	const ownerID = "11111111-1111-4111-8111-111111111111"
	var gotOwner string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOwner = r.Header.Get("X-Docbank-Owner")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(server.Close)
	owners, err := New(server.URL, "key").WithPhotoOwner(ownerID).PhotoOwners(t.Context())
	require.NoError(t, err)
	require.Empty(t, owners)
	require.Equal(t, ownerID, gotOwner)
}
