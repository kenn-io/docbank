package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/text/cases"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/geo"
	"go.kenn.io/docbank/internal/query"
)

// PhotoTechnicalProjectionRecipe identifies the mapping and embedded
// gazetteer data used to derive projection rows. Changing it re-projects every
// retained source generation the next time a store opens.
const PhotoTechnicalProjectionRecipe = "photo-technical/v2"

// PhotoTechnicalFields are typed facts projected from one source-metadata
// generation. Nil values mean that the source did not provide a valid fact.
// Each JSON tag is also the field's SQL column name.
type PhotoTechnicalFields struct {
	CameraMake           *string  `json:"camera_make"`
	CameraModel          *string  `json:"camera_model"`
	LensMake             *string  `json:"lens_make"`
	LensModel            *string  `json:"lens_model"`
	ISO                  *int64   `json:"iso"`
	ExposureTimeSeconds  *float64 `json:"exposure_time_seconds"`
	FNumber              *float64 `json:"f_number"`
	ExposureBiasEV       *float64 `json:"exposure_bias_ev"`
	FocalLengthMM        *float64 `json:"focal_length_mm"`
	WidthPX              *int64   `json:"width_px"`
	HeightPX             *int64   `json:"height_px"`
	CaptureTime          *string  `json:"capture_time"`
	CaptureTimeRaw       *string  `json:"capture_time_raw"`
	CaptureTimePrecision *string  `json:"capture_time_precision"`
	CaptureTimeTimezone  *string  `json:"capture_time_timezone"`
	CaptureTimeOffset    *string  `json:"capture_time_offset"`
	Orientation          *int64   `json:"orientation"`
	Latitude             *float64 `json:"latitude"`
	Longitude            *float64 `json:"longitude"`
	LocationLabel        *string  `json:"location_label"`
}

// photoTechnicalColumns lists PhotoTechnicalFields' columns in field order.
var photoTechnicalColumns = func() []string {
	fields := reflect.TypeFor[PhotoTechnicalFields]()
	columns := make([]string, fields.NumField())
	for i := range columns {
		columns[i] = fields.Field(i).Tag.Get("json")
	}
	return columns
}()

var (
	photoTechnicalSelect = "p." + strings.Join(photoTechnicalColumns, ",p.")
	photoTechnicalInsert = "INSERT INTO photo_technical_metadata(generation_id,capture_sort_key,capture_date," +
		"camera_make_folded,camera_model_folded,lens_make_folded,lens_model_folded," +
		strings.Join(photoTechnicalColumns, ",") + ") VALUES(?,?,?,?,?,?,?" + strings.Repeat(",?", len(photoTechnicalColumns)) +
		") ON CONFLICT(generation_id) DO NOTHING"
)

// columnValues returns the fields in column order for SQL arguments.
func (f *PhotoTechnicalFields) columnValues() []any {
	value := reflect.ValueOf(f).Elem()
	values := make([]any, value.NumField())
	for i := range values {
		values[i] = value.Field(i).Interface()
	}
	return values
}

// columnPointers returns the fields' addresses in column order for Scan,
// which stores NULL as a nil pointer.
func (f *PhotoTechnicalFields) columnPointers() []any {
	value := reflect.ValueOf(f).Elem()
	pointers := make([]any, value.NumField())
	for i := range pointers {
		pointers[i] = value.Field(i).Addr().Interface()
	}
	return pointers
}

// empty reports whether the source supplied no photo fact at all.
func (f *PhotoTechnicalFields) empty() bool { return reflect.ValueOf(f).Elem().IsZero() }

