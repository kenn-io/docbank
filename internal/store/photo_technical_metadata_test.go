package store

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/geo"
)

func photoMetadataField(key, namespace, source string, value document.SourceMetadataValueV1) document.SourceMetadataFieldV1 {
	return document.SourceMetadataFieldV1{Key: key, Namespace: namespace, SourceField: source, Value: value}
}

func photoString(value string) document.SourceMetadataValueV1 {
	return document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: &value}
}

func photoInteger(value int64) document.SourceMetadataValueV1 {
	return document.SourceMetadataValueV1{Kind: document.SourceMetadataInteger, Integer: &value}
}

func photoNumber(value float64) document.SourceMetadataValueV1 {
	return document.SourceMetadataValueV1{Kind: document.SourceMetadataNumber, Number: &value}
}

func photoBoolean(value bool) document.SourceMetadataValueV1 {
	return document.SourceMetadataValueV1{Kind: document.SourceMetadataBoolean, Boolean: &value}
}

func photoTimestamp(raw, normalized string, precision document.SourceMetadataTimestampPrecision,
	timezone document.SourceMetadataTimezoneKind, offset string) document.SourceMetadataValueV1 {
	return document.SourceMetadataValueV1{Kind: document.SourceMetadataTimestamp,
		Timestamp: &document.SourceMetadataTimestampV1{Raw: raw, Normalized: normalized,
			Precision: precision, Timezone: timezone, Offset: offset}}
}

func photoCanonical(t *testing.T, fields ...document.SourceMetadataFieldV1) []byte {
	t.Helper()
	canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{
		ContractVersion: document.SourceMetadataContractV1, Fields: fields,
	})
	require.NoError(t, err)
	return canonical
}

func TestPhotoTechnicalMetadataFields(t *testing.T) {
	t.Parallel()
	metadata := document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{
		photoMetadataField("image.exif.camera_make", "image.exif", "Make", photoString("Synthetic Camera")),
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Model 1")),
		photoMetadataField("image.exif.lens_make", "image.exif", "LensMake", photoString("Synthetic Lens")),
		photoMetadataField("image.exif.lens_model", "image.exif", "LensModel", photoString("Lens 1")),
		photoMetadataField("image.exif.iso", "image.exif", "PhotographicSensitivity", photoInteger(400)),
		photoMetadataField("media.container.width_px", "media.container", "RAFImageWidth", photoInteger(640)),
		photoMetadataField("image.exif.pixel_width", "image.exif", "PixelXDimension", photoInteger(320)),
		photoMetadataField("media.container.height_px", "media.container", "Height", photoInteger(480)),
		photoMetadataField("image.exif.exposure_time_seconds", "image.exif", "ExposureTime", photoNumber(0.01)),
		photoMetadataField("image.exif.f_number", "image.exif", "FNumber", photoNumber(2.8)),
		photoMetadataField("image.exif.exposure_bias_ev", "image.exif", "ExposureBiasValue", photoNumber(-1.5)),
		photoMetadataField("image.exif.focal_length_mm", "image.exif", "FocalLength", photoNumber(50)),
		photoMetadataField("image.exif.orientation", "image.exif", "Orientation", photoInteger(6)),
		photoMetadataField("created", "image.exif", "DateTimeOriginal",
			photoTimestamp("2024:05:06 12:34:56-07:00", "2024-05-06T12:34:56-07:00",
				document.SourceMetadataPrecisionSecond, document.SourceMetadataTimezoneOffset, "-07:00")),
		photoMetadataField("title", "xmp", "Title", photoString("ignored")),
	}}
	canonical, _, err := document.MarshalSourceMetadataV1(metadata)
	require.NoError(t, err)
	decoded, _, err := document.DecodeSourceMetadataV1(canonical)
	require.NoError(t, err)
	fields := projectPhotoTechnicalMetadata(decoded, nil)
	require.NotNil(t, fields.CameraMake)
	assert.Equal(t, "Synthetic Camera", *fields.CameraMake)
	assert.Equal(t, "Synthetic Lens", *fields.LensMake)
	assert.Equal(t, int64(400), *fields.ISO)
	assert.InDelta(t, 0.01, *fields.ExposureTimeSeconds, 0.000001)
	assert.InDelta(t, 2.8, *fields.FNumber, 0.000001)
	assert.InDelta(t, -1.5, *fields.ExposureBiasEV, 0.000001)
	assert.InDelta(t, 50, *fields.FocalLengthMM, 0.000001)
	assert.Equal(t, int64(640), *fields.WidthPX, "container dimensions take precedence")
	assert.Equal(t, int64(480), *fields.HeightPX)
	assert.Equal(t, "2024-05-06T12:34:56-07:00", *fields.CaptureTime)
	assert.Equal(t, "-07:00", *fields.CaptureTimeOffset)
	assert.Equal(t, int64(6), *fields.Orientation)
	assert.Nil(t, fields.LocationLabel)
}

