package store

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
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

func TestPhotoCheckoutPublicationRechecksVisibility(t *testing.T) {
	t.Parallel()
	for _, column := range []string{"hidden_at", "excluded_at", "trashed"} {
		t.Run(column, func(t *testing.T) {
			s := newTestStore(t)
			a := albumAsset(t, s, "photo.jpg")
			c, _, err := s.CreatePhotoCheckout(t.Context(), PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, "checkout", 0)
			require.NoError(t, err)
			entries, err := s.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			if column == "trashed" {
				_, err = s.TrashPhotoAsset(t.Context(), a.ID, a.Revision)
			} else {
				_, err = s.db.ExecContext(t.Context(), `UPDATE photo_assets SET `+column+`=? WHERE asset_id=?`, nowRFC3339(), a.ID)
			}
			require.NoError(t, err)
			called := false
			err = s.PublishPhotoCheckoutEntry(t.Context(), entries[0], func() (bool, error) { called = true; return true, nil })
			require.ErrorIs(t, err, ErrInvalidPhotoCheckout)
			require.False(t, called)
		})
	}
}

func TestPhotoCheckoutPublicationReservesWriterWithoutTransaction(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	a := albumAsset(t, s, "photo.jpg")
	c, _, err := s.CreatePhotoCheckout(t.Context(), PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, "checkout", 0)
	require.NoError(t, err)
	entries, err := s.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	mutation := make(chan error, 1)
	require.NoError(t, s.PublishPhotoCheckoutEntry(ctx, entries[0], func() (bool, error) {
		// An independent connection can write because publication holds no SQLite transaction.
		_, err := s.db.ExecContext(ctx, `UPDATE storage_operations SET error='' WHERE operation_id=?`, c.OperationID)
		if err != nil {
			return false, err
		}
		waits := s.writeDB.Stats().WaitCount
		go func() {
			_, err := s.SetPhotoAssetExcluded(ctx, a.ID, a.Revision, true)
			mutation <- err
		}()
		require.Eventually(t, func() bool { return s.writeDB.Stats().WaitCount > waits }, time.Second, time.Millisecond)
		return true, nil
	}))
	require.NoError(t, <-mutation)
	entries, err = s.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.True(t, entries[0].Published)
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
				name   string
				mutate func(*metadataPhotoCheckoutEntry)
				path   bool
			}{
				{"unsafe-path", func(entry *metadataPhotoCheckoutEntry) { entry.Path = "../photo.jpg" }, true},
				{"size", func(entry *metadataPhotoCheckoutEntry) { entry.Size++ }, false},
				{"file", func(entry *metadataPhotoCheckoutEntry) { entry.FileID = second.Files[0].ID }, false},
				{"asset", func(entry *metadataPhotoCheckoutEntry) { entry.AssetID = "00000000-0000-4000-8000-000000000001" }, false},
			} {
				lines := bytes.Split(exported.Bytes(), []byte{'\n'})
				changed := false
				for i, line := range lines {
					if len(line) == 0 {
						continue
					}
					var entry metadataPhotoCheckoutEntry
					require.NoError(t, json.Unmarshal(line, &entry))
					if entry.Type == "photo_checkout_entry" {
						bad.mutate(&entry)
						lines[i], err = json.Marshal(entry)
						require.NoError(t, err)
						changed = true
					}
				}
				require.True(t, changed, bad.name)
				invalid, err := Open(filepath.Join(t.TempDir(), bad.name+".db"), driver.driver)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, invalid.Close()) })
				err = invalid.ImportMetadata(t.Context(), bytes.NewReader(bytes.Join(lines, []byte{'\n'})))
				if bad.path {
					require.ErrorIs(t, err, ErrInvalidPhotoCheckout)
				} else {
					require.ErrorContains(t, err, "photo checkout frozen bases do not match content versions")
				}
			}
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
	cases := []struct {
		name, filename string
		limit          int64
	}{
		{"empty all", "", 100}, {"audit refusal", "", 100},
		{"negative limit", "", -1}, {"all without limit", "", 0}, {"insufficient limit", "", 19},
	}
	if runtime.GOOS == "windows" {
		for _, name := range []string{"CON.jpg", "bad:name.jpg", "trailing.", "trailing ", "control\x01.jpg"} {
			cases = append(cases, struct {
				name, filename string
				limit          int64
			}{name, name, 100})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			selection := PhotoCheckoutSelection{All: true}
			expectedErr := ErrInvalidPhotoCheckout
			var asset PhotoAsset
			if tc.name != "empty all" {
				asset = albumAsset(t, s, "photo.jpg")
				if tc.filename != "" || tc.name == "audit refusal" || tc.name == "negative limit" {
					selection = PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}
				}
				if tc.filename != "" {
					_, err := s.db.ExecContext(t.Context(), "UPDATE nodes SET name=? WHERE id=?", tc.filename, asset.Files[0].NodeID)
					require.NoError(t, err)
				} else if tc.name == "audit refusal" {
					seedInitialAuditAuthority(t, s, s.RootID())
					expectedErr = ErrAuditMutationUnsupported
				}
			}
			_, _, err := s.CreatePhotoCheckout(t.Context(), selection, "checkout", tc.limit)
			require.ErrorIs(t, err, expectedErr)
			if tc.name == "negative limit" {
				require.ErrorContains(t, err, "byte_limit must be nonnegative")
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

func TestPhotoCheckoutSourceNameSelection(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	result, source := seedGroupedCheckoutSources(t, s)
	raw := fileByRole(result.Asset.Files, PhotoRoleRAW)
	node, err := s.NodeByID(ctx, raw.NodeID)
	require.NoError(t, err)
	_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent", SourceDescription: "synthetic", OriginalPath: filepath.Join(source, "ignored.ARW")})
	require.NoError(t, err)
	retained, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{result.Asset.ID}}, "retained", 0)
	require.NoError(t, err)
	entries, err := s.PhotoCheckoutEntries(ctx, retained.ID)
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

func TestPhotoSourceMetadataRestoreAndMissingIdentityBackfill(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	result, _ := seedGroupedCheckoutSources(t, s)
	raw := fileByRole(result.Asset.Files, PhotoRoleRAW)
	var backup bytes.Buffer
	err := s.ExportMetadata(ctx, &backup)
	require.NoError(t, err)
	sources := make(map[string]string)
	for _, file := range result.Asset.Files {
		var selectedSource string
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT source_provenance_identity FROM photo_files WHERE file_id=?`, file.ID).Scan(&selectedSource))
		sources[file.ID] = selectedSource
	}
	rawSource := sources[raw.ID]
	var alternateSource string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT identity FROM provenance WHERE node_id=? AND original_path LIKE '%AAA.ARW'`, raw.NodeID).Scan(&alternateSource))
	for _, variant := range []string{"current", "missing source_provenance_identity", "another node"} {
		expectedSource := rawSource
		if variant == "current" {
			expectedSource = alternateSource
		}
		lines := bytes.Split(backup.Bytes(), []byte{'\n'})
		for i, line := range lines {
			if !bytes.Contains(line, []byte(`"type":"photo_file"`)) {
				continue
			}
			var file metadataPhotoFile
			require.NoError(t, json.Unmarshal(line, &file))
			if variant == "missing source_provenance_identity" {
				file.SourceProvenanceIdentity = nil
			} else if variant == "current" && file.FileID == raw.ID {
				file.SourceProvenanceIdentity = &alternateSource
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
		for _, file := range result.Asset.Files {
			if variant != "missing source_provenance_identity" && file.ID != raw.ID {
				continue
			}
			want := expectedSource
			if variant == "missing source_provenance_identity" {
				want = sources[file.ID]
			}
			var storedSource string
			require.NoError(t, restored.db.QueryRowContext(ctx, `SELECT source_provenance_identity FROM photo_files WHERE file_id=?`, file.ID).Scan(&storedSource))
			require.Equal(t, want, storedSource, "%s: %s", variant, file.ID)
		}
	}
}

func TestPhotoCheckoutResolvesCompanionSources(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"separate ingests", "conflicting pair", "trashed target", "trashed conflicting sidecar", "multiple sidecars"} {
		t.Run(scenario, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			source := t.TempDir()
			run, err := s.BeginIngest(ctx, "photo-import", source)
			require.NoError(t, err)
			raw := photoImportTestMember(filepath.Join(source, "IMG.ARW"), PhotoRoleRAW, browseHash("raw"), "image/x-sony-arw")
			members := []PhotoImportMember{raw, photoImportTestMember(filepath.Join(source, "IMG.JPG"), PhotoRoleImage, browseHash("jpeg"), "image/jpeg")}
			if scenario == "multiple sidecars" {
				members = append(members, photoImportTestMember(filepath.Join(source, "IMG.XMP"), PhotoRoleSidecar, browseHash("first sidecar"), "application/rdf+xml"))
			}
			result, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(members...))
			require.NoError(t, err)
			name := "IMG.ARW"
			if scenario == "multiple sidecars" {
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
				sidecarName := name + ".XMP"
				if scenario == "conflicting pair" || scenario == "trashed conflicting sidecar" {
					sidecarName = "unrelated.XMP"
				}
				node, err := s.IngestFileExact(ctx, run, s.RootID(), sidecarName, browseHash("sidecar"), 20, "application/rdf+xml", filepath.Join(source, sidecarName), "")
				require.NoError(t, err)
				target := fileByRole(result.Asset.Files, PhotoRoleRAW).ID
				result.Asset, err = s.AttachPhotoFile(ctx, result.Asset.ID, result.Asset.Revision, node.ID, PhotoRoleSidecar, &target)
				require.NoError(t, err)
				for i, file := range result.Asset.Files {
					node, err := s.NodeByID(ctx, file.NodeID)
					require.NoError(t, err)
					node, _, err = s.Move(ctx, node.ID, s.RootID(), fmt.Sprintf("virtual-%d.dat", i), node.Revision)
					require.NoError(t, err)
					if scenario == "trashed target" && file.Role == PhotoRoleRAW || scenario == "trashed conflicting sidecar" && file.Role == PhotoRoleSidecar {
						_, _, err = s.Trash(ctx, node.ID, node.Revision)
						require.NoError(t, err)
					}
				}
				if scenario == "trashed target" {
					jpeg := fileByRole(result.Asset.Files, PhotoRoleImage).ID
					_, err = s.SetPhotoDisplay(ctx, result.Asset.ID, result.Asset.Revision, &jpeg)
					require.NoError(t, err)
				}
			}
			checkout, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{result.Asset.ID}}, "checkout", 0)
			if scenario == "conflicting pair" {
				require.ErrorIs(t, err, ErrInvalidPhotoCheckout)
				require.Contains(t, err.Error(), result.Asset.ID)
				require.Contains(t, err.Error(), fileByRole(result.Asset.Files, PhotoRoleSidecar).ID)
				checkouts, err := s.ListPhotoCheckouts(ctx)
				require.NoError(t, err)
				require.Empty(t, checkouts)
				return
			}
			require.NoError(t, err)
			entries, err := s.PhotoCheckoutEntries(ctx, checkout.ID)
			require.NoError(t, err)
			expected := []string{"IMG.JPG"}
			if scenario != "trashed conflicting sidecar" {
				expected = append(expected, name+".XMP")
			}
			if scenario != "trashed target" {
				expected = append(expected, name)
			}
			if scenario == "multiple sidecars" {
				expected = []string{"RENAMED.ARW", "IMG.JPG", "RENAMED.XMP", "RENAMED.ARW.XMP"}
			}
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				names = append(names, filepath.Base(entry.Path))
			}
			require.ElementsMatch(t, expected, names)
		})
	}
}

