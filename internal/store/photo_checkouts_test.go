package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPhotoCheckoutSelections(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	var assets []PhotoAsset
	for _, v := range []struct {
		name, date string
		size       int64
	}{{"first.jpg", "2024-01-01", 10}, {"last.jpg", "2025-12-31", 20}, {"outside.jpg", "2026-01-01", 40}} {
		n, err := s.CreateFile(ctx, s.RootID(), v.name, browseHash(v.name), v.size, "image/jpeg")
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(ctx, n.ID)
		require.NoError(t, err)
		if v.name == "first.jpg" {
			run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
			require.NoError(t, err)
			group := photoImportTestGroup(
				photoImportTestMember("first.ARW", PhotoRoleRAW, browseHash("raw"), "image/x-sony-arw"),
				photoImportTestMember("first.jpg", PhotoRoleImage, browseHash(v.name), "image/jpeg"),
				photoImportTestMember("first.ARW.XMP", PhotoRoleSidecar, browseHash("xmp"), "application/rdf+xml"))
			group.Members[0].Size = v.size + 1
			group.Members[1].Size = v.size
			group.Members[2].Size = v.size + 2
			result, err := s.IngestPhotoGroup(ctx, run, group)
			require.NoError(t, err)
			asset = result.Asset
			n, err = s.NodeByID(ctx, fileByRole(asset.Files, PhotoRoleRAW).NodeID)
			require.NoError(t, err)
		}
		assets = append(assets, asset)
		browsePhotoMetadata(t, s, n, v.date, photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(v.date, v.date, document.SourceMetadataPrecisionDate, document.SourceMetadataTimezoneOmitted, "")))
	}
	a, b, c := assets[0], assets[1], assets[2]
	set, err := s.CreatePhotoSet(ctx, "Trip")
	require.NoError(t, err)
	_, err = s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: []string{a.ID, b.ID}})
	require.NoError(t, err)
	for _, test := range []struct {
		selection            PhotoCheckoutSelection
		assets, files, bytes int64
		selected             []PhotoAsset
	}{
		{PhotoCheckoutSelection{All: true}, 3, 5, 93, []PhotoAsset{a, b, c}},
		{PhotoCheckoutSelection{AssetIDs: []string{a.ID, a.ID}}, 1, 3, 33, []PhotoAsset{a}},
		{PhotoCheckoutSelection{AlbumIDs: []string{set.ID}}, 2, 4, 53, []PhotoAsset{a, b}},
		{PhotoCheckoutSelection{Years: &PhotoCheckoutYears{From: 2024, To: 2025}}, 2, 4, 53, []PhotoAsset{a, b}},
		{PhotoCheckoutSelection{AssetIDs: []string{c.ID}, AlbumIDs: []string{set.ID}, Years: &PhotoCheckoutYears{From: 2024, To: 2025}}, 3, 5, 93, []PhotoAsset{a, b, c}},
	} {
		estimate, err := s.EstimatePhotoCheckout(ctx, test.selection)
		require.NoError(t, err)
		require.Equal(t, test.assets, estimate.Assets)
		require.Equal(t, test.files, estimate.Files)
		require.Equal(t, test.bytes, estimate.Bytes)
		checkout, _, _, err := s.CreatePhotoCheckout(ctx, test.selection, t.TempDir(), test.bytes)
		require.NoError(t, err)
		entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
		require.NoError(t, err)
		expectedVersions := make(map[string]string)
		for _, asset := range test.selected {
			for _, file := range asset.Files {
				node, err := s.NodeByID(ctx, file.NodeID)
				require.NoError(t, err)
				expectedVersions[file.ID] = node.CurrentVersionID
			}
		}
		actualVersions := make(map[string]string)
		for _, entry := range entries {
			actualVersions[entry.FileID] = entry.VersionID
		}
		require.Len(t, entries, len(expectedVersions))
		require.Equal(t, expectedVersions, actualVersions)
	}
	_, err = s.SetPhotoAssetExcluded(ctx, c.ID, c.Revision, true)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, b.ID, b.Revision, true)
	require.NoError(t, err)
	var eligibleFiles []string
	for _, file := range a.Files {
		eligibleFiles = append(eligibleFiles, file.ID)
	}
	for _, selection := range []PhotoCheckoutSelection{
		{AssetIDs: []string{a.ID, b.ID, c.ID}, AlbumIDs: []string{set.ID}},
		{All: true},
		{Years: &PhotoCheckoutYears{From: 2024, To: 2025}},
	} {
		estimate, err := s.EstimatePhotoCheckout(ctx, selection)
		require.NoError(t, err)
		require.Equal(t, PhotoCheckoutEstimate{Assets: 1, Files: 3, Bytes: 33}, estimate)
		checkout, _, _, err := s.CreatePhotoCheckout(ctx, selection, t.TempDir(), estimate.Bytes)
		require.NoError(t, err)
		entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
		require.NoError(t, err)
		var selectedFiles []string
		for _, entry := range entries {
			selectedFiles = append(selectedFiles, entry.FileID)
		}
		require.ElementsMatch(t, eligibleFiles, selectedFiles)
	}
}

