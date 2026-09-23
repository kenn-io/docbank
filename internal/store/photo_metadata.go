package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
)

type metadataPhotoAsset struct {
	Type                  string  `json:"type"`
	AssetID               string  `json:"asset_id"`
	Kind                  string  `json:"kind"`
	Revision              int64   `json:"revision"`
	ExcludedAt            *string `json:"excluded_at"`
	DisplayFileID         *string `json:"display_file_id"`
	DisplayOverrideFileID *string `json:"display_override_file_id"`
	CreatedAt             string  `json:"created_at"`
	UpdatedAt             string  `json:"updated_at"`
}

type metadataPhotoFile struct {
	Type        string  `json:"type"`
	FileID      string  `json:"file_id"`
	AssetID     string  `json:"asset_id"`
	NodeID      int64   `json:"node_id"`
	Role        string  `json:"role"`
	SidecarOfID *string `json:"sidecar_of_file_id"`
	CreatedAt   string  `json:"created_at"`
}

type metadataPhotoSettings struct {
	Type       string  `json:"type"`
	Preference *string `json:"preference"`
	Revision   int64   `json:"revision"`
	UpdatedAt  string  `json:"updated_at"`
}

type metadataPhotoReceipt struct {
	Type           string  `json:"type"`
	ReceiptID      string  `json:"receipt_id"`
	Operation      string  `json:"operation"`
	AssetID        *string `json:"asset_id"`
	SettingsKey    *string `json:"settings_key"`
	BeforeRevision int64   `json:"before_revision"`
	AfterRevision  int64   `json:"after_revision"`
	BeforeJSON     string  `json:"before_json"`
	AfterJSON      string  `json:"after_json"`
	CreatedAt      string  `json:"created_at"`
}

