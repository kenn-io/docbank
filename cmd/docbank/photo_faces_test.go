package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"

	"github.com/google/uuid"
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
	for _, value := range []string{"1,2,3", "a,0,0.1,0.1", "NaN,0,0.1,0.1", "0,0,0,0.1", "0.9,0,0.2,0.1", "-0.1,0,0.2,0.1"} {
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
		name   string
		status int
		retry  bool
	}{
		{"validation", http.StatusUnprocessableEntity, false},
		{"conflict", http.StatusConflict, false},
		{"server", http.StatusServiceUnavailable, true},
		{"unreadable success", http.StatusCreated, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(server.Close)
			_, err := daemonconn.New(server.URL, "synthetic-key").CreatePhotoFace(t.Context(), versionID, request)
			require.Error(t, err)
			result := photoFaceCreateError(faceID, err)
			require.ErrorIs(t, result, err)
			if tc.retry {
				require.Contains(t, result.Error(), "retry with --face-id "+faceID)
			} else {
				require.Same(t, err, result)
			}
		})
	}
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	_, err := daemonconn.New(server.URL, "synthetic-key").CreatePhotoFace(t.Context(), versionID, request)
	require.True(t, daemonconn.IsTransportError(err))
	require.Contains(t, photoFaceCreateError(faceID, err).Error(), "retry with --face-id "+faceID)
	require.Same(t, err, photoFaceCreateError("", err))
	validation := errors.New("invalid request")
	require.Same(t, validation, photoFaceCreateError(faceID, validation))
}