func TestPhotoCheckoutMetadata(t *testing.T) {
	t.Parallel()
	type fixture struct {
		asset, second PhotoAsset
		checkout      PhotoCheckout
		entries       []PhotoCheckoutEntry
		snapshot      []byte
	}
	makeFixture := func(t *testing.T, s *Store) fixture {
		t.Helper()
		a := albumAsset(t, s, "photo.jpg")
		second := albumAsset(t, s, "second.jpg")
		c, _, _, err := s.CreatePhotoCheckout(t.Context(), PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, "machine-local-checkout", 100)
		require.NoError(t, err)
		entries, err := s.PhotoCheckoutEntries(t.Context(), c.ID)
		require.NoError(t, err)
		require.NoError(t, s.PublishPhotoCheckoutEntry(t.Context(), entries[0], func() (bool, error) { return true, nil }))
		require.NoError(t, s.FinishPhotoCheckout(t.Context(), c.ID, c.OperationID, StorageOperationCompleted, "{}", "", time.Now().Add(time.Hour)))
		var exported bytes.Buffer
		require.NoError(t, s.ExportMetadata(t.Context(), &exported))
		return fixture{a, second, c, entries, exported.Bytes()}
	}
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			s := newTestStoreWithDriver(t, driver.driver)
			f := makeFixture(t, s)
			require.NotContains(t, string(f.snapshot), "machine-local-")
			restored := newTestStoreWithDriver(t, driver.driver)
			require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(f.snapshot)))
			actual, err := restored.PhotoCheckout(t.Context(), f.checkout.ID)
			require.NoError(t, err)
			require.Equal(t, f.checkout.Selection, actual.Selection)
			require.Equal(t, f.checkout.ByteLimit, actual.ByteLimit)
			require.Equal(t, "unbound", actual.State)
			require.Empty(t, actual.Destination)
			require.Empty(t, actual.OperationID)
			require.Empty(t, actual.Error)
			actualEntries, err := restored.PhotoCheckoutEntries(t.Context(), f.checkout.ID)
			require.NoError(t, err)
			require.Equal(t, f.entries, actualEntries)
			var roundtrip bytes.Buffer
			require.NoError(t, restored.ExportMetadata(t.Context(), &roundtrip))
			require.Equal(t, string(f.snapshot), roundtrip.String())
			t.Run("retired checkout round trip", func(t *testing.T) {
				retired, err := s.RetirePhotoCheckout(t.Context(), f.checkout.ID)
				require.NoError(t, err)
				var retiredSnapshot bytes.Buffer
				require.NoError(t, s.ExportMetadata(t.Context(), &retiredSnapshot))
				retiredRestored := newTestStoreWithDriver(t, driver.driver)
				require.NoError(t, retiredRestored.ImportMetadata(t.Context(), bytes.NewReader(retiredSnapshot.Bytes())))
				actualRetired, err := retiredRestored.PhotoCheckout(t.Context(), f.checkout.ID)
				require.NoError(t, err)
				require.Equal(t, retired.RetiredAt, actualRetired.RetiredAt)
				require.True(t, actualRetired.RetentionReleased)
			})
		})
	}
	t.Run("restore colon filename on Windows", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("Windows filename compatibility")
		}
		f := makeFixture(t, newTestStore(t))
		colonPath := "undated/" + f.asset.ID + "/Trip: Day 1.jpg"
		portableLines := bytes.Split(f.snapshot, []byte{'\n'})
		for i, line := range portableLines {
			if !bytes.Contains(line, []byte(`"type":"photo_checkout_entry"`)) {
				continue
			}
			var entry metadataPhotoCheckoutEntry
			require.NoError(t, json.Unmarshal(line, &entry))
			entry.Path = colonPath
			encoded, err := json.Marshal(entry)
			portableLines[i] = encoded
			require.NoError(t, err)
		}
		windowsRestored, err := Open(filepath.Join(t.TempDir(), "windows-colon-restored.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, windowsRestored.Close()) })
		require.NoError(t, windowsRestored.ImportMetadata(t.Context(), bytes.NewReader(bytes.Join(portableLines, []byte{'\n'}))))
		actual, err := windowsRestored.PhotoCheckout(t.Context(), f.checkout.ID)
		require.NoError(t, err)
		require.Equal(t, "unbound", actual.State)
		actualEntries, err := windowsRestored.PhotoCheckoutEntries(t.Context(), f.checkout.ID)
		require.NoError(t, err)
		require.Equal(t, colonPath, actualEntries[0].Path)
		require.NoError(t, windowsRestored.ExportMetadata(t.Context(), &bytes.Buffer{}))
	})
	for _, bad := range []struct {
		name   string
		mutate func(*metadataPhotoCheckoutEntry, fixture)
		path   bool
	}{
		{"unsafe-path", func(entry *metadataPhotoCheckoutEntry, _ fixture) { entry.Path = "../photo.jpg" }, true},
		{"size", func(entry *metadataPhotoCheckoutEntry, _ fixture) { entry.Size++ }, false},
		{"file", func(entry *metadataPhotoCheckoutEntry, f fixture) { entry.FileID = f.second.Files[0].ID }, false},
		{"asset", func(entry *metadataPhotoCheckoutEntry, _ fixture) {
			entry.AssetID = "00000000-0000-4000-8000-000000000001"
		}, false},
	} {
		t.Run("invalid frozen entry "+bad.name, func(t *testing.T) {
			f := makeFixture(t, newTestStore(t))
			lines := bytes.Split(f.snapshot, []byte{'\n'})
			changed := false
			for i, line := range lines {
				if len(line) == 0 {
					continue
				}
				var entry metadataPhotoCheckoutEntry
				require.NoError(t, json.Unmarshal(line, &entry))
				if entry.Type == "photo_checkout_entry" {
					bad.mutate(&entry, f)
					encoded, err := json.Marshal(entry)
					lines[i] = encoded
					require.NoError(t, err)
					changed = true
				}
			}
			require.True(t, changed, bad.name)
			invalid, err := Open(filepath.Join(t.TempDir(), bad.name+".db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, invalid.Close()) })
			err = invalid.ImportMetadata(t.Context(), bytes.NewReader(bytes.Join(lines, []byte{'\n'})))
			if bad.path {
				require.ErrorIs(t, err, ErrInvalidPhotoCheckout)
			} else {
				require.ErrorContains(t, err, "photo checkout frozen bases do not match content versions")
			}
		})
	}
}

func TestPhotoCheckoutCreateValidation(t *testing.T) {
	t.Parallel()
	type rejectedCase struct {
		name                                       string
		limit                                      int64
		withAsset, explicitSelection, auditRefusal bool
		setSelection, overflow                     bool
		selection                                  PhotoCheckoutSelection
		expectedErr                                error
		errorText                                  string
	}
	cases := []rejectedCase{
		{name: "all with assets", limit: 100, setSelection: true, selection: PhotoCheckoutSelection{All: true, AssetIDs: []string{"00000000-0000-4000-8000-000000000001"}}, errorText: "all cannot be combined"},
		{name: "empty all", limit: 100, errorText: "selection contains no files"},
		{name: "empty selection", limit: 100, setSelection: true, errorText: "select assets, albums, years, or all"},
		{name: "invalid asset ID", limit: 100, setSelection: true, selection: PhotoCheckoutSelection{AssetIDs: []string{"bad"}}},
		{name: "invalid album ID", limit: 100, setSelection: true, selection: PhotoCheckoutSelection{AlbumIDs: []string{"bad"}}},
		{name: "invalid year start", limit: 100, setSelection: true, selection: PhotoCheckoutSelection{Years: &PhotoCheckoutYears{From: 0, To: 2024}}},
		{name: "invalid year range", limit: 100, setSelection: true, selection: PhotoCheckoutSelection{Years: &PhotoCheckoutYears{From: 2025, To: 2024}}},
		{name: "invalid year end", limit: 100, setSelection: true, selection: PhotoCheckoutSelection{Years: &PhotoCheckoutYears{From: 2024, To: 10000}}},
		{name: "unknown asset ID", limit: 100, setSelection: true, selection: PhotoCheckoutSelection{AssetIDs: []string{"00000000-0000-4000-8000-000000000001"}}, expectedErr: ErrNotFound},
		{name: "byte overflow", limit: 100, withAsset: true, overflow: true},
		{name: "audit refusal", limit: 100, withAsset: true, explicitSelection: true, auditRefusal: true},
		{name: "negative limit", limit: -1, withAsset: true, explicitSelection: true, errorText: "byte_limit must be nonnegative"},
		{name: "all without limit", withAsset: true, errorText: "all requires a positive byte_limit"},
		{name: "insufficient limit", limit: 19, withAsset: true, errorText: "selected bytes exceed byte_limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			selection := PhotoCheckoutSelection{All: true}
			if tc.setSelection {
				selection = tc.selection
			}
			expectedErr := tc.expectedErr
			if expectedErr == nil {
				expectedErr = ErrInvalidPhotoCheckout
			}
			var asset PhotoAsset
			if tc.withAsset {
				asset = albumAsset(t, s, "photo.jpg")
				if tc.explicitSelection {
					selection = PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}
				}
				if tc.auditRefusal {
					seedInitialAuditAuthority(t, s, s.RootID())
					expectedErr = ErrAuditMutationUnsupported
				}
			}
			if tc.overflow {
				_, err := s.CreateFile(t.Context(), s.RootID(), "large.jpg", browseHash("large"), math.MaxInt64, "image/jpeg")
				require.NoError(t, err)
			}
			_, _, _, err := s.CreatePhotoCheckout(t.Context(), selection, "checkout", tc.limit)
			require.ErrorIs(t, err, expectedErr)
			if tc.errorText != "" {
				require.ErrorContains(t, err, tc.errorText)
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
	albumAsset(t, s, "photo.jpg")
	checkout, op, _, err := s.CreatePhotoCheckout(t.Context(), PhotoCheckoutSelection{All: true}, "checkout", 20)
	require.NoError(t, err)
	require.Equal(t, StorageOperationKindPhotoCheckout, op.Kind)
	require.Equal(t, op.ID, checkout.OperationID)
	require.Equal(t, int64(1), op.TotalObjects)
	status, err := s.PhotoCheckoutStatus(t.Context(), checkout.ID)
	require.NoError(t, err)
	require.Equal(t, PhotoCheckoutStatus{Checkout: checkout, Files: 1, Bytes: 20}, status)
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
	entries, err := s.PhotoCheckoutEntries(t.Context(), checkout.ID)
	require.NoError(t, err)
	require.NoError(t, s.PublishPhotoCheckoutEntry(t.Context(), entries[0], func() (bool, error) { return true, nil }))
	status, err = s.PhotoCheckoutStatus(t.Context(), checkout.ID)
	require.NoError(t, err)
	require.Equal(t, PhotoCheckoutStatus{Checkout: resumed, Files: 1, Bytes: 20, PublishedFiles: 1, PublishedBytes: 20}, status)
	_, err = s.RetirePhotoCheckout(t.Context(), checkout.ID)
	require.ErrorIs(t, err, ErrInvalidPhotoCheckout)
	require.NoError(t, s.FinishStorageOperation(t.Context(), op.ID, StorageOperationFailed, "{}", "terminal error", time.Now().Add(time.Hour)))
	finished, err := s.PhotoCheckout(t.Context(), checkout.ID)
	require.NoError(t, err)
	require.Equal(t, string(StorageOperationFailed), finished.State)
	require.Equal(t, "terminal error", finished.Error)
	retired, err := s.RetirePhotoCheckout(t.Context(), checkout.ID)
	require.NoError(t, err)
	require.NotNil(t, retired.RetiredAt)
	finished.RetentionReleased = true
	finished.RetiredAt = retired.RetiredAt
	require.Equal(t, finished, retired)
	again, err := s.RetirePhotoCheckout(t.Context(), checkout.ID)
	require.NoError(t, err)
	require.Equal(t, retired, again)
}

func TestPhotoCheckoutConflictsAndEntryPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, sidecar, trashRole, captureDate string
		multipleSidecars, skipConflict        bool
		paths                                 map[string]string
	}{
		{name: "separate ingests", sidecar: "IMG.ARW.XMP", captureDate: "2024-06-15", paths: map[string]string{"raw": "2024/06/15/%s/IMG.ARW", "jpeg": "2024/06/15/%s/IMG.JPG", "sidecar": "2024/06/15/%s/IMG.ARW.XMP"}},
		{name: "trashed target", sidecar: "IMG.ARW.XMP", trashRole: PhotoRoleRAW, paths: map[string]string{"jpeg": "undated/%s/IMG.JPG"}},
		{name: "conflicting pair", sidecar: "unrelated.XMP", skipConflict: true},
		{name: "trashed conflicting sidecar", sidecar: "unrelated.XMP", trashRole: PhotoRoleSidecar, paths: map[string]string{"raw": "undated/%s/IMG.ARW", "jpeg": "undated/%s/IMG.JPG"}},
		{name: "multiple sidecars", multipleSidecars: true, paths: map[string]string{"raw": "undated/%s/RENAMED.ARW", "jpeg": "undated/%s/IMG.JPG", "first sidecar": "undated/%s/RENAMED.XMP", "second sidecar": "undated/%s/RENAMED.ARW.XMP"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			source := t.TempDir()
			run, err := s.BeginIngest(ctx, "photo-import", source)
			require.NoError(t, err)
			members := []PhotoImportMember{
				photoImportTestMember(filepath.Join(source, "IMG.ARW"), PhotoRoleRAW, browseHash("raw"), "image/x-sony-arw"),
				photoImportTestMember(filepath.Join(source, "IMG.JPG"), PhotoRoleImage, browseHash("jpeg"), "image/jpeg"),
			}
			if tc.multipleSidecars {
				members = append(members, photoImportTestMember(filepath.Join(source, "IMG.XMP"), PhotoRoleSidecar, browseHash("first sidecar"), "application/rdf+xml"))
			}
			result, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(members...))
			require.NoError(t, err)
			if tc.multipleSidecars {
				for _, observation := range []struct{ sidecar, hash string }{{"RENAMED.ARW.XMP", "second sidecar"}, {"RENAMED.XMP", "first sidecar"}} {
					run, err = s.BeginIngest(ctx, "photo-import", source)
					require.NoError(t, err)
					result, err = s.IngestPhotoGroup(ctx, run, photoImportTestGroup(
						photoImportTestMember(filepath.Join(source, "RENAMED.ARW"), PhotoRoleRAW, browseHash("raw"), "image/x-sony-arw"),
						photoImportTestMember(filepath.Join(source, observation.sidecar), PhotoRoleSidecar, browseHash(observation.hash), "application/rdf+xml")))
					require.NoError(t, err)
				}
			} else {
				run, err = s.BeginIngest(ctx, "photo-import", source)
				require.NoError(t, err)
				node, err := s.IngestFileExact(ctx, run, s.RootID(), tc.sidecar, browseHash("sidecar"), 20, "application/rdf+xml", filepath.Join(source, tc.sidecar), "")
				require.NoError(t, err)
				target := fileByRole(result.Asset.Files, PhotoRoleRAW).ID
				result.Asset, err = s.AttachPhotoFile(ctx, result.Asset.ID, result.Asset.Revision, node.ID, PhotoRoleSidecar, &target)
				require.NoError(t, err)
				if tc.skipConflict {
					good := albumAsset(t, s, "good.jpg")
					selection := PhotoCheckoutSelection{All: true}
					estimate, err := s.EstimatePhotoCheckout(ctx, selection)
					require.NoError(t, err)
					require.Equal(t, int64(1), estimate.Assets)
					require.Equal(t, int64(1), estimate.Files)
					require.Equal(t, int64(20), estimate.Bytes)
					require.Equal(t, []PhotoCheckoutSkip{{AssetID: result.Asset.ID, FileID: fileByRole(result.Asset.Files, PhotoRoleSidecar).ID, Name: "unrelated.XMP", Reason: "incompatible source names for target " + target}}, estimate.SkippedPhotos)
					require.Equal(t, int64(1), estimate.SkippedPhotoCount)
					checkout, _, createdEstimate, err := s.CreatePhotoCheckout(ctx, selection, "checkout", 20)
					require.NoError(t, err)
					require.Equal(t, estimate, createdEstimate)
					entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
					require.NoError(t, err)
					require.Len(t, entries, 1)
					require.Equal(t, good.ID, entries[0].AssetID)
					return
				}
				for i, file := range result.Asset.Files {
					node, err := s.NodeByID(ctx, file.NodeID)
					require.NoError(t, err)
					node, _, err = s.Move(ctx, node.ID, s.RootID(), fmt.Sprintf("virtual-%d.dat", i), node.Revision)
					require.NoError(t, err)
					if file.Role == tc.trashRole {
						_, _, err = s.Trash(ctx, node.ID, node.Revision)
						require.NoError(t, err)
					}
				}
				if tc.trashRole == PhotoRoleRAW {
					jpeg := fileByRole(result.Asset.Files, PhotoRoleImage).ID
					_, err = s.SetPhotoDisplay(ctx, result.Asset.ID, result.Asset.Revision, &jpeg)
					require.NoError(t, err)
				}
			}
			if tc.captureDate != "" {
				node, err := s.NodeByID(ctx, fileByRole(result.Asset.Files, PhotoRoleRAW).NodeID)
				require.NoError(t, err)
				browsePhotoMetadata(t, s, node, tc.captureDate, photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(tc.captureDate, tc.captureDate, document.SourceMetadataPrecisionDate, document.SourceMetadataTimezoneOmitted, "")))
			}
			checkout, _, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{result.Asset.ID}}, "checkout", 0)
			require.NoError(t, err)
			entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
			require.NoError(t, err)
			expected := make(map[string]string, len(tc.paths))
			for _, file := range result.Asset.Files {
				node, err := s.NodeByID(ctx, file.NodeID)
				require.NoError(t, err)
				for hash, template := range tc.paths {
					if node.BlobHash == browseHash(hash) {
						expected[file.ID] = fmt.Sprintf(template, result.Asset.ID)
					}
				}
			}
			require.Len(t, expected, len(tc.paths))
			actual := make(map[string]string, len(entries))
			for _, entry := range entries {
				actual[entry.FileID] = entry.Path
			}
			require.Equal(t, expected, actual)
		})
	}
	t.Run("first observation after replacement pruning and rename", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
		require.NoError(t, err)
		source := filepath.Join(t.TempDir(), "camera")
		result, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(
			photoImportTestMember(filepath.Join(source, "IMG.ARW"), PhotoRoleRAW, browseHash("raw"), "image/x-sony-arw"),
			photoImportTestMember(filepath.Join(source, "IMG.JPG"), PhotoRoleImage, browseHash("jpeg"), "image/jpeg"),
		))
		require.NoError(t, err)
		sidecarRun, err := s.BeginIngest(ctx, "photo-import", source)
		require.NoError(t, err)
		sidecar, err := s.IngestFileExact(ctx, sidecarRun, s.RootID(), "IMG.ARW.XMP", browseHash("xmp"), 4, "application/rdf+xml", filepath.Join(source, "IMG.ARW.XMP"), "")
		require.NoError(t, err)
		target := fileByRole(result.Asset.Files, PhotoRoleRAW).ID
		result.Asset, err = s.AttachPhotoFile(ctx, result.Asset.ID, result.Asset.Revision, sidecar.ID, PhotoRoleSidecar, &target)
		require.NoError(t, err)
		laterRun, err := s.BeginIngest(ctx, "photo-import", "renamed duplicates")
		require.NoError(t, err)
		for _, observation := range []struct {
			run  IngestRun
			name string
		}{{run, "AAA"}, {laterRun, "RENAMED"}} {
			members := []PhotoImportMember{
				photoImportTestMember(filepath.Join(source, observation.name+".ARW"), PhotoRoleRAW, browseHash("raw"), "image/x-sony-arw"),
				photoImportTestMember(filepath.Join(source, observation.name+".JPG"), PhotoRoleImage, browseHash("jpeg"), "image/jpeg"),
			}
			if observation.name == "RENAMED" {
				members = append(members, photoImportTestMember(filepath.Join(source, "RENAMED.ARW.XMP"), PhotoRoleSidecar, browseHash("xmp"), "application/rdf+xml"))
			}
			duplicate, err := s.IngestPhotoGroup(ctx, observation.run, photoImportTestGroup(members...))
			require.NoError(t, err)
			require.Equal(t, result.Asset.ID, duplicate.Asset.ID)
		}
		raw := fileByRole(result.Asset.Files, PhotoRoleRAW)
		rawNode, err := s.NodeByID(ctx, raw.NodeID)
		require.NoError(t, err)
		originalRAWVersion := rawNode.CurrentVersionID
		rawNode, _, err = s.ReplaceContent(ctx, rawNode.ID, rawNode.Revision, browseHash("replacement raw"), 20, "image/x-sony-arw")
		require.NoError(t, err)
		_, err = s.PruneContentVersions(ctx, rawNode.ID, rawNode.Revision, VersionPruneSelector{AllPrior: true}, true)
		require.NoError(t, err)
		_, err = s.ContentVersionByID(ctx, originalRAWVersion)
		require.ErrorIs(t, err, ErrNotFound)
		for i, node := range result.Nodes {
			node, err = s.NodeByID(ctx, node.ID)
			require.NoError(t, err)
			_, _, err = s.Move(ctx, node.ID, s.RootID(), fmt.Sprintf("virtual-%d.%s", i, strings.TrimPrefix(filepath.Ext(node.Name), ".")), node.Revision)
			require.NoError(t, err)
		}
		checkout, _, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{result.Asset.ID}}, "checkout", 0)
		require.NoError(t, err)
		entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
		require.NoError(t, err)
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, filepath.Base(entry.Path))
		}
		require.ElementsMatch(t, []string{"IMG.ARW", "IMG.JPG", "IMG.ARW.XMP"}, names)
	})
	for _, tc := range []struct{ name, sourcePath, basename string }{
		{"Windows source path", `C:\camera\capture.JPG`, "capture.JPG"},
		{"UNC source path", `\\camera\photos\capture.JPG`, "capture.JPG"},
		{"Unix source path with backslash", `/camera/foo\bar.JPG`, `foo\bar.JPG`},
		{"Unix source path with colon", `/camera/Trip: Day 1.jpg`, "Trip: Day 1.jpg"},
		{"node name fallback", "", "fallback.jpg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.HasPrefix(tc.sourcePath, "/camera/") {
				t.Skip("source basename is incompatible with Windows filenames")
			}
			s := newTestStore(t)
			ctx := t.Context()
			var asset PhotoAsset
			if tc.sourcePath == "" {
				asset = albumAsset(t, s, tc.basename)
				node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
				require.NoError(t, err)
				_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent", SourceDescription: "synthetic", OriginalPath: filepath.Join(t.TempDir(), "ignored.jpg")})
				require.NoError(t, err)
			} else {
				run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
				require.NoError(t, err)
				member := photoImportTestMember(tc.sourcePath, PhotoRoleImage, browseHash("capture"), "image/jpeg")
				member.Name = "renamed.JPG"
				result, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(member))
				require.NoError(t, err)
				asset = result.Asset
			}
			checkout, _, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, "checkout", 0)
			require.NoError(t, err)
			entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, "undated/"+asset.ID+"/"+tc.basename, entries[0].Path)
		})
	}
}

