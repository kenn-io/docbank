package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
	"go.kenn.io/kit/packstore"
)

const metadataPhotoTechnicalType = "photo_technical_metadata"

type metadataPhotoTechnical struct {
	Type                 string   `json:"type"`
	GenerationID         string   `json:"generation_id"`
	ProjectionRecipe     string   `json:"projection_recipe"`
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

func metadataPhotoTechnicalRecord(generationID, recipe string, fields PhotoTechnicalFields) metadataPhotoTechnical {
	return metadataPhotoTechnical{
		Type: metadataPhotoTechnicalType, GenerationID: generationID, ProjectionRecipe: recipe,
		CameraMake: fields.CameraMake, CameraModel: fields.CameraModel, LensMake: fields.LensMake,
		LensModel: fields.LensModel, ISO: fields.ISO, ExposureTimeSeconds: fields.ExposureTimeSeconds,
		FNumber: fields.FNumber, ExposureBiasEV: fields.ExposureBiasEV, FocalLengthMM: fields.FocalLengthMM,
		WidthPX: fields.WidthPX, HeightPX: fields.HeightPX, CaptureTime: fields.CaptureTime,
		CaptureTimeRaw: fields.CaptureTimeRaw, CaptureTimePrecision: fields.CaptureTimePrecision,
		CaptureTimeTimezone: fields.CaptureTimeTimezone, CaptureTimeOffset: fields.CaptureTimeOffset,
		Orientation: fields.Orientation, Latitude: fields.Latitude, Longitude: fields.Longitude,
		LocationLabel: fields.LocationLabel,
	}
}

func (record metadataPhotoTechnical) fields() PhotoTechnicalFields {
	return PhotoTechnicalFields{
		CameraMake: record.CameraMake, CameraModel: record.CameraModel, LensMake: record.LensMake,
		LensModel: record.LensModel, ISO: record.ISO, ExposureTimeSeconds: record.ExposureTimeSeconds,
		FNumber: record.FNumber, ExposureBiasEV: record.ExposureBiasEV, FocalLengthMM: record.FocalLengthMM,
		WidthPX: record.WidthPX, HeightPX: record.HeightPX, CaptureTime: record.CaptureTime,
		CaptureTimeRaw: record.CaptureTimeRaw, CaptureTimePrecision: record.CaptureTimePrecision,
		CaptureTimeTimezone: record.CaptureTimeTimezone, CaptureTimeOffset: record.CaptureTimeOffset,
		Orientation: record.Orientation, Latitude: record.Latitude, Longitude: record.Longitude,
		LocationLabel: record.LocationLabel,
	}
}

func validatePhotoTechnicalMetadataRecord(record metadataPhotoTechnical) error {
	if record.Type != metadataPhotoTechnicalType {
		return errors.New("invalid photo technical metadata record type")
	}
	if _, err := packstore.ParseHash(record.GenerationID); err != nil {
		return fmt.Errorf("invalid photo technical generation ID: %w", err)
	}
	if err := validatePhotoTechnicalRecipe(record.ProjectionRecipe); err != nil {
		return err
	}
	return validatePhotoTechnicalFields(record.fields())
}

func exportPhotoTechnicalMetadata(ctx context.Context, tx metadataQuerier, write metadataWrite, backupScoped bool) error {
	query := `SELECT p.generation_id,p.projection_recipe,p.camera_make,p.camera_model,p.lens_make,p.lens_model,
		p.iso,p.exposure_time_seconds,p.f_number,p.exposure_bias_ev,p.focal_length_mm,p.width_px,p.height_px,
		p.capture_time,p.capture_time_raw,p.capture_time_precision,p.capture_time_timezone,p.capture_time_offset,
		p.orientation,p.latitude,p.longitude,p.location_label
		FROM photo_technical_metadata p ORDER BY p.generation_id`
	if backupScoped {
		query = BackupBlobAuthorityCTE() + `
		SELECT p.generation_id,p.projection_recipe,p.camera_make,p.camera_model,p.lens_make,p.lens_model,
		p.iso,p.exposure_time_seconds,p.f_number,p.exposure_bias_ev,p.focal_length_mm,p.width_px,p.height_px,
		p.capture_time,p.capture_time_raw,p.capture_time_precision,p.capture_time_timezone,p.capture_time_offset,
		p.orientation,p.latitude,p.longitude,p.location_label
		FROM photo_technical_metadata p
		JOIN source_metadata_generations g ON g.generation_id=p.generation_id
		JOIN backup_authorized_blobs a ON a.hash=g.source_sha256
		ORDER BY p.generation_id`
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("exporting photo technical metadata: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record, err := scanPhotoTechnicalRecord(rows)
		if err != nil {
			return err
		}
		if err := validatePhotoTechnicalMetadataRecord(record); err != nil {
			return fmt.Errorf("validating photo technical metadata for export: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func scanPhotoTechnicalRecord(row interface{ Scan(dest ...any) error }) (metadataPhotoTechnical, error) {
	var record metadataPhotoTechnical
	var fields PhotoTechnicalFields
	var recipe string
	var cameraMake, cameraModel, lensMake, lensModel sql.NullString
	var iso sql.NullInt64
	var exposureTime, fNumber, exposureBias, focalLength sql.NullFloat64
	var width, height sql.NullInt64
	var captureTime, captureRaw, capturePrecision, captureTimezone, captureOffset sql.NullString
	var orientation sql.NullInt64
	var latitude, longitude sql.NullFloat64
	var locationLabel sql.NullString
	err := row.Scan(&record.GenerationID, &recipe, &cameraMake, &cameraModel, &lensMake, &lensModel,
		&iso, &exposureTime, &fNumber, &exposureBias, &focalLength, &width, &height,
		&captureTime, &captureRaw, &capturePrecision, &captureTimezone, &captureOffset,
		&orientation, &latitude, &longitude, &locationLabel)
	if err != nil {
		return metadataPhotoTechnical{}, err
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
	record = metadataPhotoTechnicalRecord(record.GenerationID, recipe, fields)
	return record, nil
}

func importPhotoTechnicalMetadataRecord(ctx context.Context, tx *sql.Tx, raw jsontext.Value) error {
	var record metadataPhotoTechnical
	if err := decodeMetadataRecord(raw, &record); err != nil {
		return err
	}
	if err := validatePhotoTechnicalMetadataRecord(record); err != nil {
		return err
	}
	var present int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM source_metadata_generations WHERE generation_id=?`, record.GenerationID).Scan(&present); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("photo technical metadata references missing generation %q", record.GenerationID)
		}
		return err
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM photo_technical_metadata WHERE generation_id=?`, record.GenerationID).Scan(&present); err == nil {
		return fmt.Errorf("duplicate photo technical metadata generation %q", record.GenerationID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return insertPhotoTechnicalMetadataTx(ctx, tx, record.GenerationID, record.ProjectionRecipe, record.fields())
}

// fillMissingPhotoTechnicalMetadataTx supplies projections for legacy JSONL
// streams that predate this table. Pages are read and closed before writes so
// both SQLite drivers can advance the same transaction safely.
func fillMissingPhotoTechnicalMetadataTx(ctx context.Context, tx *sql.Tx) error {
	type sourceRow struct {
		generationID string
		canonical    []byte
	}
	readPage := func(after string) ([]sourceRow, error) {
		rows, err := tx.QueryContext(ctx, `SELECT g.generation_id,g.canonical_json
			FROM source_metadata_generations g
			LEFT JOIN photo_technical_metadata p ON p.generation_id=g.generation_id
			WHERE p.generation_id IS NULL AND g.generation_id>?
			ORDER BY g.generation_id LIMIT 100`, after)
		if err != nil {
			return nil, fmt.Errorf("reading source metadata for photo projections: %w", err)
		}
		defer func() { _ = rows.Close() }()
		var page []sourceRow
		for rows.Next() {
			var item sourceRow
			if err := rows.Scan(&item.generationID, &item.canonical); err != nil {
				return nil, err
			}
			page = append(page, item)
		}
		return page, rows.Err()
	}
	after := ""
	for {
		page, err := readPage(after)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, item := range page {
			metadata, _, err := document.DecodeSourceMetadataV1(item.canonical)
			if err != nil {
				return fmt.Errorf("decoding source metadata generation %q: %w", item.generationID, err)
			}
			fields, err := photoTechnicalFieldsForMetadata(metadata)
			if err != nil {
				return err
			}
			if err := insertPhotoTechnicalMetadataTx(ctx, tx, item.generationID,
				PhotoTechnicalProjectionRecipe, fields); err != nil {
				return err
			}
			after = item.generationID
		}
	}
}

func validatePhotoTechnicalMetadataState(ctx context.Context, tx metadataQuerier) error {
	var missing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM source_metadata_generations g
		LEFT JOIN photo_technical_metadata p ON p.generation_id=g.generation_id
		WHERE p.generation_id IS NULL`).Scan(&missing); err != nil {
		return fmt.Errorf("checking photo technical generation coverage: %w", err)
	}
	if missing != 0 {
		return fmt.Errorf("photo technical metadata lacks %d generation projections", missing)
	}
	if err := exportPhotoTechnicalMetadata(ctx, tx, func(any) error { return nil }, false); err != nil {
		return fmt.Errorf("validating photo technical metadata: %w", err)
	}
	return nil
}
