package api_test

import (
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestMailboxUploadBodyLimits(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"chunk", "transfer"} {
		for _, variant := range []string{"exact", "short", "changed", "extra"} {
			t.Run(kind+"/"+variant, func(t *testing.T) {
				ts, s := newTestServer(t, nil)
				raw := "abc"
				method, path := http.MethodPut, "/api/v1/mailbox/containers/synthetic-container/chunks/0"
				if kind == "transfer" {
					raw = "Subject: Synthetic\r\n\r\nBody\r\n"
					method, path = http.MethodPost, "/api/v1/mailbox/transfers"
				}
				hash := testHash(raw)
				body, want := raw, http.StatusOK
				switch variant {
				case "short":
					body, want = raw[:len(raw)-1], http.StatusConflict
				case "changed":
					body, want = "x"+raw[1:], http.StatusConflict
				case "extra":
					body, want = raw+"x", http.StatusRequestEntityTooLarge
				}
				request, err := http.NewRequestWithContext(t.Context(), method, ts.URL+path, io.NopCloser(strings.NewReader(body)))
				require.NoError(t, err)
				if kind == "chunk" {
					_, err = s.BeginMailboxContainer(t.Context(), store.MailboxContainerRequest{
						ID: "synthetic-container", Owner: "vault:" + s.VaultID(), SHA256: hash, Size: int64(len(raw)), Format: "mbox",
					})
					require.NoError(t, err)
					request.Header.Set(api.BlobSizeHeader, strconv.Itoa(len(raw)))
					request.Header.Set(api.BlobHashHeader, hash)
				} else {
					require.NoError(t, s.RegisterMailboxArchive(t.Context(), store.MailboxArchive{
						ID: "synthetic-archive", Owner: "vault:" + s.VaultID(), Description: "Synthetic archive",
					}))
					declaration, err := json.Marshal(store.MailboxTransferRequest{
						ArchiveID: "synthetic-archive", Reference: "message-1", SHA256: hash,
						Size: int64(len(raw)), Settings: "synthetic-settings", DestinationID: s.RootID(), Name: "message.eml",
					})
					require.NoError(t, err)
					request.Header.Set("X-Docbank-Transfer", base64.RawURLEncoding.EncodeToString(declaration))
				}
				response, err := ts.Client().Do(request)
				require.NoError(t, err)
				defer func() { require.NoError(t, response.Body.Close()) }()
				result, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.Equal(t, want, response.StatusCode, string(result))
			})
		}
	}
}

func TestMailboxUploadsOutliveRequestTimeout(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"chunk", "transfer"} {
		t.Run(kind, func(t *testing.T) {
			_, catalog := newTestServer(t, nil)
			raw := "Subject: Synthetic\r\n\r\nBody\r\n"
			hash := testHash(raw)
			endpoint := "/api/v1/mailbox/containers/slow%2Fgroup/chunks/0"
			method := http.MethodPut
			owner := "vault:" + catalog.VaultID()
			headers := http.Header{"X-Api-Key": {testAPIKey}}
			if kind == "chunk" {
				_, err := catalog.BeginMailboxContainer(t.Context(), store.MailboxContainerRequest{ID: "slow/group", Owner: owner, SHA256: hash, Size: int64(len(raw)), Format: "mbox"})
				require.NoError(t, err)
				headers.Set(api.BlobHashHeader, hash)
				headers.Set(api.BlobSizeHeader, strconv.Itoa(len(raw)))
			} else {
				endpoint = "/api/v1/mailbox/transfers"
				method = http.MethodPost
				require.NoError(t, catalog.RegisterMailboxArchive(t.Context(), store.MailboxArchive{ID: "slow", Owner: owner, Description: "Synthetic"}))
				declaration, err := json.Marshal(store.MailboxTransferRequest{ArchiveID: "slow", Reference: "one", SHA256: hash, Size: int64(len(raw)), Settings: "synthetic", DestinationID: catalog.RootID(), Name: "one.eml"})
				require.NoError(t, err)
				headers.Set("X-Docbank-Transfer", base64.RawURLEncoding.EncodeToString(declaration))
			}
			synctest.Test(t, func(t *testing.T) {
				reader, stream := io.Pipe()
				written := make(chan struct{})
				defer func() {
					_ = reader.Close()
					<-written
				}()
				go func() {
					defer close(written)
					time.Sleep(61 * time.Second)
					_, err := stream.Write([]byte(raw))
					_ = stream.CloseWithError(err)
				}()
				request := httptest.NewRequest(method, endpoint, reader)
				request.Header = headers
				response := httptest.NewRecorder()
				catalog.Server.Handler().ServeHTTP(response, request)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			})
		})
	}
}
