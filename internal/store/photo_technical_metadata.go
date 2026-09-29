package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/geo"
)

// PhotoTechnicalProjectionRecipe identifies the mapping and embedded
// gazetteer data used to derive one projection row.
const PhotoTechnicalProjectionRecipe = "photo-technical/v1"

const photoTechnicalMetadataSchemaVersion = 27

var photoTechnicalOffsetPattern = regexp.MustCompile(`^[+-](0[0-9]|1[0-4]):[0-5][0-9]$`)

// PhotoTechnicalFields are typed facts projected from one source-metadata
// generation. Nil values mean that the source did not provide a valid fact.
type PhotoTechnicalFields struct {
	CameraMake           *string
	CameraModel          *string
	LensMake             *string
	LensModel            *string
	ISO                  *int64
	ExposureTimeSeconds  *float64
	FNumber              *float64
	ExposureBiasEV       *float64
	FocalLengthMM        *float64
	WidthPX              *int64
	HeightPX             *int64
	CaptureTime          *string
	CaptureTimeRaw       *string
	CaptureTimePrecision *string
	CaptureTimeTimezone  *string
	CaptureTimeOffset    *string
	Orientation          *int64
	Latitude             *float64
	Longitude            *float64
	LocationLabel        *string
}

// PhotoTechnicalMetadata combines exact-version identity with the projected
// fields selected from that version's active source generation.
type PhotoTechnicalMetadata struct {
	Fields               PhotoTechnicalFields
	ContentVersionID     string
	GenerationID         string
	ExtractorFingerprint string
	SourceChecksum       string
	ProjectionRecipe     string
}

var photoNaturalEarth = sync.OnceValues(geo.NewNaturalEarth)

// projectPhotoTechnicalMetadata maps accepted canonical claims into nullable
// typed columns. It does not choose between repeated claims because the
// canonical source codec rejects duplicate keys before this function runs.
func projectPhotoTechnicalMetadata(metadata document.SourceMetadataV1, places *geo.NaturalEarth) PhotoTechnicalFields {
	var result PhotoTechnicalFields
	var containerWidth, containerHeight, exifWidth, exifHeight *int64
	for _, field := range metadata.Fields {
		switch field.Key {
		case "image.exif.camera_make":
			result.CameraMake = sourceMetadataString(field, "Make")
		case "image.exif.camera_model":
			result.CameraModel = sourceMetadataString(field, "Model")
		case "image.exif.lens_make":
			result.LensMake = sourceMetadataString(field, "LensMake")
		case "image.exif.lens_model":
			result.LensModel = sourceMetadataString(field, "LensModel")
		case "image.exif.iso":
			result.ISO = sourceMetadataIntegerAny(field, "image.exif", true, "PhotographicSensitivity", "ISO")
		case "image.exif.exposure_time_seconds":
			result.ExposureTimeSeconds = sourceMetadataNumber(field, "ExposureTime", true)
		case "image.exif.f_number":
			result.FNumber = sourceMetadataNumber(field, "FNumber", true)
		case "image.exif.exposure_bias_ev":
			result.ExposureBiasEV = sourceMetadataNumber(field, "ExposureBiasValue", false)
		case "image.exif.focal_length_mm":
			result.FocalLengthMM = sourceMetadataNumber(field, "FocalLength", true)
		case "media.container.width_px":
			containerWidth = sourceMetadataContainerDimension(field, true)
		case "image.exif.pixel_width":
			exifWidth = sourceMetadataInteger(field, "image.exif", "PixelXDimension", true)
		case "media.container.height_px":
			containerHeight = sourceMetadataContainerDimension(field, false)
		case "image.exif.pixel_height":
			exifHeight = sourceMetadataInteger(field, "image.exif", "PixelYDimension", true)
		case "created":
			result.CaptureTime, result.CaptureTimeRaw, result.CaptureTimePrecision,
				result.CaptureTimeTimezone, result.CaptureTimeOffset = sourceMetadataTimestamp(field)
		case "image.exif.orientation":
			result.Orientation = sourceMetadataInteger(field, "image.exif", "Orientation", true)
			if result.Orientation != nil && (*result.Orientation < 1 || *result.Orientation > 8) {
				result.Orientation = nil
			}
		}
	}
	if containerWidth != nil {
		result.WidthPX = containerWidth
	} else {
		result.WidthPX = exifWidth
	}
	if containerHeight != nil {
		result.HeightPX = containerHeight
	} else {
		result.HeightPX = exifHeight
	}

	latitude, longitude, ok := sourceMetadataGPS(metadata)
	if ok {
		result.Latitude = &latitude
		result.Longitude = &longitude
		if places != nil {
			if label, found := places.Resolve(latitude, longitude); found {
				result.LocationLabel = &label
			}
		}
	}
	return result
}