func TestPhotoCheckoutPreservesNodeBasenameWithoutProvenance(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("node basename is incompatible with Windows filenames")
	}
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, `C:\capture.jpg`)
	checkout, _, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, "checkout", 0)
	require.NoError(t, err)
	entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "undated/"+asset.ID+`/C:\capture.jpg`, entries[0].Path)
}

func TestPhotoCheckoutEntryRequiresFrozenEligibleSource(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{"trashed", "retargeted", "unpublished", "detached own file"} {
		t.Run(mutation, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			source := t.TempDir()
			run, err := s.BeginIngest(ctx, "photo-import", source)
			require.NoError(t, err)
			result, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(
				photoImportTestMember(filepath.Join(source, "IMG.ARW"), PhotoRoleRAW, browseHash("raw"), "image/x-sony-arw"),
				photoImportTestMember(filepath.Join(source, "IMG.JPG"), PhotoRoleImage, browseHash("jpeg"), "image/jpeg"),
				photoImportTestMember(filepath.Join(source, "IMG.ARW.XMP"), PhotoRoleSidecar, browseHash("xmp"), "application/rdf+xml")))
			require.NoError(t, err)
			asset := result.Asset
			asset, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, new(fileByRole(asset.Files, PhotoRoleImage).ID))
			require.NoError(t, err)
			checkout, _, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, "checkout", 0)
			require.NoError(t, err)
			entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
			require.NoError(t, err)
			sidecar := fileByRole(asset.Files, PhotoRoleSidecar)
			raw := fileByRole(asset.Files, PhotoRoleRAW)
			if mutation == "detached own file" {
				_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, raw.ID, PhotoDetachOptions{ClearDependentSidecars: true})
				require.NoError(t, err)
				after, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
				require.NoError(t, err)
				require.Equal(t, entries, after)
				for _, entry := range entries {
					if entry.FileID == raw.ID {
						err = s.PublishPhotoCheckoutEntry(ctx, entry, func() (bool, error) { t.Fatal("detached source installed"); return false, nil })
						require.ErrorIs(t, err, ErrPhotoCheckoutSourceUnavailable)
						return
					}
				}
				t.Fatal("frozen RAW entry missing")
			}
			var entry PhotoCheckoutEntry
			for _, e := range entries {
				if e.FileID == sidecar.ID {
					entry = e
				}
				if e.FileID == raw.ID && mutation != "unpublished" {
					require.NoError(t, s.PublishPhotoCheckoutEntry(ctx, e, func() (bool, error) { return true, nil }))
				}
			}
			require.Equal(t, new(raw.ID), entry.SidecarOfID)
			switch mutation {
			case "trashed":
				node, lookupErr := s.NodeByID(ctx, raw.NodeID)
				require.NoError(t, lookupErr)
				_, _, err = s.Trash(ctx, node.ID, node.Revision)
			case "retargeted":
				_, err = s.db.ExecContext(ctx, "UPDATE photo_files SET sidecar_of_file_id=? WHERE file_id=?", fileByRole(asset.Files, PhotoRoleImage).ID, sidecar.ID)
			}
			require.NoError(t, err)
			called := false
			err = s.PublishPhotoCheckoutEntry(ctx, entry, func() (bool, error) { called = true; return true, nil })
			require.ErrorIs(t, err, ErrPhotoCheckoutSourceUnavailable)
			require.False(t, called)
		})
	}
}

