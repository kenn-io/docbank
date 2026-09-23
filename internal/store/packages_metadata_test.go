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

func TestPackageMetadataRejectsVolumeOwnerSwap(t *testing.T) {
	source := newTestStore(t)
	request := validPackageRequest(t, source)
	request.PackageID = "00000000-0000-4000-8000-000000000001"
	first, err := source.CreatePackage(t.Context(), request)
	require.NoError(t, err)
	request.PackageID = "00000000-0000-4000-8000-000000000002"
	request.Volumes = []PackageVolume{{Ordinal: 1, VolumeName: "VOL001", DeclaredRoot: "VOL001",
		MappedRoot: "other-source", ResolvedRootSHA256: fakeHash("c3")}}
	second, err := source.CreatePackage(t.Context(), request)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	control := newTestStore(t)
	require.NoError(t, control.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	for _, expected := range []Package{first, second} {
		got, err := control.Package(t.Context(), expected.PackageID)
		require.NoError(t, err)
		require.Equal(t, expected, got)
	}

	lines := bytes.Split(bytes.TrimSuffix(exported.Bytes(), []byte("\n")), []byte("\n"))
	changed := 0
	for index, line := range lines {
		var kind struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &kind))
		if kind.Type != metadataPackageVolumeType {
			continue
		}
		var record metadataPackageVolume
		require.NoError(t, json.Unmarshal(line, &record))
		// Keep the original canonical bytes and checksum; only swap ownership.
		if record.PackageID == first.PackageID {
			record.PackageID = second.PackageID
		} else {
			require.Equal(t, second.PackageID, record.PackageID)
			record.PackageID = first.PackageID
		}
		lines[index], err = json.Marshal(record)
		require.NoError(t, err)
		changed++
	}
	require.Equal(t, 2, changed)
	target := newTestStore(t)
	var pristine bytes.Buffer
	require.NoError(t, target.ExportMetadata(t.Context(), &pristine))
	corrupt := append(bytes.Join(lines, []byte("\n")), '\n')
	require.ErrorIs(t, target.ImportMetadata(t.Context(), bytes.NewReader(corrupt)), ErrPackageConflict)
	var after bytes.Buffer
	require.NoError(t, target.ExportMetadata(t.Context(), &after))
	require.Equal(t, pristine.Bytes(), after.Bytes(), "rejected import must roll back every logical change")
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