func TestPhotoTechnicalMetadataIgnoresBooleanCameraKey(t *testing.T) {
	t.Parallel()
	canonical := photoCanonical(t,
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoBoolean(true)),
		photoMetadataField("image.exif.lens_model", "image.exif", "LensModel", photoString("Lens 1")),
	)
	metadata, _, err := document.DecodeSourceMetadataV1(canonical)
	require.NoError(t, err)
	fields := projectPhotoTechnicalMetadata(metadata, nil)
	assert.Nil(t, fields.CameraModel)
	require.NotNil(t, fields.LensModel)
	assert.Equal(t, "Lens 1", *fields.LensModel)
}

func TestPhotoTechnicalMetadataContainerAliases(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, widthSource, heightSource string
	}{
		{name: "RAF", widthSource: "RAFImageWidth", heightSource: "RAFImageLength"},
		{name: "preview", widthSource: "ImageWidth", heightSource: "ImageLength"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fields := projectPhotoTechnicalMetadata(document.SourceMetadataV1{
				ContractVersion: document.SourceMetadataContractV1,
				Fields: []document.SourceMetadataFieldV1{
					photoMetadataField("media.container.width_px", "media.container", test.widthSource, photoInteger(6016)),
					photoMetadataField("media.container.height_px", "media.container", test.heightSource, photoInteger(4016)),
				},
			}, nil)
			require.NotNil(t, fields.WidthPX)
			require.NotNil(t, fields.HeightPX)
			assert.Equal(t, int64(6016), *fields.WidthPX)
			assert.Equal(t, int64(4016), *fields.HeightPX)
		})
	}
}

func TestPhotoTechnicalMetadataTimestampIdentity(t *testing.T) {
	t.Parallel()
	fields := projectPhotoTechnicalMetadata(document.SourceMetadataV1{
		ContractVersion: document.SourceMetadataContractV1,
		Fields: []document.SourceMetadataFieldV1{photoMetadataField("created", "image.exif", "DateTimeOriginal",
			photoTimestamp("20240506", "2024-05-06", document.SourceMetadataPrecisionDate,
				document.SourceMetadataTimezoneOmitted, ""))},
	}, nil)
	require.NotNil(t, fields.CaptureTime)
	assert.Equal(t, "2024-05-06", *fields.CaptureTime)
	assert.Equal(t, "20240506", *fields.CaptureTimeRaw)
	assert.Equal(t, string(document.SourceMetadataPrecisionDate), *fields.CaptureTimePrecision)
	assert.Equal(t, string(document.SourceMetadataTimezoneOmitted), *fields.CaptureTimeTimezone)
	assert.Empty(t, *fields.CaptureTimeOffset)

	containerFields := projectPhotoTechnicalMetadata(document.SourceMetadataV1{
		ContractVersion: document.SourceMetadataContractV1,
		Fields: []document.SourceMetadataFieldV1{photoMetadataField("created", "media.container", "mvhd.CreationTime",
			photoTimestamp("2024-05-06T12:34:56-07:00", "2024-05-06T12:34:56-07:00",
				document.SourceMetadataPrecisionSecond, document.SourceMetadataTimezoneOffset, "-07:00"))},
	}, nil)
	require.NotNil(t, containerFields.CaptureTime)
	assert.Equal(t, "2024-05-06T12:34:56-07:00", *containerFields.CaptureTime)
	assert.Equal(t, "-07:00", *containerFields.CaptureTimeOffset)
}