func TestPhotoCheckoutBackupRetentionAcrossLifecycleStates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, state                           string
		published, uncertain, legacy, expired bool
	}{
		{name: "queued", state: "queued"},
		{name: "completed", state: "completed", published: true}, {name: "failed published", state: "failed", published: true},
		{name: "legacy export", state: "failed", published: true, legacy: true},
		{name: "failed unpublished", state: "failed"},
		{name: "running uncertain", state: "running", uncertain: true},
		{name: "expired cancelled uncertain", state: "cancelled", uncertain: true, expired: true},
	}
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					s := newTestStoreWithDriver(t, driver.driver)
					ctx := t.Context()
					asset := albumAsset(t, s, "published.jpg")
					second := albumAsset(t, s, "unpublished.jpg")
					node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
					require.NoError(t, err)
					checkout, op, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{asset.ID, second.ID}}, "checkout", 0)
					require.NoError(t, err)
					require.NoError(t, s.ClaimPhotoCheckout(ctx, checkout.ID, op.ID))
					entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
					require.NoError(t, err)
					for _, entry := range entries {
						if entry.AssetID != asset.ID {
							continue
						}
						if tc.published {
							require.NoError(t, s.PublishPhotoCheckoutEntry(ctx, entry, func() (bool, error) { return true, nil }))
						}
						if tc.uncertain {
							require.NoError(t, s.AdvanceStorageOperation(ctx, op.ID, entry.FileID, 0, 0, 0, "{}"))
						}
					}
					retainUntil := time.Now().Add(time.Hour)
					if tc.expired {
						retainUntil = time.Now().Add(-time.Hour)
					}
					switch tc.state {
					case "queued":
						require.NoError(t, s.DeferStorageOperation(ctx, op.ID, errors.New("waiting")))
					case "completed", "failed", "cancelled":
						require.NoError(t, s.FinishPhotoCheckout(ctx, checkout.ID, op.ID, StorageOperationState(tc.state), "{}", "result", retainUntil))
					}
					updated, _, err := s.ReplaceContent(ctx, node.ID, node.Revision, browseHash("replacement"), 30, "image/jpeg")
					require.NoError(t, err)
					unpublished, err := s.NodeByID(ctx, second.Files[0].NodeID)
					require.NoError(t, err)
					unpublishedUpdated, _, err := s.ReplaceContent(ctx, unpublished.ID, unpublished.Revision, browseHash("unpublished replacement"), 30, "image/jpeg")
					require.NoError(t, err)
					unpublishedSelector := VersionPruneSelector{VersionIDs: []string{unpublished.CurrentVersionID}}
					prunedUnpublished := tc.state == "failed"
					if prunedUnpublished {
						result, err := s.PruneContentVersions(ctx, unpublished.ID, unpublishedUpdated.Revision, unpublishedSelector, true)
						require.NoError(t, err)
						require.Equal(t, 1, result.DeletedVersions)
					}
					if tc.expired {
						_, err = s.PruneExpiredStorageOperations(ctx, time.Now())
						require.NoError(t, err)
						_, err = s.StorageOperation(ctx, op.ID)
						require.NoError(t, err)
						held, err := s.PruneContentVersions(ctx, node.ID, updated.Revision, VersionPruneSelector{VersionIDs: []string{node.CurrentVersionID}}, true)
						require.NoError(t, err)
						require.Zero(t, held.DeletedVersions)
						require.Len(t, held.DependencyRetained, 1)
					}
					var exported bytes.Buffer
					require.NoError(t, s.ExportMetadata(ctx, &exported))
					if tc.legacy {
						require.Contains(t, exported.String(), `"base_held":true,`)
						legacy := strings.ReplaceAll(exported.String(), `"base_held":true,`, "")
						require.NotContains(t, legacy, `"base_held"`)
						exported.Reset()
						_, err := exported.WriteString(legacy)
						require.NoError(t, err)
					}
					restored := newTestStoreWithDriver(t, driver.driver)
					require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
					released := tc.legacy || !tc.published && !tc.uncertain
					if !prunedUnpublished && !tc.published {
						result, err := restored.PruneContentVersions(ctx, unpublished.ID, unpublishedUpdated.Revision, unpublishedSelector, true)
						require.NoError(t, err)
						require.Equal(t, 1, result.DeletedVersions)
					}
					selector := VersionPruneSelector{VersionIDs: []string{node.CurrentVersionID}}
					pruned, err := restored.PruneContentVersions(ctx, node.ID, updated.Revision, selector, true)
					require.NoError(t, err)
					if released {
						require.Equal(t, 1, pruned.DeletedVersions)
					} else {
						require.Zero(t, pruned.DeletedVersions)
						require.Len(t, pruned.DependencyRetained, 1)
						_, err = restored.RetirePhotoCheckout(ctx, checkout.ID)
						require.NoError(t, err)
						if tc.expired {
							_, err = s.RetirePhotoCheckout(ctx, checkout.ID)
							require.NoError(t, err)
							_, err = s.PruneExpiredStorageOperations(ctx, time.Now())
							require.NoError(t, err)
							_, err = s.StorageOperation(ctx, op.ID)
							require.ErrorIs(t, err, ErrNotFound)
						}
						pruned, err = restored.PruneContentVersions(ctx, node.ID, updated.Revision, selector, true)
						require.NoError(t, err)
						require.Equal(t, 1, pruned.DeletedVersions)
					}
				})
			}
		})
	}
}

