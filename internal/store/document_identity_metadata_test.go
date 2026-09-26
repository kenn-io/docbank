package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type metadataQuerierWithAliasFailure struct {
	metadataQuerier

	queries int
	err     error
}

func (q *metadataQuerierWithAliasFailure) QueryContext(
	ctx context.Context, query string, args ...any,
) (*sql.Rows, error) {
	q.queries++
	if q.queries == 2 {
		return nil, q.err
	}
	return q.metadataQuerier.QueryContext(ctx, query, args...)
}

func TestDocumentIdentityAuthorityBootstrapsAndRoundTripsMetadata(t *testing.T) {
	source := newTestStore(t)
	file, err := source.CreateFile(t.Context(), source.RootID(), "source.txt", fakeHash("d1"), 9, "text/plain",
		BlobPhysical{Encoding: "raw", StoredBytes: 9, PackEligible: true, Created: true})
	require.NoError(t, err)
	identity, err := source.EnsureDocumentIdentity(t.Context(), file.ID)
	require.NoError(t, err)
	require.NoError(t, source.PutDocumentIdentityAlias(t.Context(),
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333", identity.DocumentUID))

	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))

	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	var restored bytes.Buffer
	require.NoError(t, target.ExportMetadata(t.Context(), &restored))
	require.Equal(t, exported.Bytes(), restored.Bytes())
}

func TestExportDocumentIdentityMetadataReturnsAliasQueryErrorWithoutPanic(t *testing.T) {
	source := newTestStore(t)
	want := errors.New("aliases query unavailable")
	query := &metadataQuerierWithAliasFailure{metadataQuerier: source.db, err: want}

	var got error
	require.NotPanics(t, func() {
		got = exportDocumentIdentityMetadata(t.Context(), query, func(any) error { return nil })
	})
	require.ErrorIs(t, got, want)
}
