package store

import (
	"bytes"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestBackupCaptureFollowsBlobRootReferences(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	probe := fakeHash("267")
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return s.EnsureBlobTx(tx, probe, 7)
	}))

	canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{
		ContractVersion: document.SourceMetadataContractV1,
	})
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(ctx, probe, fakeHash("f1"), canonical)
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(ctx, probe, fakeHash("f2"), canonical)
	require.NoError(t, err)

	originalRoots := blobRootReferences
	blobRootReferences = append([]blobReference{{
		table: "source_metadata_generations", column: "source_sha256",
	}}, originalRoots...)
	t.Cleanup(func() { blobRootReferences = originalRoots })

	unreachable, err := s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	assert.NotContains(t, blobHashes(unreachable), probe,
		"the synthetic sixth root must keep its blob reachable")

	var exported bytes.Buffer
	snapshot, err := s.BeginMetadataSnapshot(ctx)
	require.NoError(t, err)
	require.NoError(t, snapshot.ExportBackup(ctx, &exported))
	require.NoError(t, snapshot.Close())

	blobRecord := `"type":"blob","hash":"` + probe + `"`
	assert.Equal(t, 1, strings.Count(exported.String(), blobRecord),
		"duplicate root rows must produce one exported blob record")
}
