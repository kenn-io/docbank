package api_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestEmailPendingAndIneligible(t *testing.T) {
	ts, s := newTestServer(t, nil)
	for _, tc := range []struct {
		name   string
		mime   string
		status int
	}{
		{"mail.eml", "message/rfc822", http.StatusAccepted},
		{"ordinary.bin", "application/octet-stream", http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, err := s.CreateFile(t.Context(), s.RootID(), tc.name, testHash(tc.name), 7, tc.mime)
			require.NoError(t, err)
			response, body := get(t, ts, "/api/v1/versions/"+node.CurrentVersionID+"/email", nil)
			require.Equal(t, tc.status, response.StatusCode)
			if tc.status == http.StatusAccepted {
				require.Contains(t, body, `"state":"pending"`)
			}
		})
	}
}

func TestEmailEnsureExactGenerationAndPartBytes(t *testing.T) {
	ts, s := newTestServer(t, nil)
	raw := "Subject: Synthetic\r\nBcc: Hidden Person <hidden@example.test>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nemail-api-marker"
	version := createEmailVersion(t, s, "mail.eml", raw)
	path := "/api/v1/versions/" + version.ID + "/email"

	pendingResponse, pendingBody := get(t, ts, path, nil)
	require.Equal(t, http.StatusAccepted, pendingResponse.StatusCode, pendingBody)
	response, body := do(t, ts, http.MethodPost, path, nil, struct{}{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	var metadata api.EmailMetadata
	require.NoError(t, json.Unmarshal([]byte(body), &metadata))
	require.Equal(t, version.ID, metadata.Version.ID)
	require.Equal(t, version.BlobHash, metadata.Evidence.Source.SHA256)
	require.NotEmpty(t, metadata.Evidence.Inventory)
	require.Len(t, metadata.Evidence.Inventory.Messages, 1)
	require.Len(t, metadata.Evidence.Inventory.Messages[0].Fields.Bcc, 1)
	require.Contains(t, *metadata.Evidence.Inventory.Messages[0].Fields.Bcc[0].Text, "hidden@example.test")

	exactPath := path + "/generations/" + metadata.GenerationID
	exactResponse, exactBody := get(t, ts, exactPath, nil)
	require.Equal(t, http.StatusOK, exactResponse.StatusCode, exactBody)
	assert.JSONEq(t, body, exactBody)

	partResponse, partBody := get(t, ts, exactPath+"/parts/1/raw_headers", nil)
	require.Equal(t, http.StatusOK, partResponse.StatusCode, partBody)
	assert.Contains(t, partBody, "Bcc: Hidden Person <hidden@example.test>")
	assert.NotContains(t, partBody, "email-api-marker")
	assert.Equal(t, "application/octet-stream", partResponse.Header.Get("Content-Type"))
	assert.Equal(t, "nosniff", partResponse.Header.Get("X-Content-Type-Options"))
	assert.Contains(t, partResponse.Header.Get("Content-Disposition"), "attachment")
	assert.Equal(t, version.ID, partResponse.Header.Get(api.ContentVersionHeader))
	assert.Equal(t, metadata.GenerationID, partResponse.Header.Get(api.EmailGenerationHeader))
	assert.Equal(t, metadata.AttachmentID, partResponse.Header.Get(api.EmailAttachmentHeader))
	assert.Equal(t, "1", partResponse.Header.Get(api.EmailPartPathHeader))
	assert.Equal(t, "raw_headers", partResponse.Header.Get(api.EmailPartRoleHeader))
	partSum := sha256.Sum256([]byte(partBody))
	assert.Equal(t, "sha-256=:"+base64.StdEncoding.EncodeToString(partSum[:])+":",
		partResponse.Trailer.Get("Content-Digest"))
}

func TestEmailRoutesRejectUnknownInputAndInvalidSelections(t *testing.T) {
	ts, s := newTestServer(t, nil)
	raw := "Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nbody\r\n--x--\r\n"
	version := createEmailVersion(t, s, "multipart.eml", raw)
	path := "/api/v1/versions/" + version.ID + "/email"

	response, body := do(t, ts, http.MethodPost, path, nil, map[string]any{"future": true})
	assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	assert.Contains(t, body, `"code":"validation"`)
	response, body = do(t, ts, http.MethodPost, path, nil, struct{}{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var metadata api.EmailMetadata
	require.NoError(t, json.Unmarshal([]byte(body), &metadata))
	base := path + "/generations/" + metadata.GenerationID + "/parts/"

	for _, tc := range []struct {
		selection string
		status    int
		code      string
	}{
		{"01/raw_headers", http.StatusUnprocessableEntity, "invalid_email_part"},
		{"1/future_role", http.StatusUnprocessableEntity, "invalid_email_part"},
		{"1/body_utf8", http.StatusConflict, "email_part_unavailable"},
		{"9/raw_headers", http.StatusNotFound, "not_found"},
	} {
		response, body := get(t, ts, base+tc.selection, nil)
		assert.Equal(t, tc.status, response.StatusCode, tc.selection+": "+body)
		assert.Contains(t, body, `"code":"`+tc.code+`"`)
	}
}

func TestEmailMalformedMIMEPublishesRetrievableEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, headers, body string
		code                document.EmailDiagnosticCode
		related             bool
	}{
		{"valid control", "Content-Type: text/plain\r\n", "body", "", false},
		{"missing related boundary", "Content-Type: multipart/related\r\n", "body", document.EmailDiagnosticBoundaryMissing, true},
		{"invalid related boundary", "Content-Type: multipart/related; boundary=" + strings.Repeat("x", 71) + "\r\n", "body", document.EmailDiagnosticBoundaryInvalid, true},
		{"unsupported related transfer", "Content-Type: multipart/related; boundary=x\r\nContent-Transfer-Encoding: x-private\r\n", "body", document.EmailDiagnosticTransferUnsupported, true},
		{"malformed related transfer", "Content-Type: multipart/related; boundary=x\r\nContent-Transfer-Encoding: base64\r\n", "YQ=!", document.EmailDiagnosticTransferInvalid, true},
		{"empty transfer", "Content-Transfer-Encoding: \t\r\n", "body", document.EmailDiagnosticInvalidHeader, false},
		{"missing media subtype", "Content-Type: text\r\n", "body", document.EmailDiagnosticInvalidHeader, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, s := newTestServer(t, nil)
			headers := tc.headers + "\r\n"
			version := createEmailVersion(t, s, "malformed.eml", headers+tc.body)
			path := "/api/v1/versions/" + version.ID + "/email"
			response, body := do(t, ts, http.MethodPost, path, nil, struct{}{})
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			response, body = get(t, ts, path, nil)
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			var metadata api.EmailMetadata
			require.NoError(t, json.Unmarshal([]byte(body), &metadata))
			inventory := metadata.Evidence.Inventory
			require.NotNil(t, inventory)
			require.Len(t, inventory.Parts, 1)
			if tc.related {
				assert.Equal(t, document.EmailInventoryPartial, inventory.State)
				require.NotNil(t, inventory.Termination)
				assert.Equal(t, tc.code, inventory.Termination.Code)
				require.Len(t, inventory.Messages[0].RelatedGroups, 1)
				assert.Equal(t, "1", inventory.Messages[0].RelatedGroups[0].RootPath)
			} else if tc.code != "" {
				diagnostics := slices.Concat(inventory.Parts[0].Diagnostics, inventory.Parts[0].Media.Diagnostics)
				require.NotEmpty(t, diagnostics)
				assert.Equal(t, tc.code, diagnostics[0].Code)
			}
			response, body = get(t, ts, path+"/generations/"+metadata.GenerationID+"/parts/1/raw_headers", nil)
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			assert.Equal(t, headers, body)
		})
	}
}

func TestEmailEnsureRejectsOversizeBodyBeforeDecoding(t *testing.T) {
	ts, s := newTestServer(t, nil)
	version := createEmailVersion(t, s, "mail.eml", "Content-Type: text/plain\r\n\r\nbody")
	path := "/api/v1/versions/" + version.ID + "/email"

	response, body := do(t, ts, http.MethodPost, path, nil,
		map[string]string{"content": strings.Repeat("x", (1<<20)+1)})
	assert.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode, body)
	assert.Contains(t, body, `"code":"too_large"`)
}