func exportPhotoMetadata(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT asset_id, kind, revision, excluded_at, display_file_id,
		       display_override_file_id, created_at, updated_at
		FROM photo_assets ORDER BY asset_id`)
	if err != nil {
		return fmt.Errorf("exporting photo assets: %w", err)
	}
	for rows.Next() {
		var record metadataPhotoAsset
		if err := rows.Scan(&record.AssetID, &record.Kind, &record.Revision, &record.ExcludedAt, &record.DisplayFileID, &record.DisplayOverrideFileID, &record.CreatedAt, &record.UpdatedAt); err != nil {
			_ = rows.Close() //nolint:sqlclosecheck // close before returning the scan error.
			return err
		}
		record.Type = metadataPhotoAssetType
		if err := validatePhotoAssetMetadataRecord(record); err != nil {
			_ = rows.Close()
			return err
		}
		if err := write(record); err != nil {
			_ = rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `
		SELECT file_id, asset_id, node_id, role, sidecar_of_file_id, created_at
		FROM photo_files ORDER BY file_id`)
	if err != nil {
		return fmt.Errorf("exporting photo files: %w", err)
	}
	for rows.Next() {
		var record metadataPhotoFile
		if err := rows.Scan(&record.FileID, &record.AssetID, &record.NodeID, &record.Role, &record.SidecarOfID, &record.CreatedAt); err != nil {
			_ = rows.Close() //nolint:sqlclosecheck // close before returning the scan error.
			return err
		}
		record.Type = metadataPhotoFileType
		if err := validatePhotoFileMetadataRecord(record); err != nil {
			_ = rows.Close()
			return err
		}
		if err := write(record); err != nil {
			_ = rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var settings metadataPhotoSettings
	var preference sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT preference, revision, updated_at FROM photo_library_settings WHERE singleton=1`).Scan(&preference, &settings.Revision, &settings.UpdatedAt)
	if err == nil {
		settings.Type = metadataPhotoSettingsType
		if preference.Valid {
			settings.Preference = new(preference.String)
		}
		if err := validatePhotoSettingsMetadataRecord(settings); err != nil {
			return err
		}
		if err := write(settings); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("exporting photo settings: %w", err)
	}
	rows, err = tx.QueryContext(ctx, `
		SELECT receipt_id, operation, asset_id, settings_key, before_revision,
		       after_revision, before_json, after_json, created_at
		FROM photo_change_receipts ORDER BY receipt_id`)
	if err != nil {
		return fmt.Errorf("exporting photo receipts: %w", err)
	}
	for rows.Next() {
		var record metadataPhotoReceipt
		var assetID, settingsKey sql.NullString
		if err := rows.Scan(&record.ReceiptID, &record.Operation, &assetID, &settingsKey, &record.BeforeRevision, &record.AfterRevision, &record.BeforeJSON, &record.AfterJSON, &record.CreatedAt); err != nil {
			_ = rows.Close() //nolint:sqlclosecheck // close before returning the scan error.
			return err
		}
		if assetID.Valid {
			record.AssetID = new(assetID.String)
		}
		if settingsKey.Valid {
			record.SettingsKey = new(settingsKey.String)
		}
		record.Type = metadataPhotoReceiptType
		if err := validatePhotoReceiptMetadataRecord(record); err != nil {
			_ = rows.Close()
			return err
		}
		if err := write(record); err != nil {
			_ = rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	return rows.Close()
}

func validatePhotoAssetMetadataRecord(v metadataPhotoAsset) error {
	if v.Type != metadataPhotoAssetType || validateUUIDv4(v.AssetID) != nil || !photoKindValid(v.Kind) || v.Revision < 1 {
		return errors.New("invalid photo asset metadata")
	}
	if v.ExcludedAt != nil {
		if err := validateMetadataTime("photo asset excluded_at", *v.ExcludedAt); err != nil {
			return err
		}
	}
	if err := validateMetadataTime("photo asset created_at", v.CreatedAt); err != nil {
		return err
	}
	return validateMetadataTime("photo asset updated_at", v.UpdatedAt)
}

func validatePhotoFileMetadataRecord(v metadataPhotoFile) error {
	if v.Type != metadataPhotoFileType || validateUUIDv4(v.FileID) != nil || validateUUIDv4(v.AssetID) != nil || v.NodeID < 1 || !photoRoleValid(v.Role) {
		return errors.New("invalid photo file metadata")
	}
	if v.Role == PhotoRoleSidecar && v.SidecarOfID == nil {
		return errors.New("invalid photo sidecar pointer")
	}
	if v.SidecarOfID != nil && validateUUIDv4(*v.SidecarOfID) != nil {
		return errors.New("invalid photo sidecar pointer")
	}
	return validateMetadataTime("photo file created_at", v.CreatedAt)
}

func validatePhotoSettingsMetadataRecord(v metadataPhotoSettings) error {
	if v.Type != metadataPhotoSettingsType || v.Revision < 1 || !photoPreferenceValid(v.Preference) {
		return errors.New("invalid photo settings metadata")
	}
	if v.UpdatedAt != "" {
		return validateMetadataTime("photo settings updated_at", v.UpdatedAt)
	}
	return nil
}

func validatePhotoReceiptMetadataRecord(v metadataPhotoReceipt) error {
	if v.Type != metadataPhotoReceiptType || validateUUIDv4(v.ReceiptID) != nil || v.BeforeRevision < 0 || v.AfterRevision < 0 || len(v.BeforeJSON) > maxPhotoReceiptBytes || len(v.AfterJSON) > maxPhotoReceiptBytes {
		return errors.New("invalid photo receipt metadata")
	}
	switch v.Operation {
	case "create", "promote", "attach", "detach", "exclude", "display", "purge", "settings_recompute":
		if v.AssetID == nil || v.SettingsKey != nil {
			return errors.New("invalid photo receipt asset/settings identity")
		}
	case "settings":
		if v.AssetID != nil || v.SettingsKey == nil || *v.SettingsKey != "library" {
			return errors.New("invalid photo settings receipt identity")
		}
	default:
		return fmt.Errorf("invalid photo receipt operation %q", v.Operation)
	}
	if v.AfterRevision != v.BeforeRevision+1 || v.Operation == "create" && v.BeforeRevision != 0 ||
		v.Operation != "create" && v.Operation != "promote" && v.BeforeRevision < 1 {
		return errors.New("invalid photo receipt revision transition")
	}
	if v.AssetID != nil && validateUUIDv4(*v.AssetID) != nil {
		return errors.New("invalid photo receipt asset")
	}
	if !jsontext.Value(v.BeforeJSON).IsValid() || !jsontext.Value(v.AfterJSON).IsValid() {
		return errors.New("invalid photo receipt JSON")
	}
	return validateMetadataTime("photo receipt created_at", v.CreatedAt)
}

func importPhotoMetadataRecord(ctx context.Context, tx *sql.Tx, kind string, raw jsontext.Value) error {
	switch kind {
	case metadataPhotoAssetType:
		var v metadataPhotoAsset
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validatePhotoAssetMetadataRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO photo_assets(asset_id,kind,revision,excluded_at,display_file_id,display_override_file_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, v.AssetID, v.Kind, v.Revision, v.ExcludedAt, v.DisplayFileID, v.DisplayOverrideFileID, v.CreatedAt, v.UpdatedAt)
		return err
	case metadataPhotoFileType:
		var v metadataPhotoFile
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validatePhotoFileMetadataRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO photo_files(file_id,asset_id,node_id,role,sidecar_of_file_id,created_at) VALUES(?,?,?,?,?,?)`, v.FileID, v.AssetID, v.NodeID, v.Role, v.SidecarOfID, v.CreatedAt)
		return err
	case metadataPhotoSettingsType:
		var v metadataPhotoSettings
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validatePhotoSettingsMetadataRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO photo_library_settings(singleton,preference,revision,updated_at) VALUES(1,?,?,?)`, v.Preference, v.Revision, v.UpdatedAt)
		return err
	case metadataPhotoReceiptType:
		var v metadataPhotoReceipt
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validatePhotoReceiptMetadataRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO photo_change_receipts(receipt_id,operation,asset_id,settings_key,before_revision,after_revision,before_json,after_json,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, v.ReceiptID, v.Operation, v.AssetID, v.SettingsKey, v.BeforeRevision, v.AfterRevision, v.BeforeJSON, v.AfterJSON, v.CreatedAt)
		return err
	default:
		return fmt.Errorf("unknown photo metadata record %q", kind)
	}
}

