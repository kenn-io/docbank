package api_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestMailboxMalformedTransferIsInputError(t *testing.T) {
	ts, s := newTestServer(t, nil)
	require.NoError(t, s.RegisterMailboxArchive(t.Context(), store.MailboxArchive{
		ID: "synthetic-archive", Owner: "vault:" + s.VaultID(), Description: "Synthetic archive",
	}))
	raw := "Content-Type: multipart/mixed; boundary=b\r\n\r\n"
	digest := sha256.Sum256([]byte(raw))
	declaration, err := json.Marshal(store.MailboxTransferRequest{
		ArchiveID: "synthetic-archive", Reference: "message-1", SHA256: hex.EncodeToString(digest[:]),
		Size: int64(len(raw)), Settings: "synthetic-settings", DestinationID: s.RootID(), Name: "message.eml",
	})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		ts.URL+"/api/v1/mailbox/transfers", strings.NewReader(raw))
	require.NoError(t, err)
	request.Header.Set("X-Docbank-Transfer", base64.RawURLEncoding.EncodeToString(declaration))
	response, err := ts.Client().Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, string(body))
	var problem api.Error
	require.NoError(t, json.Unmarshal(body, &problem))
	require.Equal(t, "mailbox_invalid", problem.Code)
}

func TestMailboxMutationPreservesMaintenanceResponse(t *testing.T) {
	gate := api.NewOperationGate()
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Gate = gate })
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- gate.MaintainContext(t.Context(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	defer func() {
		close(release)
		require.NoError(t, <-done)
	}()
	<-entered
	response, body := do(t, ts, http.MethodPost, "/api/v1/mailbox/containers", nil,
		api.MailboxContainerInput{ID: "synthetic-container", SHA256: strings.Repeat("a", 64), Size: 3, Format: "mbox"})
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode, body)
	var problem api.Error
	require.NoError(t, json.Unmarshal([]byte(body), &problem))
	require.Equal(t, "maintenance_busy", problem.Code)
}

func TestMailboxUnknownArchiveIsNotFound(t *testing.T) {
	ts, s := newTestServer(t, nil)
	raw := "Subject: Synthetic\r\n\r\nBody\r\n"
	digest := sha256.Sum256([]byte(raw))
	declaration, err := json.Marshal(store.MailboxTransferRequest{
		ArchiveID: "unregistered-synthetic-archive", Reference: "message-1", SHA256: hex.EncodeToString(digest[:]),
		Size: int64(len(raw)), Settings: "synthetic-settings", DestinationID: s.RootID(), Name: "message.eml",
	})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		ts.URL+"/api/v1/mailbox/transfers", strings.NewReader(raw))
	require.NoError(t, err)
	request.Header.Set("X-Docbank-Transfer", base64.RawURLEncoding.EncodeToString(declaration))
	response, err := ts.Client().Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode, string(body))
	var problem api.Error
	require.NoError(t, json.Unmarshal(body, &problem))
	require.Equal(t, "not_found", problem.Code)
}
