package daemonconn_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestProductionPackageDownloadChecksTicketAndCompleteBytes(t *testing.T) {
	const (
		jobID       = "77777777-7777-4777-8777-777777777777"
		operationID = "88888888-8888-4888-8888-888888888888"
		versionID   = "99999999-9999-4999-8999-999999999999"
	)
	archive := []byte("synthetic verified recipient archive")
	digest := sha256.Sum256(archive)
	for _, scenario := range []struct {
		name, ticketURL string
		archive         []byte
		wantOK          bool
	}{
		{"verified", "/api/daemon/web-download/file?ticket=synthetic", archive, true},
		{"truncated", "/api/daemon/web-download/file?ticket=synthetic", archive[:len(archive)-1], false},
		{"cross origin", "https://example.invalid/elsewhere", archive, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					if r.URL.Path != "/api/v1/productions/jobs/"+jobID+"/packages/"+operationID+"/download" {
						t.Errorf("unexpected request path %q", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if err := json.MarshalWrite(w, api.ProductionPackageDownloadTicket{
						URL: scenario.ticketURL, ArchiveSHA256: hex.EncodeToString(digest[:]),
						Size: int64(len(archive)), VersionID: versionID,
					}); err != nil {
						t.Errorf("write synthetic ticket: %v", err)
					}
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(scenario.archive)
			}))
			t.Cleanup(server.Close)
			var output bytes.Buffer
			receipt, err := daemonconn.New(server.URL, "synthetic-key").DownloadProductionPackageTo(
				t.Context(), jobID, operationID, &output)
			if scenario.wantOK {
				require.NoError(t, err)
				require.Equal(t, archive, output.Bytes())
				require.Equal(t, hex.EncodeToString(digest[:]), receipt.ArchiveSHA256)
			} else {
				require.Error(t, err)
			}
		})
	}
}