func sourceMetadataContainerDimension(field document.SourceMetadataFieldV1, width bool) *int64 {
	if field.Namespace != "media.container" || field.Value.Kind != document.SourceMetadataInteger || field.Value.Integer == nil {
		return nil
	}
	var allowed map[string]bool
	if width {
		allowed = map[string]bool{"Width": true, "ImageWidth": true, "RAFImageWidth": true, "PixelXDimension": true}
	} else {
		allowed = map[string]bool{"Height": true, "ImageLength": true, "RAFImageLength": true, "PixelYDimension": true}
	}
	if !allowed[field.SourceField] || *field.Value.Integer <= 0 {
		return nil
	}
	value := *field.Value.Integer
	return &value
}

func sourceMetadataIntegerAny(field document.SourceMetadataFieldV1, namespace string, positive bool, sourceFields ...string) *int64 {
	for _, sourceField := range sourceFields {
		if value := sourceMetadataInteger(field, namespace, sourceField, positive); value != nil {
			return value
		}
	}
	return nil
}

func sourceMetadataString(field document.SourceMetadataFieldV1, sourceField string) *string {
	if field.Namespace != "image.exif" || field.SourceField != sourceField ||
		field.Value.Kind != document.SourceMetadataString || field.Value.String == nil {
		return nil
	}
	value := *field.Value.String
	return &value
}

func sourceMetadataInteger(field document.SourceMetadataFieldV1, namespace, sourceField string, positive bool) *int64 {
	if field.Namespace != namespace || field.SourceField != sourceField ||
		field.Value.Kind != document.SourceMetadataInteger || field.Value.Integer == nil {
		return nil
	}
	value := *field.Value.Integer
	if positive && value <= 0 {
		return nil
	}
	return &value
}

func sourceMetadataNumber(field document.SourceMetadataFieldV1, sourceField string, positive bool) *float64 {
	if field.Namespace != "image.exif" || field.SourceField != sourceField ||
		field.Value.Kind != document.SourceMetadataNumber || field.Value.Number == nil {
		return nil
	}
	value := *field.Value.Number
	if math.IsNaN(value) || math.IsInf(value, 0) || positive && value <= 0 {
		return nil
	}
	return &value
}

func sourceMetadataTimestamp(field document.SourceMetadataFieldV1) (*string, *string, *string, *string, *string) {
	if field.Key != "created" || field.Value.Kind != document.SourceMetadataTimestamp || field.Value.Timestamp == nil {
		return nil, nil, nil, nil, nil
	}
	if (field.Namespace != "image.exif" || field.SourceField != "DateTimeOriginal") &&
		(field.Namespace != "media.container" || field.SourceField != "mvhd.CreationTime") {
		return nil, nil, nil, nil, nil
	}
	stamp := field.Value.Timestamp
	normalized, raw := stamp.Normalized, stamp.Raw
	precision, timezone, offset := string(stamp.Precision), string(stamp.Timezone), stamp.Offset
	return &normalized, &raw, &precision, &timezone, &offset
}

func sourceMetadataGPS(metadata document.SourceMetadataV1) (float64, float64, bool) {
	var latitude, longitude string
	for _, field := range metadata.Fields {
		if field.Namespace != "image.exif" || field.Value.Kind != document.SourceMetadataString || field.Value.String == nil {
			continue
		}
		switch field.Key {
		case "image.exif.gps_latitude":
			if field.SourceField == "GPSLatitude" {
				latitude = *field.Value.String
			}
		case "image.exif.gps_longitude":
			if field.SourceField == "GPSLongitude" {
				longitude = *field.Value.String
			}
		}
	}
	lat, latErr := strconv.ParseFloat(strings.TrimSpace(latitude), 64)
	lon, lonErr := strconv.ParseFloat(strings.TrimSpace(longitude), 64)
	if latErr != nil || lonErr != nil || math.IsNaN(lat) || math.IsInf(lat, 0) ||
		math.IsNaN(lon) || math.IsInf(lon, 0) || lat < -90 || lat > 90 || lon < -180 || lon > 180 ||
		lat == 0 && lon == 0 {
		return 0, 0, false
	}
	return lat, lon, true
}