func TestPhotoCheckoutPublicationOrdersEligibilityMutationsAndRecordsCancellation(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{"hide", "exclude", "photo trash", "node trash", "path trash"} {
		t.Run(mutation, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			asset := albumAsset(t, s, "photo.jpg")
			other := albumAsset(t, s, "other.jpg")
			require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
			checkout, _, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, "checkout", 0)
			require.NoError(t, err)
			entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
			require.NoError(t, err)
			folder, err := s.MkdirAll(ctx, "/photos-to-trash")
			require.NoError(t, err)
			if mutation == "path trash" {
				node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
				require.NoError(t, err)
				_, _, err = s.Move(ctx, node.ID, folder.ID, node.Name, node.Revision)
				require.NoError(t, err)
			}
			exercise := func(t *testing.T, ctx context.Context, waitSynchronously bool) {
				t.Helper()
				started, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				releaseInstall := func() { once.Do(func() { close(release) }) }
				defer releaseInstall()
				published := make(chan error, 1)
				go func() {
					published <- s.PublishPhotoCheckoutEntry(ctx, entries[0], func() (bool, error) { close(started); <-release; return true, nil })
				}()
				<-started
				changed := make(chan error, 1)
				attempting := make(chan struct{})
				go func() {
					close(attempting)
					var err error
					switch mutation {
					case "hide":
						_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
					case "exclude":
						_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
					case "photo trash":
						_, err = s.TrashPhotoAsset(ctx, asset.ID, asset.Revision)
					case "node trash":
						_, _, err = s.Trash(ctx, asset.Files[0].NodeID, UnconditionalRev)
					case "path trash":
						_, _, err = s.TrashPath(ctx, "/photos-to-trash")
					}
					changed <- err
				}()
				<-attempting
				if waitSynchronously {
					synctest.Wait()
					select {
					case err := <-changed:
						t.Fatalf("eligibility changed during installation: %v", err)
					default:
					}
				} else {
					select {
					case err := <-changed:
						t.Fatalf("eligibility changed during installation: %v", err)
					case <-time.After(50 * time.Millisecond): //nolint:kennlint // the tree RWMutex has no waiter signal
					}
				}
				_, err := s.SetPhotoAssetExcluded(ctx, other.ID, other.Revision, true)
				require.NoError(t, err)
				releaseInstall()
				require.NoError(t, <-published)
				require.NoError(t, <-changed)
				err = s.PublishPhotoCheckoutEntry(ctx, entries[0], func() (bool, error) { t.Fatal("ineligible source installed"); return false, nil })
				require.ErrorIs(t, err, ErrPhotoCheckoutSourceUnavailable)
			}
			if mutation == "node trash" || mutation == "path trash" {
				exercise(t, t.Context(), false)
			} else {
				synctest.Test(t, func(t *testing.T) { exercise(t, t.Context(), true) })
			}
		})
	}
	t.Run("cancelled installation stays published", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		asset := albumAsset(t, s, "photo.jpg")
		checkout, _, _, err := s.CreatePhotoCheckout(t.Context(), PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, "checkout", 0)
		require.NoError(t, err)
		entries, err := s.PhotoCheckoutEntries(t.Context(), checkout.ID)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		err = s.PublishPhotoCheckoutEntry(ctx, entries[0], func() (bool, error) { cancel(); return true, context.Canceled })
		require.ErrorIs(t, err, context.Canceled)
		status, err := s.PhotoCheckoutStatus(t.Context(), checkout.ID)
		require.NoError(t, err)
		require.Equal(t, int64(1), status.PublishedFiles)
	})
}

