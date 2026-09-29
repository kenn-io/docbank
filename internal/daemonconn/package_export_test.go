package daemonconn_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestPackageExportDownloadStreamsAndRejectsInvalidResponses(t *testing.T) {
	archive := bytes.Repeat([]byte("verified-load-file-archive"), 1024)
	digest := sha256.Sum256(archive)
	request := api.PackageExportRequest{
		SnapshotID: "11111111-1111-4111-8111-111111111111", ProfileID: "export-csv-natives-v1",
	}
	for _, test := range []struct {
		name       string
		status     int
		body       []byte
		wantOK     bool
		wantNoData bool
	}{
		{name: "verified", status: http.StatusOK, body: archive, wantOK: true},
		{name: "truncated", status: http.StatusOK, body: archive[:len(archive)-1]},
		{name: "corrupt", status: http.StatusOK, body: append([]byte(nil), archive...)},
		{name: "rejected", status: http.StatusInternalServerError, body: []byte("rejected"), wantNoData: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "corrupt" {
				test.body[len(test.body)/2] ^= 0xff
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/packages/exports" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusCreated)
					if err := json.MarshalWrite(w, api.PackageExportTicket{
						URL: "/download", Name: "loadfile.zip", SnapshotID: request.SnapshotID,
						ProfileID: request.ProfileID, ArchiveSHA256: hex.EncodeToString(digest[:]),
						ManifestSHA256: strings.Repeat("a", 64), CrosswalkSHA256: strings.Repeat("b", 64),
						Size: int64(len(archive)), Records: 1,
					}); err != nil {
						return
					}
					return
				}
				w.WriteHeader(test.status)
				_, _ = w.Write(test.body)
			}))
			t.Cleanup(server.Close)
			var output bytes.Buffer
			receipt, err := daemonconn.New(server.URL, "").CreatePackageExport(t.Context(), request, &output)
			if test.wantOK {
				require.NoError(t, err)
				require.Equal(t, archive, output.Bytes())
				require.Equal(t, int64(len(archive)), receipt.Size)
				return
			}
			require.Error(t, err)
			if test.wantNoData {
				require.Empty(t, output.Bytes())
			}
		})
	}
}