func validatePhotoMetadataState(ctx context.Context, tx metadataQuerier) error {
	if err := validatePhotoGraph(ctx, tx); err != nil {
		return err
	}
	if err := validatePhotoReceiptHistory(ctx, tx); err != nil {
		return err
	}
	if err := exportPhotoMetadata(ctx, tx, func(any) error { return nil }); err != nil {
		return fmt.Errorf("validating photo metadata: %w", err)
	}
	return nil
}

func validatePhotoReceiptHistory(ctx context.Context, tx metadataQuerier) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT receipt_id, operation, asset_id, settings_key, before_revision,
		       after_revision, before_json, after_json, created_at
		FROM photo_change_receipts
		ORDER BY COALESCE(asset_id, settings_key), before_revision, after_revision, receipt_id`)
	if err != nil {
		return fmt.Errorf("reading photo receipt history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	assets := map[string][]metadataPhotoReceipt{}
	var settings []metadataPhotoReceipt
	for rows.Next() {
		var receipt metadataPhotoReceipt
		var assetID, settingsKey sql.NullString
		if err := rows.Scan(&receipt.ReceiptID, &receipt.Operation, &assetID, &settingsKey,
			&receipt.BeforeRevision, &receipt.AfterRevision, &receipt.BeforeJSON,
			&receipt.AfterJSON, &receipt.CreatedAt); err != nil {
			return fmt.Errorf("scanning photo receipt history: %w", err)
		}
		receipt.Type = metadataPhotoReceiptType
		if assetID.Valid {
			receipt.AssetID = new(assetID.String)
			assets[assetID.String] = append(assets[assetID.String], receipt)
		} else if settingsKey.Valid {
			receipt.SettingsKey = new(settingsKey.String)
			settings = append(settings, receipt)
		}
		if err := validatePhotoReceiptMetadataRecord(receipt); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading photo receipt history: %w", err)
	}
	for assetID, receipts := range assets {
		current, err := currentPhotoReceiptAssetState(ctx, tx, assetID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("invalid photo receipt history: asset %s does not exist", assetID)
		} else if err != nil {
			return fmt.Errorf("reading photo receipt asset %s: %w", assetID, err)
		}
		if err := validatePhotoAssetReceiptChain(assetID, current, receipts); err != nil {
			return err
		}
	}
	assetRows, err := tx.QueryContext(ctx, `
		SELECT asset_id, kind, revision, excluded_at, display_override_file_id,
		       (SELECT COUNT(*) FROM photo_files WHERE asset_id=photo_assets.asset_id),
		       COALESCE((SELECT role FROM photo_files WHERE asset_id=photo_assets.asset_id
		                 ORDER BY created_at, file_id LIMIT 1), '')
		FROM photo_assets`)
	if err != nil {
		return fmt.Errorf("reading revised photo assets: %w", err)
	}
	defer func() { _ = assetRows.Close() }()
	for assetRows.Next() {
		var assetID, kind, initialRole string
		var revision, fileCount int64
		var excluded, override sql.NullString
		if err := assetRows.Scan(&assetID, &kind, &revision, &excluded, &override, &fileCount, &initialRole); err != nil {
			return fmt.Errorf("scanning revised photo assets: %w", err)
		}
		if len(assets[assetID]) == 0 && (revision != 1 || excluded.Valid || override.Valid || fileCount != 1 ||
			kind == PhotoKindPhoto && initialRole != PhotoRoleImage || kind == PhotoKindVideo && initialRole != PhotoRoleVideo) {
			return fmt.Errorf("invalid photo receipt history: asset %s at a revised state has no history", assetID)
		}
	}
	if err := assetRows.Err(); err != nil {
		return fmt.Errorf("reading revised photo assets: %w", err)
	}
	if err := assetRows.Close(); err != nil {
		return err
	}
	if len(settings) > 0 {
		var current struct {
			Preference *string
			Revision   int64
		}
		var preference sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT preference, revision FROM photo_library_settings WHERE singleton=1`).Scan(&preference, &current.Revision); errors.Is(err, sql.ErrNoRows) {
			return errors.New("invalid photo receipt history: settings do not exist")
		} else if err != nil {
			return fmt.Errorf("reading photo receipt settings: %w", err)
		}
		if preference.Valid {
			current.Preference = new(preference.String)
		}
		if err := validatePhotoSettingsReceiptChain(current.Preference, current.Revision, settings); err != nil {
			return err
		}
	} else {
		var revision int64
		var preference sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT preference, revision FROM photo_library_settings WHERE singleton=1`).Scan(&preference, &revision); err == nil && (revision != 1 || preference.Valid) {
			return errors.New("invalid photo settings receipt history: non-default settings have no history")
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reading photo receipt settings: %w", err)
		}
	}
	return nil
}

func currentPhotoReceiptAssetState(ctx context.Context, tx metadataQuerier, assetID string) (photoReceiptAssetState, error) {
	var state photoReceiptAssetState
	var excluded, display, override sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT asset_id, kind, revision, excluded_at, display_file_id,
		       display_override_file_id,
		       (SELECT COUNT(*) FROM photo_files WHERE asset_id=photo_assets.asset_id),
		       COALESCE((SELECT role FROM photo_files WHERE asset_id=photo_assets.asset_id
		                 ORDER BY created_at, file_id LIMIT 1), '')
		FROM photo_assets WHERE asset_id=?`, assetID).Scan(&state.ID, &state.Kind, &state.Revision,
		&excluded, &display, &override, &state.FileCount, &state.InitialRole)
	if excluded.Valid {
		state.ExcludedAt = new(excluded.String)
	}
	if display.Valid {
		state.DisplayFileID = new(display.String)
	}
	if override.Valid {
		state.DisplayOverrideFileID = new(override.String)
	}
	return state, err
}