// TestPreserveUncertainPhotoCheckoutHoldsSurvivesClearedPublication covers
// recovery after a destination can no longer be confirmed: a reconcile that
// cannot verify a previously published entry still clears its published
// flag (ObservePhotoCheckout), but PreserveUncertainPhotoCheckoutHolds must
// run first so the source version stays held, matching the documented
// "published or uncertain copies retain their bases until retirement"
// guarantee.
func TestPreserveUncertainPhotoCheckoutHoldsSurvivesClearedPublication(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "photo.jpg")
	node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	checkout, op, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, "checkout", 0)
	require.NoError(t, err)
	require.NoError(t, s.ClaimPhotoCheckout(ctx, checkout.ID, op.ID))
	entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
	require.NoError(t, err)
	require.NoError(t, s.PublishPhotoCheckoutEntry(ctx, entries[0], func() (bool, error) { return true, nil }))

	require.NoError(t, s.PreserveUncertainPhotoCheckoutHolds(ctx, checkout.ID, op.ID))
	require.NoError(t, s.ObservePhotoCheckout(ctx, checkout.ID, op.ID, nil))
	require.NoError(t, s.FinishPhotoCheckout(ctx, checkout.ID, op.ID, StorageOperationFailed, "{}", "destination unreachable", time.Now().Add(time.Hour)))

	entries, err = s.PhotoCheckoutEntries(ctx, checkout.ID)
	require.NoError(t, err)
	require.False(t, entries[0].Published, "publication can no longer be confirmed")

	updated, _, err := s.ReplaceContent(ctx, node.ID, node.Revision, browseHash("replacement"), 30, "image/jpeg")
	require.NoError(t, err)
	selector := VersionPruneSelector{VersionIDs: []string{node.CurrentVersionID}}
	pruned, err := s.PruneContentVersions(ctx, node.ID, updated.Revision, selector, true)
	require.NoError(t, err)
	require.Zero(t, pruned.DeletedVersions, "an uncertain copy must keep its source base until retirement")
	require.Len(t, pruned.DependencyRetained, 1)

	_, err = s.RetirePhotoCheckout(ctx, checkout.ID)
	require.NoError(t, err)
	pruned, err = s.PruneContentVersions(ctx, node.ID, updated.Revision, selector, true)
	require.NoError(t, err)
	require.Equal(t, 1, pruned.DeletedVersions, "retirement releases the hold")
}