func photoTechnicalFieldsForMetadata(metadata document.SourceMetadataV1) (PhotoTechnicalFields, error) {
	_, _, hasGPS := sourceMetadataGPS(metadata)
	if !hasGPS {
		return projectPhotoTechnicalMetadata(metadata, nil), nil
	}
	places, err := photoNaturalEarth()
	if err != nil {
		return PhotoTechnicalFields{}, fmt.Errorf("loading photo gazetteer: %w", err)
	}
	return projectPhotoTechnicalMetadata(metadata, places), nil
}

func insertPhotoTechnicalMetadataTx(
	ctx context.Context, tx *sql.Tx, generationID, recipe string, fields PhotoTechnicalFields,
) error {
	if err := validatePhotoTechnicalFields(fields); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO photo_technical_metadata(
		generation_id,projection_recipe,camera_make,camera_model,lens_make,lens_model,iso,
		exposure_time_seconds,f_number,exposure_bias_ev,focal_length_mm,width_px,height_px,
		capture_time,capture_time_raw,capture_time_precision,capture_time_timezone,capture_time_offset,
		orientation,latitude,longitude,location_label
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(generation_id) DO NOTHING`,
		generationID, recipe, fields.CameraMake, fields.CameraModel, fields.LensMake, fields.LensModel,
		fields.ISO, fields.ExposureTimeSeconds, fields.FNumber, fields.ExposureBiasEV,
		fields.FocalLengthMM, fields.WidthPX, fields.HeightPX, fields.CaptureTime, fields.CaptureTimeRaw,
		fields.CaptureTimePrecision, fields.CaptureTimeTimezone, fields.CaptureTimeOffset,
		fields.Orientation, fields.Latitude, fields.Longitude, fields.LocationLabel)
	return err
}

func scanPhotoTechnicalFields(row interface{ Scan(dest ...any) error }) (PhotoTechnicalFields, error) {
	var fields PhotoTechnicalFields
	var cameraMake, cameraModel, lensMake, lensModel sql.NullString
	var iso sql.NullInt64
	var exposureTime, fNumber, exposureBias, focalLength sql.NullFloat64
	var width, height sql.NullInt64
	var captureTime, captureRaw, capturePrecision, captureTimezone, captureOffset sql.NullString
	var orientation sql.NullInt64
	var latitude, longitude sql.NullFloat64
	var locationLabel sql.NullString
	err := row.Scan(&cameraMake, &cameraModel, &lensMake, &lensModel, &iso, &exposureTime,
		&fNumber, &exposureBias, &focalLength, &width, &height, &captureTime, &captureRaw,
		&capturePrecision, &captureTimezone, &captureOffset, &orientation, &latitude, &longitude,
		&locationLabel)
	if err != nil {
		return PhotoTechnicalFields{}, err
	}
	fields.CameraMake = photoNullableString(cameraMake)
	fields.CameraModel = photoNullableString(cameraModel)
	fields.LensMake = photoNullableString(lensMake)
	fields.LensModel = photoNullableString(lensModel)
	fields.ISO = photoNullableInt64(iso)
	fields.ExposureTimeSeconds = photoNullableFloat64(exposureTime)
	fields.FNumber = photoNullableFloat64(fNumber)
	fields.ExposureBiasEV = photoNullableFloat64(exposureBias)
	fields.FocalLengthMM = photoNullableFloat64(focalLength)
	fields.WidthPX = photoNullableInt64(width)
	fields.HeightPX = photoNullableInt64(height)
	fields.CaptureTime = photoNullableString(captureTime)
	fields.CaptureTimeRaw = photoNullableString(captureRaw)
	fields.CaptureTimePrecision = photoNullableString(capturePrecision)
	fields.CaptureTimeTimezone = photoNullableString(captureTimezone)
	fields.CaptureTimeOffset = photoNullableString(captureOffset)
	fields.Orientation = photoNullableInt64(orientation)
	fields.Latitude = photoNullableFloat64(latitude)
	fields.Longitude = photoNullableFloat64(longitude)
	fields.LocationLabel = photoNullableString(locationLabel)
	return fields, validatePhotoTechnicalFields(fields)
}

func photoNullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func photoNullableInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func photoNullableFloat64(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}

func validatePhotoTechnicalFields(fields PhotoTechnicalFields) error {
	for name, value := range map[string]*string{
		"camera_make": fields.CameraMake, "camera_model": fields.CameraModel,
		"lens_make": fields.LensMake, "lens_model": fields.LensModel,
		"capture_time": fields.CaptureTime, "capture_time_raw": fields.CaptureTimeRaw,
		"capture_time_precision": fields.CaptureTimePrecision, "capture_time_timezone": fields.CaptureTimeTimezone,
		"capture_time_offset": fields.CaptureTimeOffset, "location_label": fields.LocationLabel,
	} {
		if value != nil && (!utf8.ValidString(*value) || len(*value) > document.MaxSourceMetadataValueBytes) {
			return fmt.Errorf("photo technical %s is not bounded UTF-8", name)
		}
	}
	for name, value := range map[string]*float64{
		"exposure_time_seconds": fields.ExposureTimeSeconds, "f_number": fields.FNumber,
		"exposure_bias_ev": fields.ExposureBiasEV, "focal_length_mm": fields.FocalLengthMM,
		"latitude": fields.Latitude, "longitude": fields.Longitude,
	} {
		if value != nil && math.IsNaN(*value) || value != nil && math.IsInf(*value, 0) {
			return fmt.Errorf("photo technical %s must be finite", name)
		}
	}
	for name, value := range map[string]*float64{
		"exposure_time_seconds": fields.ExposureTimeSeconds, "f_number": fields.FNumber,
		"focal_length_mm": fields.FocalLengthMM,
	} {
		if value != nil && *value <= 0 {
			return fmt.Errorf("photo technical %s must be positive", name)
		}
	}
	if fields.WidthPX != nil && *fields.WidthPX <= 0 || fields.HeightPX != nil && *fields.HeightPX <= 0 {
		return errors.New("photo technical dimensions must be positive")
	}
	if fields.ISO != nil && *fields.ISO <= 0 {
		return errors.New("photo technical ISO must be positive")
	}
	if fields.Orientation != nil && (*fields.Orientation < 1 || *fields.Orientation > 8) {
		return errors.New("photo technical orientation is invalid")
	}
	if (fields.Latitude == nil) != (fields.Longitude == nil) {
		return errors.New("photo technical GPS coordinates must be a complete pair")
	}
	if fields.Latitude != nil && (*fields.Latitude < -90 || *fields.Latitude > 90 ||
		*fields.Longitude < -180 || *fields.Longitude > 180 || *fields.Latitude == 0 && *fields.Longitude == 0) {
		return errors.New("photo technical GPS coordinates are invalid")
	}
	captureValues := []*string{fields.CaptureTime, fields.CaptureTimeRaw, fields.CaptureTimePrecision, fields.CaptureTimeTimezone, fields.CaptureTimeOffset}
	hasCapture := false
	for _, value := range captureValues {
		hasCapture = hasCapture || value != nil
	}
	if hasCapture {
		for _, value := range captureValues {
			if value == nil {
				return errors.New("photo technical capture timestamp is incomplete")
			}
		}
		if *fields.CaptureTimePrecision != string(document.SourceMetadataPrecisionDate) &&
			*fields.CaptureTimePrecision != string(document.SourceMetadataPrecisionHour) &&
			*fields.CaptureTimePrecision != string(document.SourceMetadataPrecisionMinute) &&
			*fields.CaptureTimePrecision != string(document.SourceMetadataPrecisionSecond) &&
			*fields.CaptureTimePrecision != string(document.SourceMetadataPrecisionFraction) {
			return errors.New("photo technical timestamp precision is invalid")
		}
		switch *fields.CaptureTimeTimezone {
		case string(document.SourceMetadataTimezoneOmitted), string(document.SourceMetadataTimezoneUTC):
			if *fields.CaptureTimeOffset != "" {
				return errors.New("photo technical timestamp offset is inconsistent")
			}
		case string(document.SourceMetadataTimezoneOffset):
			if !photoTechnicalOffsetPattern.MatchString(*fields.CaptureTimeOffset) {
				return errors.New("photo technical timestamp offset is invalid")
			}
		default:
			return errors.New("photo technical timestamp timezone is invalid")
		}
		_, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{
			ContractVersion: document.SourceMetadataContractV1,
			Fields: []document.SourceMetadataFieldV1{{
				Key: "created", Namespace: "image.exif", SourceField: "DateTimeOriginal",
				Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataTimestamp,
					Timestamp: &document.SourceMetadataTimestampV1{
						Normalized: *fields.CaptureTime, Raw: *fields.CaptureTimeRaw,
						Precision: document.SourceMetadataTimestampPrecision(*fields.CaptureTimePrecision),
						Timezone:  document.SourceMetadataTimezoneKind(*fields.CaptureTimeTimezone),
						Offset:    *fields.CaptureTimeOffset,
					}},
			}},
		})
		if err != nil {
			return fmt.Errorf("photo technical capture timestamp is invalid: %w", err)
		}
	}
	return nil
}

// ContentVersionPhotoMetadata returns the projection selected by one exact
// content version's blob and active source-metadata head.
func (s *Store) ContentVersionPhotoMetadata(ctx context.Context, versionID string) (PhotoTechnicalMetadata, error) {
	if err := validateUUIDv4(versionID); err != nil {
		return PhotoTechnicalMetadata{}, fmt.Errorf("content version %q: %w", versionID, ErrNotFound)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PhotoTechnicalMetadata{}, fmt.Errorf("starting photo metadata snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	version, err := scanContentVersion(tx.QueryRowContext(ctx,
		`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=?`, versionID))
	if err != nil {
		return PhotoTechnicalMetadata{}, fmt.Errorf("content version %q: %w", versionID, err)
	}
	generation, _, err := activeSourceMetadata(ctx, tx, version.BlobHash)
	if err != nil {
		return PhotoTechnicalMetadata{}, err
	}
	result := PhotoTechnicalMetadata{
		ContentVersionID: version.ID, GenerationID: generation.GenerationID,
		ExtractorFingerprint: generation.ExtractorFingerprint, SourceChecksum: generation.Checksum,
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT projection_recipe FROM photo_technical_metadata WHERE generation_id=?`, generation.GenerationID).
		Scan(&result.ProjectionRecipe); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PhotoTechnicalMetadata{}, fmt.Errorf("photo technical metadata for content version %q: %w", versionID, ErrNotFound)
		}
		return PhotoTechnicalMetadata{}, fmt.Errorf("reading photo technical metadata recipe: %w", err)
	}
	fields, err := scanPhotoTechnicalFields(tx.QueryRowContext(ctx, `SELECT
		camera_make,camera_model,lens_make,lens_model,iso,exposure_time_seconds,f_number,
		exposure_bias_ev,focal_length_mm,width_px,height_px,capture_time,capture_time_raw,
		capture_time_precision,capture_time_timezone,capture_time_offset,orientation,latitude,
		longitude,location_label FROM photo_technical_metadata WHERE generation_id=?`, generation.GenerationID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PhotoTechnicalMetadata{}, fmt.Errorf("photo technical metadata for content version %q: %w", versionID, ErrNotFound)
		}
		return PhotoTechnicalMetadata{}, fmt.Errorf("reading photo technical metadata: %w", err)
	}
	result.Fields = fields
	if err := validatePhotoTechnicalRecipe(result.ProjectionRecipe); err != nil {
		return PhotoTechnicalMetadata{}, err
	}
	if err := tx.Commit(); err != nil {
		return PhotoTechnicalMetadata{}, fmt.Errorf("closing photo metadata snapshot: %w", err)
	}
	return result, nil
}

func validatePhotoTechnicalRecipe(recipe string) error {
	if recipe == "" || len(recipe) > document.MaxSourceMetadataLabelBytes || !utf8.ValidString(recipe) {
		return errors.New("photo technical projection recipe is invalid")
	}
	return nil
}
