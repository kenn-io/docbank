package main

import (
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestParsePhotoFaceBox(t *testing.T) {
	t.Parallel()
	box, err := parsePhotoFaceBox("0.1, 0.2,0.3,0.4")
	require.NoError(t, err)
	require.Equal(t, &document.FaceBox{X: 0.1, Y: 0.2, Width: 0.3, Height: 0.4}, box)
	box, err = parsePhotoFaceBox("")
	require.NoError(t, err)
	require.Nil(t, box)
	for _, value := range []string{"1,2,3", "a,0,0.1,0.1", "0.9,0,0.2,0.1", "NaN,0,0.1,0.1", "0,0,0,0.1", "-0.1,0,0.1,0.1"} {
		_, err := parsePhotoFaceBox(value)
		require.Error(t, err, value)
	}
}

func TestPhotoFaceCreateRequest(t *testing.T) {
	t.Parallel()
	box := &document.FaceBox{Width: 0.2, Height: 0.2}
	request, err := photoFaceCreateRequest(box, "", "")
	require.NoError(t, err)
	id, err := uuid.Parse(request.FaceID)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(4), id.Version())
	retry, err := photoFaceCreateRequest(box, "", request.FaceID)
	require.NoError(t, err)
	require.Equal(t, request, retry)
	_, err = photoFaceCreateRequest(nil, "", request.FaceID)
	require.Error(t, err)
	_, err = photoFaceCreateRequest(box, "", "invalid")
	require.Error(t, err)
}

func TestPhotoFaceCreateErrorRetryAdvice(t *testing.T) {
	t.Parallel()
	const faceID = "22222222-2222-4222-8222-222222222222"
	const versionID = "11111111-1111-4111-8111-111111111111"
	request := api.CreatePhotoFaceRequest{FaceID: faceID, Box: &document.FaceBox{Width: 0.2, Height: 0.2}}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"validation", store.ErrInvalidPhotoFace},
		{"conflict", store.ErrExists},
		{"ordinary error", errors.New("invalid request")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Same(t, tc.err, photoFaceCreateError(faceID, tc.err))
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	_, err := daemonconn.New(server.URL, "synthetic-key").CreatePhotoFace(t.Context(), versionID, request)
	require.Error(t, err)
	require.Contains(t, photoFaceCreateError(faceID, err).Error(), "retry with --face-id "+faceID)
	require.Same(t, err, photoFaceCreateError("", err))
}

func TestPhotoFaceDecisionCLIRequest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)
	const (
		versionID   = "11111111-1111-4111-8111-111111111111"
		faceID      = "22222222-2222-4222-8222-222222222222"
		personID    = "33333333-3333-4333-8333-333333333333"
		operationID = "44444444-4444-4444-8444-444444444444"
		undoID      = "55555555-5555-4555-8555-555555555555"
		token       = "synthetic-proof-token"
	)
	var record kitdaemon.RuntimeRecord
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case kitdaemon.DefaultPingPath:
			assert.NoError(t, json.MarshalWrite(w, kitdaemon.PingInfo{OK: true, Service: daemonconn.Service, Version: version.Version, PID: record.PID}))
		case daemonauth.ChallengePath:
			nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
			assert.NoError(t, err)
			assert.NoError(t, json.MarshalWrite(w, map[string]string{"proof": daemonauth.Proof(token, nonce)}))
		case "/api/v1/photos/versions/" + versionID + "/face-decisions":
			requests.Add(1)
			assert.Equal(t, http.MethodPost, r.Method)
			var request api.PhotoFaceDecisionRequest
			assert.NoError(t, json.UnmarshalRead(r.Body, &request))
			assert.Equal(t, api.PhotoFaceDecisionRequest{Action: "name", FaceID: faceID, PersonID: personID, Revision: 7, OperationID: operationID}, request)
			assert.NoError(t, json.MarshalWrite(w, api.PhotoFaceDecisionResult{ReceiptID: operationID}))
		case "/api/v1/photos/face-decisions/" + operationID + "/undo":
			requests.Add(1)
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, undoID, r.URL.Query().Get("operation_id"))
			assert.NoError(t, json.MarshalWrite(w, api.PhotoFaceDecisionResult{ReceiptID: undoID}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	record = daemonconn.NewRecord(strings.TrimPrefix(server.URL, "http://"), "synthetic-api-key", token, "")
	_, err := daemonconn.RuntimeStore(dir).Write(record)
	require.NoError(t, err)
	output, err := runCLI(t, "photos", "faces", "name", versionID, "--face", faceID, "--person", personID, "--revision", "7", "--operation-id", operationID)
	require.NoError(t, err)
	var named api.PhotoFaceDecisionResult
	require.NoError(t, json.Unmarshal([]byte(output), &named))
	require.Equal(t, operationID, named.ReceiptID)
	output, err = runCLI(t, "photos", "faces", "undo", named.ReceiptID, "--operation-id", undoID)
	require.NoError(t, err)
	var undone api.PhotoFaceDecisionResult
	require.NoError(t, json.Unmarshal([]byte(output), &undone))
	require.Equal(t, undoID, undone.ReceiptID)
	require.Equal(t, int32(2), requests.Load())
}