func TestPhotoTechnicalMetadataGPSStrings(t *testing.T) {
	t.Parallel()
	valid := document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{
		photoMetadataField("image.exif.gps_latitude", "image.exif", "GPSLatitude", photoString("48.8566000")),
		photoMetadataField("image.exif.gps_longitude", "image.exif", "GPSLongitude", photoString("2.3522000")),
	}}
	fields := projectPhotoTechnicalMetadata(valid, nil)
	require.NotNil(t, fields.Latitude)
	require.NotNil(t, fields.Longitude)
	assert.InDelta(t, 48.8566, *fields.Latitude, 0.0000001)
	assert.InDelta(t, 2.3522, *fields.Longitude, 0.0000001)
	invalid := valid
	invalid.Fields[1].Value.String = new("NaN")
	invalidFields := projectPhotoTechnicalMetadata(invalid, nil)
	assert.Nil(t, invalidFields.Latitude)
	assert.Nil(t, invalidFields.Longitude)
	ocean := valid
	ocean.Fields[0].Value.String = new("0")
	ocean.Fields[1].Value.String = new("-30")
	places, err := photoNaturalEarth()
	require.NoError(t, err)
	oceanFields := projectPhotoTechnicalMetadata(ocean, places)
	require.NotNil(t, oceanFields.Latitude)
	require.NotNil(t, oceanFields.Longitude)
	assert.Nil(t, oceanFields.LocationLabel, "valid open-ocean GPS keeps coordinates without a fabricated label")
}