func TestPhotoCheckoutRetireAfterAuditActivation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "photo.jpg")
	checkout, operation, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, "checkout", 0)
	require.NoError(t, err)
	require.NoError(t, s.FinishPhotoCheckout(ctx, checkout.ID, operation.ID, StorageOperationCompleted, "{}", "", time.Time{}))
	seedInitialAuditAuthority(t, s, s.RootID())
	retired, err := s.RetirePhotoCheckout(ctx, checkout.ID)
	require.NoError(t, err)
	require.NotNil(t, retired.RetiredAt)
	require.True(t, retired.RetentionReleased)
	persisted, err := s.PhotoCheckout(ctx, checkout.ID)
	require.NoError(t, err)
	require.Equal(t, retired, persisted)
}

func TestPhotoCheckoutYearFolderUsesTimelineDay(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	node, err := s.IngestFileExact(ctx, run, s.RootID(), "photo.jpg", browseHash("mtime-photo"), 1, "image/jpeg", "photo.jpg", "2026-03-14T12:00:00Z")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	checkout, _, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{Years: &PhotoCheckoutYears{From: 2026, To: 2026}}, t.TempDir(), 0)
	require.NoError(t, err)
	entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "2026/03/14/"+asset.ID+"/photo.jpg", entries[0].Path)
}