// PhotoTechnicalMetadata combines exact-version identity with the projected
// fields selected from that version's active source generation.
type PhotoTechnicalMetadata struct {
	Fields               PhotoTechnicalFields
	ContentVersionID     string
	GenerationID         string
	ExtractorFingerprint string
	SourceChecksum       string
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

// insertPhotoTechnicalMetadataTx records a projection that has at least one
// fact. Generations without photo facts, such as PDFs and email, get no row.
func insertPhotoTechnicalMetadataTx(
	ctx context.Context, tx *sql.Tx, generationID string, fields PhotoTechnicalFields,
) error {
	if fields.empty() {
		return nil
	}
	var captureKey, captureDate string
	if fields.CaptureTime != nil {
		captureKey = query.CaptureTimeKey(*fields.CaptureTime, *fields.CaptureTimePrecision,
			*fields.CaptureTimeTimezone, *fields.CaptureTimeOffset)
		if captureKey != "" {
			// Date filters follow the source's calendar day, before timezone conversion.
			captureDate = (*fields.CaptureTime)[:len("2006-01-02")]
		}
	}
	args := []any{generationID, captureKey, captureDate}
	fold := cases.Fold()
	for _, label := range []*string{fields.CameraMake, fields.CameraModel, fields.LensMake, fields.LensModel} {
		folded := ""
		if label != nil {
			folded = fold.String(*label)
		}
		args = append(args, folded)
	}
	_, err := tx.ExecContext(ctx, photoTechnicalInsert, append(args, fields.columnValues()...)...)
	return err
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
	err = tx.QueryRowContext(ctx, `SELECT `+photoTechnicalSelect+`
		FROM photo_technical_metadata p WHERE p.generation_id=?`, generation.GenerationID).
		Scan(result.Fields.columnPointers()...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PhotoTechnicalMetadata{}, fmt.Errorf("photo technical metadata for content version %q: %w", versionID, ErrNotFound)
		}
		return PhotoTechnicalMetadata{}, fmt.Errorf("reading photo technical metadata: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return PhotoTechnicalMetadata{}, fmt.Errorf("closing photo metadata snapshot: %w", err)
	}
	return result, nil
}

// refreshPhotoTechnicalMetadata re-projects the store when it was last
// projected by another recipe. Unchanged stores pay one single-row read.
func (s *Store) refreshPhotoTechnicalMetadata(ctx context.Context) error {
	if current, err := photoTechnicalRecipeCurrent(ctx, s.db); err != nil || current {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if current, err := photoTechnicalRecipeCurrent(ctx, tx); err != nil || current {
			return err
		}
		return refreshPhotoTechnicalMetadataTx(ctx, tx)
	})
}

func photoTechnicalRecipeCurrent(ctx context.Context, q metadataQuerier) (bool, error) {
	var recipe string
	err := q.QueryRowContext(ctx,
		`SELECT projection_recipe FROM photo_technical_metadata_state WHERE singleton=1`).Scan(&recipe)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading photo technical projection recipe: %w", err)
	}
	return recipe == PhotoTechnicalProjectionRecipe, nil
}

// refreshPhotoTechnicalMetadataTx is the one owner of bulk projection. It
// drops every row, projects every generation from its retained canonical
// JSON, and records the recipe it applied, so generations that yielded no
// facts are not decoded again until it changes.
// Pages of generation IDs are read and closed before writes so both SQLite
// drivers can advance the same transaction safely. Each generation's canonical
// JSON, up to 8 MiB, is then read alone, so the pass holds one at a time.
func refreshPhotoTechnicalMetadataTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM photo_technical_metadata`); err != nil {
		return fmt.Errorf("removing stale photo technical metadata: %w", err)
	}
	readPage := func(after string) ([]string, error) {
		rows, err := tx.QueryContext(ctx, `SELECT generation_id
			FROM source_metadata_generations WHERE generation_id>?
			ORDER BY generation_id LIMIT 100`, after)
		if err != nil {
			return nil, fmt.Errorf("reading source metadata for photo projections: %w", err)
		}
		defer func() { _ = rows.Close() }()
		var page []string
		for rows.Next() {
			var generationID string
			if err := rows.Scan(&generationID); err != nil {
				return nil, err
			}
			page = append(page, generationID)
		}
		return page, rows.Err()
	}
	for after := ""; ; {
		page, err := readPage(after)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, generationID := range page {
			after = generationID
			var canonical []byte
			if err := tx.QueryRowContext(ctx, `SELECT canonical_json FROM source_metadata_generations
				WHERE generation_id=?`, generationID).Scan(&canonical); err != nil {
				return fmt.Errorf("reading source metadata %s for photo projection: %w", generationID, err)
			}
			metadata, _, err := document.DecodeSourceMetadataV1(canonical)
			if err != nil {
				// Reads already report corrupt evidence; it must not block opening the store.
				continue
			}
			fields, err := photoTechnicalFieldsForMetadata(metadata)
			if err != nil {
				return err
			}
			if err := insertPhotoTechnicalMetadataTx(ctx, tx, generationID, fields); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO photo_technical_metadata_state(singleton,projection_recipe)
		VALUES(1,?) ON CONFLICT(singleton) DO UPDATE SET projection_recipe=excluded.projection_recipe`,
		PhotoTechnicalProjectionRecipe)
	if err != nil {
		return fmt.Errorf("recording photo technical projection recipe: %w", err)
	}
	return nil
}