func validatePhotoAssetReceiptChain(assetID string, current photoReceiptAssetState, receipts []metadataPhotoReceipt) error {
	if receipts[0].BeforeRevision > 1 {
		return fmt.Errorf("invalid photo receipt history: asset %s starts at revision %d", assetID, receipts[0].BeforeRevision)
	}
	var previous int64
	var previousState photoReceiptAssetState
	for index, receipt := range receipts {
		if index > 0 && receipt.BeforeRevision != previous {
			return fmt.Errorf("invalid photo receipt history: asset %s has a revision gap", assetID)
		}
		var before, after photoReceiptAssetState
		if err := decodePhotoReceiptState(receipt.BeforeJSON, &before, "id", "revision"); err != nil || before.Revision != receipt.BeforeRevision ||
			before.Revision > 0 && before.ID != assetID || before.Revision == 0 && before.ID != "" {
			return fmt.Errorf("invalid photo receipt history: asset %s has a mismatched before state", assetID)
		}
		if index == 0 && before.Revision == 1 && !validAutomaticPhotoBaseline(before) {
			return fmt.Errorf("invalid photo receipt history: asset %s has an invalid automatic baseline", assetID)
		}
		if err := decodePhotoReceiptState(receipt.AfterJSON, &after, "id", "revision"); err != nil || after.ID != assetID || after.Revision != receipt.AfterRevision {
			return fmt.Errorf("invalid photo receipt history: asset %s has a mismatched after state", assetID)
		}
		if index > 0 && !equalPhotoReceiptAssetState(previousState, before) {
			return fmt.Errorf("invalid photo receipt history: asset %s has contradictory state at revision %d", assetID, before.Revision)
		}
		if !validPhotoReceiptOperationState(receipt.Operation, before, after) {
			return fmt.Errorf("invalid photo receipt history: asset %s has a mismatched %s transition", assetID, receipt.Operation)
		}
		previous = receipt.AfterRevision
		previousState = after
	}
	if !equalPhotoReceiptAssetState(previousState, current) {
		return fmt.Errorf("invalid photo receipt history: asset %s terminal state does not match revision %d", assetID, current.Revision)
	}
	return nil
}

