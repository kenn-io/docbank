package store

import (
	"bytes"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

func TestPackageSnapshotAndManifestSurviveMetadataRestore(t *testing.T) {
	s := newTestStore(t)
	request := validPackageRequest(t, s)
	packageBefore, err := s.CreatePackage(t.Context(), request)
	require.NoError(t, err)
	snapshotBefore, err := s.CollectionSnapshot(t.Context(), request.SnapshotID)
	require.NoError(t, err)
	membersBefore, err := s.SnapshotMembers(t.Context(), request.SnapshotID, 0, 100)
	require.NoError(t, err)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	packageAfter, err := restored.Package(t.Context(), request.PackageID)
	require.NoError(t, err)
	require.Equal(t, packageBefore, packageAfter)
	snapshotAfter, err := restored.CollectionSnapshot(t.Context(), request.SnapshotID)
	require.NoError(t, err)
	require.Equal(t, snapshotBefore, snapshotAfter)
	membersAfter, err := restored.SnapshotMembers(t.Context(), request.SnapshotID, 0, 100)
	require.NoError(t, err)
	require.Equal(t, membersBefore, membersAfter)
	var again bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &again))
	require.Equal(t, exported.Bytes(), again.Bytes())
}

func TestPackageMetadataRejectsMemberTamperWithRecomputedRowChecksum(t *testing.T) {
	s := newTestStore(t)
	request := validPackageRequest(t, s)
	_, err := s.CreatePackage(t.Context(), request)
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &out))
	lines := bytes.Split(bytes.TrimSuffix(out.Bytes(), []byte("\n")), []byte("\n"))
	changed := false
	for index, line := range lines {
		var kind struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &kind))
		if kind.Type != metadataCollectionSnapshotMemberType {
			continue
		}
		var record metadataCollectionSnapshotMember
		require.NoError(t, json.Unmarshal(line, &record))
		member, err := canonical.Decode[CollectionSnapshotMember](record.CanonicalJSON)
		require.NoError(t, err)
		member.FrozenFieldsJSON = `{"altered":true}`
		encoded, checksum, err := snapshotRowBytes(member)
		require.NoError(t, err)
		record.CanonicalJSON, record.Checksum = encoded, checksum
		lines[index], err = json.Marshal(record)
		require.NoError(t, err)
		changed = true
	}
	require.True(t, changed)
	fresh := newTestStore(t)
	corrupt := append(bytes.Join(lines, []byte("\n")), '\n')
	err = fresh.ImportMetadata(t.Context(), bytes.NewReader(corrupt))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPackageConflict)
}