func TestEmailEnsurePreservesMaintenanceBusy(t *testing.T) {
	gate := api.NewOperationGate()
	ts, s := newTestServer(t, func(d *api.Deps) { d.Gate = gate })
	version := createEmailVersion(t, s, "mail.eml", "Content-Type: text/plain\r\n\r\nbody")

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	done := make(chan error, 1)
	go func() {
		done <- gate.Maintain(func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	response, body := do(t, ts, http.MethodPost,
		"/api/v1/versions/"+version.ID+"/email", nil, struct{}{})
	assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode, body)
	assert.Contains(t, body, `"code":"maintenance_busy"`)
	releaseOnce.Do(func() { close(release) })
	require.NoError(t, <-done)
}

func TestEmailGenerationRequiresActualVersionAttachment(t *testing.T) {
	ts, s := newTestServer(t, nil)
	first := createEmailVersion(t, s, "first.eml", "Content-Type: text/plain\r\n\r\nfirst")
	second := createEmailVersion(t, s, "second.eml", "Content-Type: text/plain\r\n\r\nsecond")
	firstPath := "/api/v1/versions/" + first.ID + "/email"
	response, body := do(t, ts, http.MethodPost, firstPath, nil, struct{}{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var metadata api.EmailMetadata
	require.NoError(t, json.Unmarshal([]byte(body), &metadata))

	response, body = get(t, ts, "/api/v1/versions/"+second.ID+"/email/generations/"+metadata.GenerationID, nil)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	assert.Contains(t, body, `"code":"not_found"`)
}

func TestEmailPartCorruptPhysicalBytesReturnIntegrityError(t *testing.T) {
	ts, s := newTestServer(t, nil)
	version := createEmailVersion(t, s, "mail.eml", "Content-Type: text/plain\r\n\r\ncorrupt-me")
	path := "/api/v1/versions/" + version.ID + "/email"
	response, body := do(t, ts, http.MethodPost, path, nil, struct{}{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var metadata api.EmailMetadata
	require.NoError(t, json.Unmarshal([]byte(body), &metadata))
	var payloadHash string
	for _, part := range metadata.Evidence.Inventory.Parts {
		if part.Path == "1" && part.Payload != nil {
			payloadHash = part.Payload.SHA256
		}
	}
	require.NotEmpty(t, payloadHash)
	require.NoError(t, os.WriteFile(
		filepath.Join(s.BlobsDir, payloadHash[:2], payloadHash), []byte("short"), 0o600,
	))
	response, body = get(t, ts, path+"/generations/"+metadata.GenerationID+"/parts/1/decoded_payload", nil)
	assert.Equal(t, http.StatusInternalServerError, response.StatusCode, body)
	assert.Contains(t, body, `"code":"content_corrupt"`)
}

func TestEmailRoutesRemainOutsideBrowserSessionCapability(t *testing.T) {
	ts, s := newTestServer(t, nil)
	version := createEmailVersion(t, s, "mail.eml", "Bcc: hidden@example.test\r\nContent-Type: text/plain\r\n\r\nbody")
	path := "/api/v1/versions/" + version.ID + "/email"
	response, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		response, body = do(t, ts, method, path, headers, struct{}{})
		assert.Equal(t, http.StatusForbidden, response.StatusCode, method+": "+body)
	}
}

func createEmailVersion(t *testing.T, s *testStore, name, raw string) store.ContentVersion {
	t.Helper()
	hash, size, err := s.Blobs.Write(strings.NewReader(raw))
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), s.RootID(), name, hash, size, "message/rfc822")
	require.NoError(t, err)
	version, err := s.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	return version
}