func validAutomaticPhotoBaseline(state photoReceiptAssetState) bool {
	return state.FileCount == 1 && state.ExcludedAt == nil && state.DisplayOverrideFileID == nil &&
		(state.Kind == PhotoKindPhoto && state.InitialRole == PhotoRoleImage ||
			state.Kind == PhotoKindVideo && state.InitialRole == PhotoRoleVideo)
}

func equalPhotoReceiptAssetState(left, right photoReceiptAssetState) bool {
	return left.ID == right.ID && left.Kind == right.Kind && left.Revision == right.Revision &&
		equalPhotoString(left.ExcludedAt, right.ExcludedAt) && equalPhotoString(left.DisplayFileID, right.DisplayFileID) &&
		equalPhotoString(left.DisplayOverrideFileID, right.DisplayOverrideFileID) && left.FileCount == right.FileCount &&
		left.InitialRole == right.InitialRole
}

func validPhotoReceiptOperationState(operation string, before, after photoReceiptAssetState) bool {
	if before.Revision > 0 && before.Kind != after.Kind {
		return false
	}
	switch operation {
	case "create":
		return before.Revision == 0 && after.FileCount == 1 && receiptStateAddsInitialRole(after)
	case "promote":
		if before.Revision == 0 {
			return after.FileCount == 1 && receiptStateAddsInitialRole(after)
		}
		return before.ExcludedAt != nil && after.ExcludedAt == nil && before.FileCount == after.FileCount && before.InitialRole == after.InitialRole
	case "attach":
		if before.FileCount == 0 {
			return after.FileCount == 1 && receiptStateAddsInitialRole(after)
		}
		return after.FileCount == before.FileCount+1 && before.InitialRole == after.InitialRole
	case "detach", "purge":
		return after.FileCount < before.FileCount
	case "exclude":
		return before.FileCount == after.FileCount && before.InitialRole == after.InitialRole &&
			(before.ExcludedAt == nil) != (after.ExcludedAt == nil)
	case "display":
		return before.FileCount == after.FileCount && before.InitialRole == after.InitialRole &&
			(!equalPhotoString(before.DisplayOverrideFileID, after.DisplayOverrideFileID) || !equalPhotoString(before.DisplayFileID, after.DisplayFileID))
	case "settings_recompute":
		return before.FileCount == after.FileCount && before.InitialRole == after.InitialRole &&
			!equalPhotoString(before.DisplayFileID, after.DisplayFileID)
	default:
		return false
	}
}

func receiptStateAddsInitialRole(state photoReceiptAssetState) bool {
	for _, change := range state.MemberChanges {
		if change.After != nil && change.After.Role == state.InitialRole {
			return true
		}
	}
	return false
}

func validatePhotoSettingsReceiptChain(preference *string, revision int64, receipts []metadataPhotoReceipt) error {
	if receipts[0].BeforeRevision != 1 {
		return errors.New("invalid photo settings receipt history: history must start at revision 1")
	}
	var previous int64
	var previousPreference *string
	for index, receipt := range receipts {
		if index > 0 && receipt.BeforeRevision != previous {
			return errors.New("invalid photo settings receipt history: revision gap")
		}
		var before, after struct {
			Preference *string `json:"preference"`
			Revision   int64   `json:"revision"`
		}
		if err := decodePhotoReceiptState(receipt.BeforeJSON, &before, "revision"); err != nil || before.Revision != receipt.BeforeRevision {
			return errors.New("invalid photo settings receipt history: mismatched before state")
		}
		if err := decodePhotoReceiptState(receipt.AfterJSON, &after, "revision"); err != nil || after.Revision != receipt.AfterRevision {
			return errors.New("invalid photo settings receipt history: mismatched after state")
		}
		if index == 0 && before.Preference != nil || !photoPreferenceValid(before.Preference) || !photoPreferenceValid(after.Preference) {
			return errors.New("invalid photo settings receipt history: invalid preference state")
		}
		if index > 0 && !equalPhotoString(previousPreference, before.Preference) {
			return errors.New("invalid photo settings receipt history: contradictory state")
		}
		if equalPhotoString(before.Preference, after.Preference) {
			return errors.New("invalid photo settings receipt history: preference did not change")
		}
		previous = receipt.AfterRevision
		previousPreference = after.Preference
	}
	if previous != revision || !equalPhotoString(previousPreference, preference) {
		return fmt.Errorf("invalid photo settings receipt history: terminal state does not match revision %d", revision)
	}
	return nil
}

func decodePhotoReceiptState(raw string, target any, required ...string) error {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return err
	}
	for _, field := range required {
		if _, ok := fields[field]; !ok {
			return fmt.Errorf("missing %s", field)
		}
	}
	return json.Unmarshal([]byte(raw), target)
}
