package store

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
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
		checkout, _, err := s.CreatePhotoCheckout(ctx, test.selection, t.TempDir(), test.bytes)
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
		checkout, _, err := s.CreatePhotoCheckout(ctx, selection, t.TempDir(), estimate.Bytes)
		require.NoError(t, err)
		entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
		require.NoError(t, err)
		var selectedFiles []string
		for _, entry := range entries {
			selectedFiles = append(selectedFiles, entry.FileID)
		}
		require.ElementsMatch(t, eligibleFiles, selectedFiles)
	}
	for _, bad := range []PhotoCheckoutSelection{{}, {AssetIDs: []string{"bad"}}, {AlbumIDs: []string{"bad"}}, {Years: &PhotoCheckoutYears{From: 0, To: 2024}}, {Years: &PhotoCheckoutYears{From: 2025, To: 2024}}, {Years: &PhotoCheckoutYears{From: 2024, To: 10000}}} {
		_, err := s.EstimatePhotoCheckout(ctx, bad)
		require.ErrorIs(t, err, ErrInvalidPhotoCheckout)
	}
	_, err = s.EstimatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{"00000000-0000-4000-8000-000000000001"}})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.CreateFile(ctx, s.RootID(), "large.jpg", browseHash("large"), math.MaxInt64, "image/jpeg")
	require.NoError(t, err)
	_, err = s.EstimatePhotoCheckout(ctx, PhotoCheckoutSelection{All: true})
	require.ErrorIs(t, err, ErrInvalidPhotoCheckout)
}

func TestPhotoCheckoutRefusesUnavailableSources(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"hidden_at", "excluded_at", "trashed", "detach"} {
		t.Run(scenario, func(t *testing.T) {
			s := newTestStore(t)
			a := albumAsset(t, s, "photo.jpg")
			c, _, err := s.CreatePhotoCheckout(t.Context(), PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, "checkout", 0)
			require.NoError(t, err)
			entries, err := s.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			switch scenario {
			case "detach":
				_, err = s.DetachPhotoFile(t.Context(), a.ID, a.Revision, a.Files[0].ID, PhotoDetachOptions{})
			case "trashed":
				_, err = s.TrashPhotoAsset(t.Context(), a.ID, a.Revision)
			case "excluded_at":
				_, err = s.SetPhotoAssetExcluded(t.Context(), a.ID, a.Revision, true)
			case "hidden_at":
				require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
				_, err = s.SetPhotoAssetHidden(t.Context(), a.ID, a.Revision, true)
			}
			require.NoError(t, err)
			if scenario == "detach" {
				after, err := s.PhotoCheckoutEntries(t.Context(), c.ID)
				require.NoError(t, err)
				require.Equal(t, entries, after)
			}
			called := false
			err = s.PublishPhotoCheckoutEntry(t.Context(), entries[0], func() (bool, error) { called = true; return true, nil })
			require.ErrorIs(t, err, ErrPhotoCheckoutSourceUnavailable)
			require.False(t, called)
		})
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
		c, _, err := s.CreatePhotoCheckout(t.Context(), PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, "machine-local-checkout", 100)
		require.NoError(t, err)
		entries, err := s.PhotoCheckoutEntries(t.Context(), c.ID)
		require.NoError(t, err)
		require.NoError(t, s.PublishPhotoCheckoutEntry(t.Context(), entries[0], func() (bool, error) { return true, nil }))
		require.NoError(t, s.FinishPhotoCheckout(t.Context(), c.ID, c.OperationID, StorageOperationFailed, "{}", "machine-local-error", time.Now().Add(time.Hour)))
		var exported bytes.Buffer
		require.NoError(t, s.ExportMetadata(t.Context(), &exported))
		return fixture{a, second, c, entries, exported.Bytes()}
	}
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "source.db"), driver.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			f := makeFixture(t, s)
			c, entries := f.checkout, f.entries
			require.NotContains(t, string(f.snapshot), "machine-local-")
			restored, err := Open(filepath.Join(t.TempDir(), "restored.db"), driver.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, restored.Close()) })
			require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(f.snapshot)))
			actual, err := restored.PhotoCheckout(t.Context(), c.ID)
			require.NoError(t, err)
			require.Equal(t, c.Selection, actual.Selection)
			require.Equal(t, c.ByteLimit, actual.ByteLimit)
			require.Equal(t, "unbound", actual.State)
			require.Empty(t, actual.Destination)
			require.Empty(t, actual.OperationID)
			require.Empty(t, actual.Error)
			actualEntries, err := restored.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			require.Equal(t, entries, actualEntries)
			var roundtrip bytes.Buffer
			require.NoError(t, restored.ExportMetadata(t.Context(), &roundtrip))
			require.Equal(t, string(f.snapshot), roundtrip.String())
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
		name, filename                             string
		limit                                      int64
		withAsset, explicitSelection, auditRefusal bool
		errorText                                  string
	}
	cases := []rejectedCase{
		{name: "empty all", limit: 100, errorText: "selection contains no files"},
		{name: "audit refusal", limit: 100, withAsset: true, explicitSelection: true, auditRefusal: true},
		{name: "negative limit", limit: -1, withAsset: true, explicitSelection: true, errorText: "byte_limit must be nonnegative"},
		{name: "all without limit", withAsset: true, errorText: "all requires a positive byte_limit"},
		{name: "insufficient limit", limit: 19, withAsset: true, errorText: "selected bytes exceed byte_limit"},
	}
	if runtime.GOOS == "windows" {
		for _, name := range []string{"CON.jpg", "bad:name.jpg", "trailing.", "trailing ", "control\x01.jpg"} {
			cases = append(cases, rejectedCase{name: name, filename: name, limit: 100, withAsset: true, explicitSelection: true})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			selection := PhotoCheckoutSelection{All: true}
			expectedErr := ErrInvalidPhotoCheckout
			var asset PhotoAsset
			if tc.withAsset {
				asset = albumAsset(t, s, "photo.jpg")
				if tc.explicitSelection {
					selection = PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}
				}
				if tc.filename != "" {
					_, err := s.db.ExecContext(t.Context(), "UPDATE nodes SET name=? WHERE id=?", tc.filename, asset.Files[0].NodeID)
					require.NoError(t, err)
				} else if tc.auditRefusal {
					seedInitialAuditAuthority(t, s, s.RootID())
					expectedErr = ErrAuditMutationUnsupported
				}
			}
			_, _, err := s.CreatePhotoCheckout(t.Context(), selection, "checkout", tc.limit)
			require.ErrorIs(t, err, expectedErr)
			if tc.errorText != "" {
				require.ErrorContains(t, err, tc.errorText)
			}
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
	albumAsset(t, s, "photo.jpg")
	checkout, op, err := s.CreatePhotoCheckout(t.Context(), PhotoCheckoutSelection{All: true}, "checkout", 20)
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
	require.NoError(t, s.FinishStorageOperation(t.Context(), op.ID, StorageOperationFailed, "{}", "terminal error", time.Now().Add(time.Hour)))
	finished, err := s.PhotoCheckout(t.Context(), checkout.ID)
	require.NoError(t, err)
	require.Equal(t, string(StorageOperationFailed), finished.State)
	require.Equal(t, "terminal error", finished.Error)
}

func TestPhotoCheckoutEntryPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, sidecar, trashRole, captureDate string
		multipleSidecars                      bool
		paths                                 map[string]string
	}{
		{name: "separate ingests", sidecar: "IMG.ARW.XMP", captureDate: "2024-06-15", paths: map[string]string{"raw": "2024/06/15/%s/IMG.ARW", "jpeg": "2024/06/15/%s/IMG.JPG", "sidecar": "2024/06/15/%s/IMG.ARW.XMP"}},
		{name: "trashed target", sidecar: "IMG.ARW.XMP", trashRole: PhotoRoleRAW, paths: map[string]string{"jpeg": "undated/%s/IMG.JPG", "sidecar": "undated/%s/IMG.ARW.XMP"}},
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
				if tc.trashRole == PhotoRoleSidecar {
					_, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{result.Asset.ID}}, "checkout", 0)
					require.ErrorIs(t, err, ErrInvalidPhotoCheckout)
					require.ErrorContains(t, err, result.Asset.ID)
					require.ErrorContains(t, err, fileByRole(result.Asset.Files, PhotoRoleSidecar).ID)
					checkouts, err := s.ListPhotoCheckouts(ctx)
					require.NoError(t, err)
					require.Empty(t, checkouts)
					operations, err := s.StorageOperations(ctx, 10)
					require.NoError(t, err)
					require.Empty(t, operations)
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
			checkout, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{result.Asset.ID}}, "checkout", 0)
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
		checkout, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{result.Asset.ID}}, "checkout", 0)
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
			checkout, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, "checkout", 0)
			require.NoError(t, err)
			entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, "undated/"+asset.ID+"/"+tc.basename, entries[0].Path)
		})
	}
}
