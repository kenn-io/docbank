package api_test

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestVersionFactsRoutesAndOpenLink(t *testing.T) {
	t.Parallel()
	for _, origin := range []string{"", "https://docs.example.test"} {
		t.Run(origin, func(t *testing.T) {
			t.Parallel()
			ts, s := newTestServer(t, func(d *api.Deps) { d.Cfg.Web.PublicOrigin = origin })
			node := createFileWithContent(t, ts, s, "sample.txt", "synthetic")
			for _, kind := range []string{"dates", "people"} {
				path := fmt.Sprintf("/api/v1/versions/%s/%s", node.CurrentVersionID, kind)
				res, body := get(t, ts, path, nil)
				require.Equal(t, http.StatusOK, res.StatusCode, body)
				var result map[string]any
				require.NoError(t, json.Unmarshal([]byte(body), &result))
				require.Equal(t, node.CurrentVersionID, result["content_version_id"])
				if origin == "" {
					require.NotContains(t, result, "open")
				} else {
					open, ok := result["open"].(map[string]any)
					require.True(t, ok)
					require.Equal(t, fmt.Sprintf("%s/#node=%d&version=%s", origin, node.ID, node.CurrentVersionID), open["web"])
				}
				res, body = get(t, ts, path+"?timezone=UTC", nil)
				require.Equal(t, http.StatusBadRequest, res.StatusCode, body)
				require.Equal(t, "unknown_parameter", decodeProblem(t, body).Code)
				res, body = get(t, ts, "/api/v1/versions/00000000-0000-4000-8000-000000000000/"+kind, nil)
				require.Equal(t, http.StatusNotFound, res.StatusCode, body)
				require.Equal(t, "version_not_found", decodeProblem(t, body).Code)
			}
		})
	}
}

func TestVersionFactsBrowserFullForbidden(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	node := createFileWithContent(t, ts, s, "sample.txt", "synthetic")
	res, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, res.StatusCode, body)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	for _, kind := range []string{"dates", "people"} {
		path := fmt.Sprintf("/api/v1/versions/%s/%s", node.CurrentVersionID, kind)
		headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: session.Token}
		res, body = get(t, ts, path, headers)
		require.Equal(t, http.StatusOK, res.StatusCode, body)
		res, body = get(t, ts, path+"?disclosure=full", headers)
		require.Equal(t, http.StatusForbidden, res.StatusCode, body)
		require.Equal(t, "disclosure_forbidden", decodeProblem(t, body).Code)
	}
	person, err := s.CreatePerson(t.Context(), "Blind Recipient", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(t.Context(), store.PersonDocumentAssertion{ContentVersionID: node.CurrentVersionID, PersonID: person.PersonID, Role: "blind_copy", Action: "assert", Revision: 1})
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/versions/%s/people", node.CurrentVersionID)
	res, body = get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, res.StatusCode, body)
	require.NotContains(t, body, person.PersonID)
	res, body = get(t, ts, path+"?disclosure=full", nil)
	require.Equal(t, http.StatusOK, res.StatusCode, body)
	require.Contains(t, body, person.PersonID)
}

func TestVersionFactsPDFMetadata(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	node, err := s.CreateFile(t.Context(), s.RootID(), "report.pdf", testHash("pdf-facts"), 1, "application/pdf")
	require.NoError(t, err)
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetAuthor("J. PELLHAM", false)
	pdf.SetCreationDate(time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC))
	pdf.SetModificationDate(time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC))
	pdf.AddPage()
	var encoded bytes.Buffer
	require.NoError(t, pdf.Output(&encoded))
	metadata, err := processing.ExtractSourceMetadata(t.Context(), t.TempDir(), encoded.Bytes())
	require.NoError(t, err)
	raw, _, err := document.MarshalSourceMetadataV1(metadata)
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(t.Context(), node.BlobHash, testHash("extractor"), raw)
	require.NoError(t, err)
	targets, err := s.MissingDocumentEventTargetsAfter(t.Context(), store.DocumentEventsDeriverFingerprint, "", 100)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	evidence, err := s.LoadDocumentEventEvidence(t.Context(), targets[0])
	require.NoError(t, err)
	record, _, err := processing.DeriveDocumentEvents(evidence)
	require.NoError(t, err)
	eventJSON, _, err := document.MarshalDocumentEventsV1(record)
	require.NoError(t, err)
	_, err = s.PublishDocumentEvents(t.Context(), targets[0], store.DocumentEventsDeriverFingerprint, evidence.InputsSHA256, eventJSON)
	require.NoError(t, err)
	_, err = s.PrepareDocumentPeopleInputs(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	res, body := get(t, ts, fmt.Sprintf("/api/v1/versions/%s/dates", node.CurrentVersionID), nil)
	require.Equal(t, http.StatusOK, res.StatusCode, body)
	var dates api.VersionDates
	require.NoError(t, json.Unmarshal([]byte(body), &dates))
	require.Equal(t, "published", dates.State)
	require.Equal(t, "created_date", dates.Primary.Reason)
	kinds := map[document.DateKind]bool{}
	for _, claim := range dates.Claims {
		kinds[claim.DateKind] = true
	}
	require.True(t, kinds["created"])
	require.True(t, kinds["modified"])
	res, body = get(t, ts, fmt.Sprintf("/api/v1/versions/%s/people", node.CurrentVersionID), nil)
	require.Equal(t, http.StatusOK, res.StatusCode, body)
	require.Contains(t, body, "J. PELLHAM")
}

func TestVersionFactsBrowserHiddenAuthorization(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.WriteContext(ctx, strings.NewReader("synthetic JPEG"))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "hidden.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = daemonconn.New(ts.URL, testAPIKey).SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true, "")
	require.NoError(t, err)
	res, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, res.StatusCode, body)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	_, cookie, err := daemonconn.New(ts.URL, testAPIKey).PhotoHidden(ctx, "unlock", "synthetic-passcode", "")
	require.NoError(t, err)
	for _, kind := range []string{"dates", "people"} {
		path := fmt.Sprintf("/api/v1/versions/%s/%s", node.CurrentVersionID, kind)
		headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: session.Token, "Cookie": cookie}
		res, body = get(t, ts, path, headers)
		require.Equal(t, http.StatusOK, res.StatusCode, body)
	}
	require.NoError(t, s.LockPhotoHidden(ctx))
	for _, kind := range []string{"dates", "people"} {
		path := fmt.Sprintf("/api/v1/versions/%s/%s", node.CurrentVersionID, kind)
		res, body = get(t, ts, path, map[string]string{"X-Api-Key": "", api.WebSessionHeader: session.Token, "Cookie": cookie})
		require.Equal(t, http.StatusNotFound, res.StatusCode, body)
	}
}
