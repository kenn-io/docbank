package api

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

func TestNotePublicationUploadErrorsKeepStorageDetailsPrivate(t *testing.T) {
	privatePath := "/synthetic/owner-private/archive"
	err := notePublicationUploadError(errors.New("opening " + privatePath + ": storage failed"))
	require.Equal(t, 500, err.Status)
	require.Equal(t, "internal", err.Code)
	require.NotContains(t, err.Detail, privatePath)
	for _, test := range []struct {
		cause  error
		status int
		code   string
	}{
		{store.ErrPublicationSourceStale, 409, "publication_source_stale"},
		{store.ErrPublicationScanSuperseded, 409, "publication_scan_superseded"},
		{ingest.ErrUploadDigestMismatch, 422, "digest_mismatch"},
	} {
		mapped := notePublicationUploadError(test.cause)
		require.Equal(t, test.status, mapped.Status)
		require.Equal(t, test.code, mapped.Code)
	}
}