func TestPhotoTechnicalMetadataPublication(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	sourceSHA256 := fakeHash("a5")
	_, err := s.CreateFile(ctx, s.RootID(), "atomic.jpg", sourceSHA256, 1, "image/jpeg")
	require.NoError(t, err)
	canonical := photoCanonical(t,
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Atomic")),
	)
	_, err = s.db.ExecContext(ctx, `CREATE TRIGGER photo_technical_metadata_test_failure
		BEFORE INSERT ON photo_technical_metadata BEGIN SELECT RAISE(ABORT, 'test projection failure'); END`)
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(ctx, sourceSHA256, fakeHash("f5"), canonical)
	require.ErrorContains(t, err, "test projection failure")
	var generations int
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM source_metadata_generations WHERE source_sha256=?`, sourceSHA256).Scan(&generations))
	assert.Zero(t, generations, "projection failure must roll back source publication")
}

func TestPhotoTechnicalMetadataRepublication(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "republication.jpg", fakeHash("ac"), 1, "image/jpeg")
	require.NoError(t, err)
	canonicalA := photoCanonical(t,
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("A")),
	)
	canonicalB := photoCanonical(t,
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("B")),
	)
	generationA, err := s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("fc"), canonicalA)
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("fd"), canonicalB)
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("fc"), canonicalA)
	require.NoError(t, err)
	projection, err := s.ContentVersionPhotoMetadata(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	assert.Equal(t, generationA.GenerationID, projection.GenerationID)
	assert.Equal(t, "A", *projection.Fields.CameraModel)
	_, err = s.db.ExecContext(ctx, `DELETE FROM photo_technical_metadata WHERE generation_id=?`, generationA.GenerationID)
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("fc"), canonicalA)
	require.NoError(t, err)
	projection, err = s.ContentVersionPhotoMetadata(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	assert.Equal(t, generationA.GenerationID, projection.GenerationID)
}

func TestPhotoTechnicalMetadataVersionBinding(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "versioned.jpg", fakeHash("a6"), 1, "image/jpeg")
	require.NoError(t, err)
	canonicalA := photoCanonical(t,
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("A")),
	)
	canonicalB := photoCanonical(t,
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("B")),
	)
	generationA, err := s.PublishSourceMetadata(ctx, first.BlobHash, fakeHash("f6"), canonicalA)
	require.NoError(t, err)
	replaced, second, err := s.ReplaceContent(ctx, first.ID, first.Revision, fakeHash("b6"), 1, "image/jpeg")
	require.NoError(t, err)
	generationB, err := s.PublishSourceMetadata(ctx, second.BlobHash, fakeHash("f7"), canonicalB)
	require.NoError(t, err)
	reverted, revert, _, err := s.RevertContent(ctx, replaced.ID, replaced.Revision, first.CurrentVersionID)
	require.NoError(t, err)
	late, err := s.CreateFile(ctx, s.RootID(), "late-copy.jpg", first.BlobHash, 1, "image/jpeg")
	require.NoError(t, err)
	checks := []struct {
		name         string
		versionID    string
		generationID string
		model        string
	}{
		{"historical", first.CurrentVersionID, generationA.GenerationID, "A"},
		{"replacement", second.ID, generationB.GenerationID, "B"},
		{"revert", revert.ID, generationA.GenerationID, "A"},
		{"late duplicate", late.CurrentVersionID, generationA.GenerationID, "A"},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			projection, err := s.ContentVersionPhotoMetadata(ctx, check.versionID)
			require.NoError(t, err)
			assert.Equal(t, check.versionID, projection.ContentVersionID)
			assert.Equal(t, check.generationID, projection.GenerationID)
			require.NotNil(t, projection.Fields.CameraModel)
			assert.Equal(t, check.model, *projection.Fields.CameraModel)
		})
	}
	assert.Equal(t, revert.ID, reverted.CurrentVersionID)
}

func TestPhotoTechnicalMetadataJSONLRejectsMalformed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	source := newTestStore(t)
	node, err := source.CreateFile(ctx, source.RootID(), "malformed.jpg", fakeHash("a7"), 1, "image/jpeg")
	require.NoError(t, err)
	canonical := photoCanonical(t,
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Malformed")),
	)
	generation, err := source.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("f8"), canonical)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))
	var projectionLine string
	for line := range strings.SplitSeq(exported.String(), "\n") {
		if strings.Contains(line, `"type":"photo_technical_metadata"`) {
			projectionLine = line
			break
		}
	}
	require.NotEmpty(t, projectionLine)
	malformed := strings.Replace(exported.String(), projectionLine,
		strings.Replace(projectionLine, generation.GenerationID, fakeHash("ff"), 1), 1)
	rejected := newTestStore(t)
	require.Error(t, rejected.ImportMetadata(ctx, strings.NewReader(malformed)))
	var nodeCount int
	require.NoError(t, rejected.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes`).Scan(&nodeCount))
	assert.Equal(t, 1, nodeCount, "a dangling projection must roll back the entire import")

	duplicate := exported.String() + projectionLine + "\n"
	rejectedDuplicate := newTestStore(t)
	require.ErrorContains(t, rejectedDuplicate.ImportMetadata(ctx, strings.NewReader(duplicate)), "duplicate photo technical metadata")
	require.NoError(t, rejectedDuplicate.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes`).Scan(&nodeCount))
	assert.Equal(t, 1, nodeCount, "a duplicate projection must roll back the entire import")
}

func TestPhotoTechnicalMetadataBackupScope(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "backup.jpg", fakeHash("a8"), 1, "image/jpeg")
	require.NoError(t, err)
	canonical := photoCanonical(t,
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Backup")),
	)
	generation, err := s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("f9"), canonical)
	require.NoError(t, err)
	snapshot, err := s.BeginMetadataSnapshot(ctx)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, snapshot.ExportBackup(ctx, &exported))
	require.NoError(t, snapshot.Close())
	assert.Contains(t, exported.String(), `"type":"source_metadata_generation"`)
	assert.Contains(t, exported.String(), `"type":"photo_technical_metadata"`)
	assert.Contains(t, exported.String(), generation.GenerationID)
}

func TestPhotoTechnicalMetadataMembershipIndependence(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	directory, _, err := s.MkdirPath(ctx, "/photos")
	require.NoError(t, err)
	_, err = s.ContentVersionPhotoMetadata(ctx, directory.CurrentVersionID)
	require.ErrorIs(t, err, ErrNotFound)
	node, err := s.CreateFile(ctx, s.RootID(), "lifecycle.jpg", fakeHash("aa"), 1, "image/jpeg")
	require.NoError(t, err)
	canonical := photoCanonical(t,
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Lifecycle")),
	)
	generation, err := s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("fa"), canonical)
	require.NoError(t, err)
	automatic, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	assert.Equal(t, PhotoRoleImage, automatic.Files[0].Role)
	projection, err := s.ContentVersionPhotoMetadata(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	assert.Equal(t, generation.GenerationID, projection.GenerationID)
	excluded, err := s.SetPhotoAssetExcluded(ctx, automatic.ID, automatic.Revision, true)
	require.NoError(t, err)
	require.NotNil(t, excluded.ExcludedAt)
	projection, err = s.ContentVersionPhotoMetadata(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	assert.Equal(t, generation.GenerationID, projection.GenerationID)
	trashed, _, err := s.Trash(ctx, node.ID, node.Revision)
	require.NoError(t, err)
	projection, err = s.ContentVersionPhotoMetadata(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	assert.Equal(t, generation.GenerationID, projection.GenerationID)
	_, _, err = s.Restore(ctx, trashed.ID, trashed.Revision)
	require.NoError(t, err)
	projection, err = s.ContentVersionPhotoMetadata(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	assert.Equal(t, "Lifecycle", *projection.Fields.CameraModel)

	raw, err := s.CreateFile(ctx, s.RootID(), "lifecycle.cr2", fakeHash("ab"), 1, "application/octet-stream")
	require.NoError(t, err)
	group, err := s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	assert.Equal(t, PhotoRoleRAW, group.Files[0].Role)

	attached, err := s.CreateFile(ctx, s.RootID(), "attached.jpg", fakeHash("ac"), 1, "image/jpeg")
	require.NoError(t, err)
	attachedCanonical := photoCanonical(t,
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Attached")),
	)
	_, err = s.PublishSourceMetadata(ctx, attached.BlobHash, fakeHash("fb"), attachedCanonical)
	require.NoError(t, err)
	attachedAutomatic, err := s.PhotoAssetForNode(ctx, attached.ID)
	require.NoError(t, err)
	attachedFileID := attachedAutomatic.Files[0].ID
	_, err = s.DetachPhotoFile(ctx, attachedAutomatic.ID, attachedAutomatic.Revision, attachedFileID, PhotoDetachOptions{})
	require.NoError(t, err)
	group, err = s.AttachPhotoFile(ctx, group.ID, group.Revision, attached.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	attachedMembership, err := s.PhotoAssetForNode(ctx, attached.ID)
	require.NoError(t, err)
	assert.Equal(t, group.ID, attachedMembership.ID)

	preference := "image"
	settings, err := s.SetPhotoSettings(ctx, 1, &preference)
	require.NoError(t, err)
	assert.Equal(t, int64(2), settings.Revision)
	group, err = s.PhotoAssetByID(ctx, group.ID)
	require.NoError(t, err)
	assert.Equal(t, PhotoDisplayVault, group.DisplaySource)
	assert.Equal(t, PhotoRoleImage, fileByID(group.Files, *group.DisplayFileID).Role)
	rawFileID := fileByRole(group.Files, PhotoRoleRAW).ID
	group, err = s.SetPhotoDisplay(ctx, group.ID, group.Revision, &rawFileID)
	require.NoError(t, err)
	assert.Equal(t, PhotoDisplayAsset, group.DisplaySource)
	assert.Equal(t, rawFileID, *group.DisplayOverrideFileID)
	group, err = s.SetPhotoAssetExcluded(ctx, group.ID, group.Revision, true)
	require.NoError(t, err)
	require.NotNil(t, group.ExcludedAt)
	attachedProjection, err := s.ContentVersionPhotoMetadata(ctx, attached.CurrentVersionID)
	require.NoError(t, err)
	assert.Equal(t, "Attached", *attachedProjection.Fields.CameraModel)

	history, err := s.CreateFile(ctx, s.RootID(), "history.jpg", fakeHash("ad"), 1, "image/jpeg")
	require.NoError(t, err)
	historicalVersionID := history.CurrentVersionID
	historicalCanonical := photoCanonical(t,
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Historical")),
	)
	_, err = s.PublishSourceMetadata(ctx, history.BlobHash, fakeHash("fc"), historicalCanonical)
	require.NoError(t, err)
	replaced, replacement, err := s.ReplaceContent(ctx, history.ID, history.Revision, fakeHash("ae"), 1, "image/jpeg")
	require.NoError(t, err)
	replacementCanonical := photoCanonical(t,
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Replacement")),
	)
	_, err = s.PublishSourceMetadata(ctx, replacement.BlobHash, fakeHash("fd"), replacementCanonical)
	require.NoError(t, err)
	_, err = s.PruneContentVersions(ctx, replaced.ID, replaced.Revision,
		VersionPruneSelector{VersionIDs: []string{historicalVersionID}}, true)
	require.NoError(t, err)
	_, err = s.ContentVersionPhotoMetadata(ctx, historicalVersionID)
	require.ErrorIs(t, err, ErrNotFound)
	replacementProjection, err := s.ContentVersionPhotoMetadata(ctx, replacement.ID)
	require.NoError(t, err)
	assert.Equal(t, "Replacement", *replacementProjection.Fields.CameraModel)
}

func TestPhotoTechnicalMetadataGenerationCascade(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "cascade.jpg", fakeHash("ab"), 1, "image/jpeg")
	require.NoError(t, err)
	canonical := photoCanonical(t,
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Cascade")),
	)
	generation, err := s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("fb"), canonical)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `DELETE FROM source_metadata_heads WHERE source_sha256=?`, node.BlobHash)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `DELETE FROM source_metadata_generations WHERE generation_id=?`, generation.GenerationID)
	require.NoError(t, err)
	var projections int
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM photo_technical_metadata WHERE generation_id=?`, generation.GenerationID).Scan(&projections))
	assert.Zero(t, projections)
}

