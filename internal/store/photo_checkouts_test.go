package store

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPhotoCheckoutSelections(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	a := albumAsset(t, s, "first.jpg")
	b := albumAsset(t, s, "last.jpg")
	c := albumAsset(t, s, "outside.jpg")
	for _, v := range []struct {
		asset PhotoAsset
		date  string
	}{{a, "2024-01-01"}, {b, "2024-12-31"}, {c, "2025-01-01"}} {
		n, err := s.NodeByID(ctx, v.asset.Files[0].NodeID)
		require.NoError(t, err)
		browsePhotoMetadata(t, s, n, v.date, photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(v.date, v.date, document.SourceMetadataPrecisionDate, document.SourceMetadataTimezoneOmitted, "")))
	}
	set, err := s.CreatePhotoSet(ctx, "Trip")
	require.NoError(t, err)
	_, err = s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: []string{a.ID, b.ID}})
	require.NoError(t, err)
	for _, test := range []struct {
		selection PhotoCheckoutSelection
		count     int64
	}{
		{PhotoCheckoutSelection{All: true}, 3},
		{PhotoCheckoutSelection{AssetIDs: []string{a.ID, a.ID}}, 1},
		{PhotoCheckoutSelection{AlbumIDs: []string{set.ID}}, 2},
		{PhotoCheckoutSelection{Years: &PhotoCheckoutYears{From: 2024, To: 2024}}, 2},
		{PhotoCheckoutSelection{AssetIDs: []string{c.ID}, AlbumIDs: []string{set.ID}, Years: &PhotoCheckoutYears{From: 2024, To: 2024}}, 3},
	} {
		estimate, err := s.EstimatePhotoCheckout(ctx, test.selection)
		require.NoError(t, err)
		require.Equal(t, test.count, estimate.Assets)
		require.Equal(t, test.count, estimate.Files)
		require.Equal(t, test.count*20, estimate.Bytes)
	}
	_, err = s.SetPhotoAssetExcluded(ctx, c.ID, c.Revision, true)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE photo_assets SET hidden_at=? WHERE asset_id=?`, nowRFC3339(), b.ID)
	require.NoError(t, err)
	estimate, err := s.EstimatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{a.ID, b.ID, c.ID}, AlbumIDs: []string{set.ID}})
	require.NoError(t, err)
	require.Equal(t, int64(1), estimate.Files)
	for _, bad := range []PhotoCheckoutSelection{{}, {AssetIDs: []string{"bad"}}, {AlbumIDs: []string{"bad"}}, {Years: &PhotoCheckoutYears{From: 0, To: 2024}}, {Years: &PhotoCheckoutYears{From: 2025, To: 2024}}, {Years: &PhotoCheckoutYears{From: 2024, To: 10000}}} {
		_, err := s.EstimatePhotoCheckout(ctx, bad)
		require.ErrorIs(t, err, ErrInvalidPhotoCheckout)
	}
	_, err = s.EstimatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{"00000000-0000-4000-8000-000000000001"}})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPhotoCheckoutCapacity(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	a := albumAsset(t, s, "photo.jpg")
	_, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, "checkout", -1)
	require.ErrorContains(t, err, "byte_limit must be nonnegative")
	for _, limit := range []int64{0, 19} {
		_, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{All: true}, "checkout", limit)
		require.ErrorIs(t, err, ErrInvalidPhotoCheckout)
	}
	c, op, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{All: true}, "checkout", 20)
	require.NoError(t, err)
	require.Equal(t, StorageOperationKindPhotoCheckout, op.Kind)
	require.Equal(t, op.ID, c.OperationID)
	require.Equal(t, int64(1), op.TotalObjects)
}

func TestPhotoCheckoutPublicationRechecksVisibility(t *testing.T) {
	t.Parallel()
	for _, column := range []string{"hidden_at", "excluded_at"} {
		t.Run(column, func(t *testing.T) {
			s := newTestStore(t)
			a := albumAsset(t, s, "photo.jpg")
			c, _, err := s.CreatePhotoCheckout(t.Context(), PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, "checkout", 0)
			require.NoError(t, err)
			entries, err := s.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			_, err = s.db.ExecContext(t.Context(), `UPDATE photo_assets SET `+column+`=? WHERE asset_id=?`, nowRFC3339(), a.ID)
			require.NoError(t, err)
			called := false
			err = s.PublishPhotoCheckoutEntry(t.Context(), entries[0], func() (bool, error) { called = true; return true, nil })
			require.ErrorIs(t, err, ErrInvalidPhotoCheckout)
			require.False(t, called)
		})
	}
}

func TestPhotoCheckoutMetadataRestoresUnboundAndValidatesFrozenEntries(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "source.db"), driver.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			a := albumAsset(t, s, "photo.jpg")
			second := albumAsset(t, s, "second.jpg")
			c, _, err := s.CreatePhotoCheckout(t.Context(), PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, "machine-local-checkout", 0)
			require.NoError(t, err)
			entries, err := s.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			require.NoError(t, s.PublishPhotoCheckoutEntry(t.Context(), entries[0], func() (bool, error) { return true, nil }))
			require.NoError(t, s.FinishPhotoCheckout(t.Context(), c.ID, c.OperationID, StorageOperationFailed, "{}", "machine-local-error", time.Now().Add(time.Hour)))
			var exported bytes.Buffer
			require.NoError(t, s.ExportMetadata(t.Context(), &exported))
			require.NotContains(t, exported.String(), "machine-local-")
			restored, err := Open(filepath.Join(t.TempDir(), "restored.db"), driver.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, restored.Close()) })
			require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
			actual, err := restored.PhotoCheckout(t.Context(), c.ID)
			require.NoError(t, err)
			require.Equal(t, "unbound", actual.State)
			require.Empty(t, actual.Destination)
			require.Empty(t, actual.OperationID)
			require.Empty(t, actual.Error)
			actualEntries, err := restored.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			entries[0].Published = false
			require.Equal(t, entries, actualEntries)
			var roundtrip bytes.Buffer
			require.NoError(t, restored.ExportMetadata(t.Context(), &roundtrip))
			require.Equal(t, exported.String(), roundtrip.String())
			for _, bad := range []struct {
				metadata string
				path     bool
			}{
				{strings.Replace(exported.String(), `"relative_path":"undated/`, `"relative_path":"../`, 1), true},
				{strings.Replace(exported.String(), `"size":20,"relative_path"`, `"size":21,"relative_path"`, 1), false},
			} {
				invalid, err := Open(filepath.Join(t.TempDir(), "invalid.db"), driver.driver)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, invalid.Close()) })
				err = invalid.ImportMetadata(t.Context(), strings.NewReader(bad.metadata))
				if bad.path {
					require.ErrorIs(t, err, ErrInvalidPhotoCheckout)
				} else {
					require.ErrorContains(t, err, "photo checkout frozen bases do not match content versions")
				}
			}
			original := `"type":"photo_checkout_entry","checkout_id":"` + c.ID + `","file_id":"` + entries[0].FileID + `"`
			forged := `"type":"photo_checkout_entry","checkout_id":"` + c.ID + `","file_id":"` + second.Files[0].ID + `"`
			require.Contains(t, exported.String(), original)
			forgedFile, err := Open(filepath.Join(t.TempDir(), "forged.db"), driver.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, forgedFile.Close()) })
			require.ErrorContains(t, forgedFile.ImportMetadata(t.Context(), strings.NewReader(strings.Replace(exported.String(), original, forged, 1))), "photo checkout frozen bases do not match content versions")
			forgedAsset, err := Open(filepath.Join(t.TempDir(), "missing-asset.db"), driver.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, forgedAsset.Close()) })
			originalAsset := `"asset_id":"` + a.ID + `","version_id":`
			missingAsset := `"asset_id":"00000000-0000-4000-8000-000000000001","version_id":`
			require.Contains(t, exported.String(), originalAsset)
			require.ErrorContains(t, forgedAsset.ImportMetadata(t.Context(), strings.NewReader(strings.Replace(exported.String(), originalAsset, missingAsset, 1))), "photo checkout frozen bases do not match content versions")
			_, err = s.DetachPhotoFile(t.Context(), a.ID, a.Revision, a.Files[0].ID, PhotoDetachOptions{})
			require.NoError(t, err)
			exported.Reset()
			require.NoError(t, s.ExportMetadata(t.Context(), &exported))
			detached, err := Open(filepath.Join(t.TempDir(), "detached.db"), driver.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, detached.Close()) })
			require.NoError(t, detached.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
			actualEntries, err = detached.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			require.Equal(t, entries, actualEntries)
		})
	}
}

func TestPhotoSourceProvenanceMetadata(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	result, _ := seedGroupedCheckoutSources(t, s)
	raw := fileByRole(result.Asset.Files, PhotoRoleRAW)
	var backup bytes.Buffer
	err := s.ExportMetadata(ctx, &backup)
	require.NoError(t, err)
	var rawSource string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT source_provenance_identity FROM photo_files WHERE file_id=?`, raw.ID).Scan(&rawSource))
	var alternateSource string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT identity FROM provenance WHERE node_id=? AND original_path LIKE '%AAA.ARW'`, raw.NodeID).Scan(&alternateSource))
	for _, variant := range []string{"current", "legacy", "another node"} {
		expectedSource := rawSource
		metadata := backup.Bytes()
		if variant == "current" {
			_, err = s.db.ExecContext(ctx, `UPDATE photo_files SET source_provenance_identity=? WHERE file_id=?`, alternateSource, raw.ID)
			require.NoError(t, err)
			var current bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &current))
			metadata = current.Bytes()
			expectedSource = alternateSource
		}
		lines := bytes.Split(metadata, []byte{'\n'})
		for i, line := range lines {
			if !bytes.Contains(line, []byte(`"type":"photo_file"`)) {
				continue
			}
			var file metadataPhotoFile
			require.NoError(t, json.Unmarshal(line, &file))
			if variant == "legacy" {
				file.SourceProvenanceIdentity = nil
			} else if variant == "another node" && file.Role == PhotoRoleImage {
				file.SourceProvenanceIdentity = &rawSource
			}
			lines[i], err = json.Marshal(file)
			require.NoError(t, err)
		}
		restored := newTestStore(t)
		err = restored.ImportMetadata(ctx, bytes.NewReader(bytes.Join(lines, []byte{'\n'})))
		if variant == "another node" {
			require.ErrorContains(t, err, "invalid photo source provenance reference")
			continue
		}
		require.NoError(t, err)
		var storedSource string
		require.NoError(t, restored.db.QueryRowContext(ctx, `SELECT source_provenance_identity FROM photo_files WHERE file_id=?`, raw.ID).Scan(&storedSource))
		require.Equal(t, expectedSource, storedSource, variant)
	}
}

func TestPhotoCheckoutRejectsByteOverflow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.CreateFile(t.Context(), s.RootID(), "large.jpg", browseHash("large"), math.MaxInt64, "image/jpeg")
	require.NoError(t, err)
	albumAsset(t, s, "small.jpg")
	_, err = s.EstimatePhotoCheckout(t.Context(), PhotoCheckoutSelection{All: true})
	require.ErrorIs(t, err, ErrInvalidPhotoCheckout)
}

func TestPhotoCheckoutRejectedCreateLeavesNoCheckoutOrOperation(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, filename string }{
		{"empty all", ""}, {"audit refusal", ""},
	}
	for _, name := range []string{"CON.jpg", "bad:name.jpg", "bad?.jpg", "trailing.", "trailing ", "control\x01.jpg", "bad\\name.jpg"} {
		cases = append(cases, struct{ name, filename string }{name, name})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			selection := PhotoCheckoutSelection{All: true}
			expectedErr := ErrInvalidPhotoCheckout
			var asset PhotoAsset
			if tc.filename != "" || tc.name == "audit refusal" {
				asset = albumAsset(t, s, "photo.jpg")
				selection = PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}
				if tc.filename != "" {
					_, err := s.db.ExecContext(t.Context(), "UPDATE nodes SET name=? WHERE id=?", tc.filename, asset.Files[0].NodeID)
					require.NoError(t, err)
				} else {
					seedInitialAuditAuthority(t, s, s.RootID())
					expectedErr = ErrAuditMutationUnsupported
				}
			}
			_, _, err := s.CreatePhotoCheckout(t.Context(), selection, "checkout", 100)
			require.ErrorIs(t, err, expectedErr)
			if tc.filename != "" {
				require.Contains(t, err.Error(), "filename")
				require.Contains(t, err.Error(), asset.ID)
				require.Contains(t, err.Error(), asset.Files[0].ID)
			}
			checkouts, err := s.ListPhotoCheckouts(t.Context())
			require.NoError(t, err)
			require.Empty(t, checkouts)
			operations, err := s.StorageOperations(t.Context(), 10)
			require.NoError(t, err)
			require.Empty(t, operations)
		})
	}
}

func TestPhotoCheckoutStatusTracksOperationAcrossPauseAndFinish(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset := albumAsset(t, s, "photo.jpg")
	checkout, op, err := s.CreatePhotoCheckout(t.Context(), PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, "checkout", 0)
	require.NoError(t, err)
	_, err = s.ClaimStorageOperation(t.Context(), op.ID)
	require.NoError(t, err)
	require.NoError(t, s.ClaimPhotoCheckout(t.Context(), checkout.ID, op.ID))
	require.NoError(t, s.DeferStorageOperation(t.Context(), op.ID, errors.New("waiting for readable lane controls")))
	paused, err := s.PhotoCheckout(t.Context(), checkout.ID)
	require.NoError(t, err)
	require.Equal(t, "queued", paused.State)
	require.Contains(t, paused.Error, "lane controls")
	list, err := s.ListPhotoCheckouts(t.Context())
	require.NoError(t, err)
	require.Equal(t, paused, list[0])
	_, err = s.ClaimStorageOperation(t.Context(), op.ID)
	require.NoError(t, err)
	resumed, err := s.PhotoCheckout(t.Context(), checkout.ID)
	require.NoError(t, err)
	require.Equal(t, "running", resumed.State)
	require.Empty(t, resumed.Error)
	require.NoError(t, s.FinishStorageOperation(t.Context(), op.ID, StorageOperationFailed, "{}", "terminal error", time.Now().Add(time.Hour)))
	finished, err := s.PhotoCheckout(t.Context(), checkout.ID)
	require.NoError(t, err)
	require.Equal(t, string(StorageOperationFailed), finished.State)
	require.Equal(t, "terminal error", finished.Error)
}

func TestPhotoCheckoutFreezesGroupedSourcePaths(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	result, source := seedGroupedCheckoutSources(t, s)
	raw := fileByRole(result.Asset.Files, PhotoRoleRAW)
	checkout, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{result.Asset.ID}}, "checkout", 0)
	require.NoError(t, err)
	entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	for _, entry := range entries {
		require.True(t, strings.HasPrefix(entry.Path, "2024/01/02/"+result.Asset.ID+"/"))
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, filepath.Base(e.Path))
	}
	require.ElementsMatch(t, []string{"IMG.ARW", "IMG.JPG", "IMG.ARW.XMP"}, names)
	node, err := s.NodeByID(ctx, raw.NodeID)
	require.NoError(t, err)
	_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent", SourceDescription: "synthetic", OriginalPath: filepath.Join(source, "ignored.ARW")})
	require.NoError(t, err)
	retained, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{result.Asset.ID}}, "retained", 0)
	require.NoError(t, err)
	entries, err = s.PhotoCheckoutEntries(ctx, retained.ID)
	require.NoError(t, err)
	rawIndex := slices.IndexFunc(entries, func(entry PhotoCheckoutEntry) bool { return entry.FileID == raw.ID })
	require.NotEqual(t, -1, rawIndex)
	require.Equal(t, "IMG.ARW", filepath.Base(entries[rawIndex].Path))
	fallbackAsset := albumAsset(t, s, "fallback.jpg")
	node, err = s.NodeByID(ctx, fallbackAsset.Files[0].NodeID)
	require.NoError(t, err)
	_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent", SourceDescription: "synthetic", OriginalPath: filepath.Join(source, "ignored.jpg")})
	require.NoError(t, err)
	fallback, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{fallbackAsset.ID}}, "fallback", 0)
	require.NoError(t, err)
	entries, err = s.PhotoCheckoutEntries(ctx, fallback.ID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, node.Name, filepath.Base(entries[0].Path))
}

func TestPhotoCheckoutNormalizesSourcePaths(t *testing.T) {
	t.Parallel()
	for _, sourcePath := range []string{`C:\camera\capture.JPG`, `\\camera\photos\capture.JPG`, `/camera/foo\bar.JPG`} {
		t.Run(sourcePath, func(t *testing.T) {
			s := newTestStore(t)
			run, err := s.BeginIngest(t.Context(), "photo-import", t.TempDir())
			require.NoError(t, err)
			member := photoImportTestMember(sourcePath, PhotoRoleImage, browseHash("capture"), "image/jpeg")
			member.Name = "renamed.JPG"
			result, err := s.IngestPhotoGroup(t.Context(), run, photoImportTestGroup(member))
			require.NoError(t, err)
			checkout, _, err := s.CreatePhotoCheckout(t.Context(), PhotoCheckoutSelection{AssetIDs: []string{result.Asset.ID}}, "checkout", 0)
			if strings.HasPrefix(sourcePath, "/camera/") {
				require.ErrorIs(t, err, ErrInvalidPhotoCheckout)
				require.ErrorContains(t, err, `filename "foo\\bar.JPG"`)
				return
			}
			require.NoError(t, err)
			entries, err := s.PhotoCheckoutEntries(t.Context(), checkout.ID)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, "undated/"+result.Asset.ID+"/capture.JPG", entries[0].Path)
		})
	}
}

func seedGroupedCheckoutSources(t *testing.T, s *Store) (PhotoImportResult, string) {
	t.Helper()
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	source := filepath.Join(t.TempDir(), "camera")
	result, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(
		photoImportTestMember(filepath.Join(source, "IMG.ARW"), PhotoRoleRAW, browseHash("raw"), "image/x-sony-arw"),
		photoImportTestMember(filepath.Join(source, "IMG.JPG"), PhotoRoleImage, browseHash("jpeg"), "image/jpeg"),
		photoImportTestMember(filepath.Join(source, "IMG.ARW.XMP"), PhotoRoleSidecar, browseHash("xmp"), "application/rdf+xml"),
	))
	require.NoError(t, err)
	laterRun, err := s.BeginIngest(ctx, "photo-import", "renamed duplicates")
	require.NoError(t, err)
	for _, observation := range []struct {
		run  IngestRun
		name string
	}{{run, "AAA"}, {laterRun, "RENAMED"}} {
		duplicate, err := s.IngestPhotoGroup(ctx, observation.run, photoImportTestGroup(
			photoImportTestMember(filepath.Join(source, observation.name+".ARW"), PhotoRoleRAW, browseHash("raw"), "image/x-sony-arw"),
			photoImportTestMember(filepath.Join(source, observation.name+".JPG"), PhotoRoleImage, browseHash("jpeg"), "image/jpeg"),
		))
		require.NoError(t, err)
		require.Equal(t, result.Asset.ID, duplicate.Asset.ID)
	}
	raw := fileByRole(result.Asset.Files, PhotoRoleRAW)
	rawNode, err := s.NodeByID(ctx, raw.NodeID)
	require.NoError(t, err)
	rawNode, _, err = s.ReplaceContent(ctx, rawNode.ID, rawNode.Revision, browseHash("replacement raw"), 20, "image/x-sony-arw")
	require.NoError(t, err)
	pruned, err := s.PruneContentVersions(ctx, rawNode.ID, rawNode.Revision, VersionPruneSelector{AllPrior: true}, true)
	require.NoError(t, err)
	require.Equal(t, 1, pruned.DeletedVersions)
	for _, node := range result.Nodes {
		node, err = s.NodeByID(ctx, node.ID)
		require.NoError(t, err)
		date := "2025-12-31"
		if node.Name == "IMG.ARW" {
			date = "2024-01-02"
		}
		browsePhotoMetadata(t, s, node, node.Name, photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(date, date, document.SourceMetadataPrecisionDate, document.SourceMetadataTimezoneOmitted, "")))
	}
	for i, node := range result.Nodes {
		node, err = s.NodeByID(ctx, node.ID)
		require.NoError(t, err)
		_, _, err = s.Move(ctx, node.ID, s.RootID(), fmt.Sprintf("virtual-%d.%s", i, strings.TrimPrefix(filepath.Ext(node.Name), ".")), node.Revision)
		require.NoError(t, err)
	}
	return result, source
}