func TestPhotoCheckoutSourcePathBasenames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ sourcePath, basename string }{
		{`C:\camera\capture.JPG`, "capture.JPG"},
		{`\\camera\photos\capture.JPG`, "capture.JPG"},
		{`/camera/foo\bar.JPG`, `foo\bar.JPG`},
		{`/camera/Trip: Day 1.jpg`, "Trip: Day 1.jpg"},
	} {
		if runtime.GOOS == "windows" && tc.basename == "Trip: Day 1.jpg" {
			continue
		}
		sourcePath := tc.sourcePath
		t.Run(sourcePath, func(t *testing.T) {
			s := newTestStore(t)
			run, err := s.BeginIngest(t.Context(), "photo-import", t.TempDir())
			require.NoError(t, err)
			member := photoImportTestMember(sourcePath, PhotoRoleImage, browseHash("capture"), "image/jpeg")
			member.Name = "renamed.JPG"
			result, err := s.IngestPhotoGroup(t.Context(), run, photoImportTestGroup(member))
			require.NoError(t, err)
			checkout, _, err := s.CreatePhotoCheckout(t.Context(), PhotoCheckoutSelection{AssetIDs: []string{result.Asset.ID}}, "checkout", 0)
			if runtime.GOOS == "windows" && strings.HasPrefix(sourcePath, "/camera/") {
				require.ErrorIs(t, err, ErrInvalidPhotoCheckout)
				return
			}
			require.NoError(t, err)
			entries, err := s.PhotoCheckoutEntries(t.Context(), checkout.ID)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, "undated/"+result.Asset.ID+"/"+tc.basename, entries[0].Path)
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