func TestPhotoTechnicalMetadataCorruptSource(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "corrupt.jpg", fakeHash("a2"), 1, "image/jpeg")
	require.NoError(t, err)
	canonical := photoCanonical(t, photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("A")))
	generation, err := s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("e2"), canonical)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `DROP TRIGGER source_metadata_generations_immutable_update`)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE source_metadata_generations SET canonical_json=? WHERE generation_id=?`, []byte("{}"), generation.GenerationID)
	require.NoError(t, err)
	_, err = s.ContentVersionPhotoMetadata(ctx, node.CurrentVersionID)
	require.ErrorIs(t, err, ErrSourceMetadataCorrupt)
}

func TestPhotoTechnicalMetadataJSONLRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	source := newTestStore(t)
	node, err := source.CreateFile(ctx, source.RootID(), "roundtrip.jpg", fakeHash("a3"), 1, "image/jpeg")
	require.NoError(t, err)
	canonical := photoCanonical(t,
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Roundtrip")),
		photoMetadataField("image.exif.gps_latitude", "image.exif", "GPSLatitude", photoString("48.8566000")),
		photoMetadataField("image.exif.gps_longitude", "image.exif", "GPSLongitude", photoString("2.3522000")),
	)
	_, err = source.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("e3"), canonical)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	projection, err := target.ContentVersionPhotoMetadata(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	assert.Equal(t, "Roundtrip", *projection.Fields.CameraModel)
	assert.Equal(t, "photo-technical/v1", projection.ProjectionRecipe)
	require.NotNil(t, projection.Fields.LocationLabel)
	assert.Contains(t, *projection.Fields.LocationLabel, "France")
	var restored bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &restored))
	assert.Equal(t, exported.String(), restored.String())
}

func TestPhotoTechnicalMetadataLegacyJSONL(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	source := newTestStore(t)
	node, err := source.CreateFile(ctx, source.RootID(), "legacy.jpg", fakeHash("a4"), 1, "image/jpeg")
	require.NoError(t, err)
	canonical := photoCanonical(t, photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Legacy")))
	_, err = source.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("e4"), canonical)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))
	var legacy strings.Builder
	for line := range strings.SplitSeq(exported.String(), "\n") {
		if !strings.Contains(line, `"type":"photo_technical_metadata"`) {
			legacy.WriteString(line)
			legacy.WriteByte('\n')
		}
	}
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, strings.NewReader(legacy.String())))
	projection, err := target.ContentVersionPhotoMetadata(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	assert.Equal(t, "Legacy", *projection.Fields.CameraModel)
}

func TestPhotoTechnicalMetadataLegacyFillSkipsExistingProjection(t *testing.T) { //nolint:paralleltest // swaps the package-level gazetteer loader
	s := newTestStore(t)
	ctx := t.Context()
	gpsNode, err := s.CreateFile(ctx, s.RootID(), "already-projected.jpg", fakeHash("a9"), 1, "image/jpeg")
	require.NoError(t, err)
	gpsCanonical := photoCanonical(t,
		photoMetadataField("image.exif.gps_latitude", "image.exif", "GPSLatitude", photoString("48.8566000")),
		photoMetadataField("image.exif.gps_longitude", "image.exif", "GPSLongitude", photoString("2.3522000")),
	)
	gpsGeneration, err := s.PublishSourceMetadata(ctx, gpsNode.BlobHash, fakeHash("f0"), gpsCanonical)
	require.NoError(t, err)
	missingNode, err := s.CreateFile(ctx, s.RootID(), "missing-projection.jpg", fakeHash("aa"), 1, "image/jpeg")
	require.NoError(t, err)
	missingCanonical := photoCanonical(t,
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Missing")),
	)
	missingGeneration, err := s.PublishSourceMetadata(ctx, missingNode.BlobHash, fakeHash("f1"), missingCanonical)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `DELETE FROM photo_technical_metadata WHERE generation_id=?`, missingGeneration.GenerationID)
	require.NoError(t, err)

	original := photoNaturalEarth
	photoNaturalEarth = sync.OnceValues(func() (*geo.NaturalEarth, error) {
		return nil, errors.New("unexpected gazetteer lookup for existing projection")
	})
	defer func() { photoNaturalEarth = original }()
	tx, err := s.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, fillMissingPhotoTechnicalMetadataTx(ctx, tx))
	require.NoError(t, tx.Commit())
	var restored int
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM photo_technical_metadata WHERE generation_id=?`, missingGeneration.GenerationID).Scan(&restored))
	assert.Equal(t, 1, restored)
	var existing int
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM photo_technical_metadata WHERE generation_id=?`, gpsGeneration.GenerationID).Scan(&existing))
	assert.Equal(t, 1, existing)
}