func TestPhotoCheckoutSkipsDuplicateNamesInAsset(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	var assets []PhotoAsset
	for _, folder := range []string{"first", "second"} {
		dir, err := s.Mkdir(ctx, s.RootID(), folder)
		require.NoError(t, err)
		node, err := s.CreateFile(ctx, dir.ID, "photo.jpg", browseHash(folder), 10, "image/jpeg")
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(ctx, node.ID)
		require.NoError(t, err)
		assets = append(assets, asset)
	}
	first, second := assets[0], assets[1]
	_, err := s.DetachPhotoFile(ctx, second.ID, second.Revision, second.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	first, err = s.AttachPhotoFile(ctx, first.ID, first.Revision, second.Files[0].NodeID, PhotoRoleImage, nil)
	require.NoError(t, err)
	good := albumAsset(t, s, "good.jpg")
	selection := PhotoCheckoutSelection{All: true}
	estimate, err := s.EstimatePhotoCheckout(ctx, selection)
	require.NoError(t, err)
	require.Equal(t, int64(1), estimate.Assets)
	require.Equal(t, int64(1), estimate.Files)
	require.Equal(t, int64(20), estimate.Bytes)
	require.Equal(t, int64(1), estimate.SkippedPhotoCount)
	require.Len(t, estimate.SkippedPhotos, 1)
	require.Equal(t, first.ID, estimate.SkippedPhotos[0].AssetID)
	require.Contains(t, []string{first.Files[0].ID, first.Files[1].ID}, estimate.SkippedPhotos[0].FileID)
	require.Equal(t, "photo.jpg", estimate.SkippedPhotos[0].Name)
	require.Contains(t, estimate.SkippedPhotos[0].Reason, "has the same name")
	checkout, _, createdEstimate, err := s.CreatePhotoCheckout(ctx, selection, "checkout", 20)
	require.NoError(t, err)
	require.Equal(t, estimate, createdEstimate)
	entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, good.ID, entries[0].AssetID)

	selection = PhotoCheckoutSelection{AssetIDs: []string{first.ID}}
	_, err = s.EstimatePhotoCheckout(ctx, selection)
	require.ErrorContains(t, err, "selection contains no files")
	checkout, _, _, err = s.CreatePhotoCheckout(ctx, selection, "checkout", 0)
	require.ErrorContains(t, err, "selection contains no files")
	require.Empty(t, checkout.ID)
}

// Creation resolves the selection without the write lock. A base purged
// before the lock must not be frozen into a checkout.
func TestPhotoCheckoutCreationRejectsSelectionPurgedBeforeCommit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	kept := albumAsset(t, s, "kept.jpg")
	purged := albumAsset(t, s, "purged.jpg")
	selection := PhotoCheckoutSelection{AssetIDs: []string{kept.ID, purged.ID}}
	var entries []PhotoCheckoutEntry
	var estimate PhotoCheckoutEstimate
	require.NoError(t, s.photoReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		entries, estimate, err = s.checkoutEntriesTx(ctx, tx, selection)
		return err
	}))
	require.Len(t, entries, 2)
	node, err := s.NodeByID(ctx, purged.Files[0].NodeID)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, node.ID, node.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)

	//nolint:dogsled // Only the stale-selection error matters after rollback.
	_, _, _, err = s.insertPhotoCheckout(ctx, selection, "checkout", 0, func(*sql.Tx) ([]PhotoCheckoutEntry, PhotoCheckoutEstimate, error) {
		return entries, estimate, nil
	})
	require.ErrorIs(t, err, errPhotoCheckoutSelectionStale)
	checkouts, err := s.ListPhotoCheckouts(ctx)
	require.NoError(t, err)
	require.Empty(t, checkouts)

	checkout, _, _, err := s.CreatePhotoCheckout(ctx, selection, "checkout", 0)
	require.NoError(t, err)
	frozen, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
	require.NoError(t, err)
	require.Len(t, frozen, 1)
	require.Equal(t, kept.Files[0].ID, frozen[0].FileID)
}

func TestPhotoCheckoutSkipsDuplicateNamesWithBoundedDetails(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	good := albumAsset(t, s, "good.jpg")
	duplicateAssets := map[string][]string{}
	for i := range 25 {
		var assets []PhotoAsset
		for _, folder := range []string{"first", "second"} {
			name := fmt.Sprintf("%s-%d", folder, i)
			dir, err := s.Mkdir(ctx, s.RootID(), name)
			require.NoError(t, err)
			node, err := s.CreateFile(ctx, dir.ID, "photo.jpg", browseHash(name), 20, "image/jpeg")
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(ctx, node.ID)
			require.NoError(t, err)
			assets = append(assets, asset)
		}
		first, second := assets[0], assets[1]
		_, err := s.DetachPhotoFile(ctx, second.ID, second.Revision, second.Files[0].ID, PhotoDetachOptions{})
		require.NoError(t, err)
		first, err = s.AttachPhotoFile(ctx, first.ID, first.Revision, second.Files[0].NodeID, PhotoRoleImage, nil)
		require.NoError(t, err)
		duplicateAssets[first.ID] = []string{first.Files[0].ID, first.Files[1].ID}
	}
	selection := PhotoCheckoutSelection{All: true}
	estimate, err := s.EstimatePhotoCheckout(t.Context(), selection)
	require.NoError(t, err)
	require.Equal(t, int64(1), estimate.Assets)
	require.Equal(t, int64(1), estimate.Files)
	require.Equal(t, int64(20), estimate.Bytes)
	require.Equal(t, int64(25), estimate.SkippedPhotoCount)
	require.Len(t, estimate.SkippedPhotos, 20)
	for _, skip := range estimate.SkippedPhotos {
		require.Contains(t, duplicateAssets[skip.AssetID], skip.FileID)
		require.Equal(t, "photo.jpg", skip.Name)
		require.Contains(t, skip.Reason, "has the same name")
	}
	checkout, _, createdEstimate, err := s.CreatePhotoCheckout(t.Context(), selection, "checkout", 20)
	require.NoError(t, err)
	require.Equal(t, estimate, createdEstimate)
	entries, err := s.PhotoCheckoutEntries(t.Context(), checkout.ID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, good.ID, entries[0].AssetID)
}
