package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
)

const maxPhotoReceiptBytes = 16 << 10

const maxPhotoInt64 = uint64(1<<63 - 1)

func photoAssetByIDQuery(ctx context.Context, q metadataQuerier, id string) (PhotoAsset, error) {
	if err := validateUUIDv4(id); err != nil {
		return PhotoAsset{}, fmt.Errorf("photo asset %q: %w", id, ErrNotFound)
	}
	var asset PhotoAsset
	var display, override sql.NullString
	if err := q.QueryRowContext(ctx, `
		SELECT asset_id, kind, revision, excluded_at, display_file_id,
		       display_override_file_id, created_at, updated_at
		FROM photo_assets WHERE asset_id=?`, id).Scan(
		&asset.ID, &asset.Kind, &asset.Revision, &asset.ExcludedAt, &display,
		&override, &asset.CreatedAt, &asset.UpdatedAt,
	); errors.Is(err, sql.ErrNoRows) {
		return PhotoAsset{}, ErrNotFound
	} else if err != nil {
		return PhotoAsset{}, fmt.Errorf("reading photo asset %q: %w", id, err)
	}
	if display.Valid {
		asset.DisplayFileID = new(display.String)
	}
	if override.Valid {
		asset.DisplayOverrideFileID = new(override.String)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT file_id, asset_id, node_id, role, sidecar_of_file_id, created_at
		FROM photo_files WHERE asset_id=? ORDER BY file_id LIMIT ?`, id, PhotoMaxFiles+1)
	if err != nil {
		return PhotoAsset{}, fmt.Errorf("reading photo asset files %q: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var file PhotoFile
		var sidecar sql.NullString
		if err := rows.Scan(&file.ID, &file.AssetID, &file.NodeID, &file.Role, &sidecar, &file.CreatedAt); err != nil {
			return PhotoAsset{}, fmt.Errorf("scanning photo asset file: %w", err)
		}
		if sidecar.Valid {
			file.SidecarOfID = new(sidecar.String)
		}
		asset.Files = append(asset.Files, file)
	}
	if err := rows.Err(); err != nil {
		return PhotoAsset{}, fmt.Errorf("reading photo asset files: %w", err)
	}
	if len(asset.Files) > PhotoMaxFiles {
		return PhotoAsset{}, fmt.Errorf("%w: asset contains more than %d files", ErrInvalidPhotoAsset, PhotoMaxFiles)
	}
	settings, err := photoSettingsTx(ctx, q)
	if err != nil {
		return PhotoAsset{}, err
	}
	choice := selectPhotoDisplay(asset.Files, settings.Preference, asset.DisplayOverrideFileID)
	asset.DisplaySource = choice.Source
	if !equalPhotoString(asset.DisplayFileID, choice.FileID) {
		return PhotoAsset{}, fmt.Errorf("%w: asset %s has a stale display pointer", ErrInvalidPhotoAsset, id)
	}
	if err := validatePhotoAssetPointers(asset, asset.Files); err != nil {
		return PhotoAsset{}, err
	}
	return asset, nil
}

func photoSettingsTx(ctx context.Context, q metadataQuerier) (PhotoSettings, error) {
	var settings PhotoSettings
	var preference sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT preference, revision, updated_at
		FROM photo_library_settings WHERE singleton=1`).Scan(
		&preference, &settings.Revision, &settings.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoSettings{Revision: 1, UpdatedAt: ""}, nil
	}
	if err != nil {
		return PhotoSettings{}, fmt.Errorf("reading photo settings: %w", err)
	}
	if preference.Valid {
		settings.Preference = new(preference.String)
	}
	if !photoPreferenceValid(settings.Preference) || settings.Revision < 1 {
		return PhotoSettings{}, fmt.Errorf("%w: invalid settings row", ErrInvalidPhotoAsset)
	}
	return settings, nil
}

// PhotoAssetByID returns one bounded photo graph.
func (s *Store) PhotoAssetByID(ctx context.Context, id string) (PhotoAsset, error) {
	return photoAssetByIDQuery(ctx, s.db, id)
}

// PhotoAssetForNode resolves the asset containing one ordinary file node.
func (s *Store) PhotoAssetForNode(ctx context.Context, nodeID int64) (PhotoAsset, error) {
	if nodeID < 1 {
		return PhotoAsset{}, fmt.Errorf("invalid photo node: %w", ErrNotFound)
	}
	var id string
	if err := s.db.QueryRowContext(ctx, `SELECT asset_id FROM photo_files WHERE node_id=?`, nodeID).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return PhotoAsset{}, ErrNotFound
	} else if err != nil {
		return PhotoAsset{}, fmt.Errorf("finding photo asset for node %d: %w", nodeID, err)
	}
	return s.PhotoAssetByID(ctx, id)
}

// PhotoSettings returns the virtual or stored vault preference.
func (s *Store) PhotoSettings(ctx context.Context) (PhotoSettings, error) {
	return photoSettingsTx(ctx, s.db)
}

func photoAssetState(asset PhotoAsset) any {
	return struct {
		ID                    string  `json:"id"`
		Kind                  string  `json:"kind"`
		Revision              int64   `json:"revision"`
		ExcludedAt            *string `json:"excluded_at"`
		DisplayFileID         *string `json:"display_file_id"`
		DisplayOverrideFileID *string `json:"display_override_file_id"`
		FileCount             int     `json:"file_count"`
	}{asset.ID, asset.Kind, asset.Revision, asset.ExcludedAt, asset.DisplayFileID, asset.DisplayOverrideFileID, len(asset.Files)}
}

func photoSettingsState(settings PhotoSettings) any {
	return struct {
		Preference *string `json:"preference"`
		Revision   int64   `json:"revision"`
	}{settings.Preference, settings.Revision}
}

func marshalPhotoState(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encoding photo receipt: %w", err)
	}
	if len(raw) > maxPhotoReceiptBytes {
		return "", fmt.Errorf("%w: receipt state exceeds %d bytes", ErrInvalidPhotoAsset, maxPhotoReceiptBytes)
	}
	return string(raw), nil
}

func writePhotoReceiptTx(ctx context.Context, tx *sql.Tx, operation, assetID, settingsKey string, beforeRevision, afterRevision int64, before, after any) error {
	beforeJSON, err := marshalPhotoState(before)
	if err != nil {
		return err
	}
	afterJSON, err := marshalPhotoState(after)
	if err != nil {
		return err
	}
	receiptID, err := newUUIDv4()
	if err != nil {
		return fmt.Errorf("allocating photo receipt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO photo_change_receipts(
			receipt_id, operation, asset_id, settings_key, before_revision,
			after_revision, before_json, after_json, created_at
		) VALUES(?,?,?,?,?,?,?,?,?)`, receiptID, operation, nullablePhotoText(assetID),
		nullablePhotoText(settingsKey), beforeRevision, afterRevision, beforeJSON,
		afterJSON, nowRFC3339()); err != nil {
		return fmt.Errorf("recording photo receipt: %w", err)
	}
	return nil
}

func nullablePhotoText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func equalPhotoString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func photoRevision(args []any) (int64, []any, error) {
	if len(args) == 0 {
		return 0, nil, fmt.Errorf("%w: expected revision", ErrPhotoAssetRevision)
	}
	var revision int64
	switch value := args[0].(type) {
	case int:
		revision = int64(value)
	case int64:
		revision = value
	case uint:
		if uint64(value) > maxPhotoInt64 {
			return 0, nil, fmt.Errorf("%w: revision overflow", ErrPhotoAssetRevision)
		}
		revision = int64(value)
	case uint64:
		if value > maxPhotoInt64 {
			return 0, nil, fmt.Errorf("%w: revision overflow", ErrPhotoAssetRevision)
		}
		revision = int64(value)
	default:
		return 0, nil, fmt.Errorf("%w: revision must be an integer", ErrPhotoAssetRevision)
	}
	if revision < 1 {
		return 0, nil, fmt.Errorf("%w: revision must be positive", ErrPhotoAssetRevision)
	}
	return revision, args[1:], nil
}

func photoIntArg(args []any, field string) (int64, []any, error) {
	if len(args) == 0 {
		return 0, nil, fmt.Errorf("%w: %s is required", ErrInvalidPhotoAsset, field)
	}
	switch value := args[0].(type) {
	case int:
		return int64(value), args[1:], nil
	case int64:
		return value, args[1:], nil
	case uint:
		if uint64(value) > maxPhotoInt64 {
			return 0, nil, fmt.Errorf("%w: %s overflows int64", ErrInvalidPhotoAsset, field)
		}
		return int64(value), args[1:], nil
	case uint64:
		if value > maxPhotoInt64 {
			return 0, nil, fmt.Errorf("%w: %s overflows int64", ErrInvalidPhotoAsset, field)
		}
		return int64(value), args[1:], nil
	default:
		return 0, nil, fmt.Errorf("%w: %s must be an integer", ErrInvalidPhotoAsset, field)
	}
}

func photoStringArg(args []any, field string) (string, []any, error) {
	if len(args) == 0 {
		return "", nil, fmt.Errorf("%w: %s is required", ErrInvalidPhotoAsset, field)
	}
	value, ok := args[0].(string)
	if !ok {
		if pointer, pointerOK := args[0].(*string); pointerOK && pointer != nil {
			value = *pointer
			ok = true
		}
	}
	if !ok || value == "" {
		return "", nil, fmt.Errorf("%w: %s must be a non-empty string", ErrInvalidPhotoAsset, field)
	}
	return value, args[1:], nil
}

func inferPhotoRole(facts PhotoNodeFacts) string {
	switch facts.AssetKind {
	case PhotoKindVideo:
		return PhotoRoleVideo
	case PhotoKindPhoto:
		return PhotoRoleImage
	default:
		return PhotoRoleRAW
	}
}

func loadPhotoFilesTx(ctx context.Context, tx *sql.Tx, assetID string) ([]PhotoFile, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT file_id, asset_id, node_id, role, sidecar_of_file_id, created_at
		FROM photo_files WHERE asset_id=? ORDER BY file_id LIMIT ?`, assetID, PhotoMaxFiles+1)
	if err != nil {
		return nil, fmt.Errorf("listing photo files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	files := make([]PhotoFile, 0)
	for rows.Next() {
		var file PhotoFile
		var sidecar sql.NullString
		if err := rows.Scan(&file.ID, &file.AssetID, &file.NodeID, &file.Role, &sidecar, &file.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning photo file: %w", err)
		}
		if sidecar.Valid {
			file.SidecarOfID = new(sidecar.String)
		}
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing photo files: %w", err)
	}
	if len(files) > PhotoMaxFiles {
		return nil, fmt.Errorf("%w: asset has more than %d files", ErrInvalidPhotoAsset, PhotoMaxFiles)
	}
	return files, nil
}

func loadPhotoAssetTx(ctx context.Context, tx *sql.Tx, id string) (PhotoAsset, error) {
	return photoAssetByIDQuery(ctx, tx, id)
}

func updatePhotoDisplayTx(ctx context.Context, tx *sql.Tx, asset PhotoAsset, settings PhotoSettings, now string) (PhotoAsset, error) {
	choice := selectPhotoDisplay(asset.Files, settings.Preference, asset.DisplayOverrideFileID)
	if equalPhotoString(asset.DisplayFileID, choice.FileID) {
		asset.DisplaySource = choice.Source
		return asset, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE photo_assets SET display_file_id=?, updated_at=? WHERE asset_id=?`, nullablePhotoString(choice.FileID), now, asset.ID); err != nil {
		return PhotoAsset{}, fmt.Errorf("updating photo display: %w", err)
	}
	asset.DisplayFileID = choice.FileID
	asset.DisplaySource = choice.Source
	return asset, nil
}

func nullablePhotoString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func insertPhotoFileTx(ctx context.Context, tx *sql.Tx, assetID string, nodeID int64, role string, sidecarOf *string, createdAt string) error {
	if err := photoRoleValidOrError(role); err != nil {
		return err
	}
	fileID, err := newUUIDv4()
	if err != nil {
		return fmt.Errorf("allocating photo file ID: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO photo_files(file_id,asset_id,node_id,role,sidecar_of_file_id,created_at)
		VALUES(?,?,?,?,?,?)`, fileID, assetID, nodeID, role, nullablePhotoString(sidecarOf), createdAt); err != nil {
		if sErr := sqliteUniquePhotoError(err); sErr != nil {
			return sErr
		}
		return fmt.Errorf("creating photo file: %w", err)
	}
	return nil
}

func photoRoleValidOrError(role string) error {
	if !photoRoleValid(role) {
		return fmt.Errorf("%w: unknown role %q", ErrInvalidPhotoAsset, role)
	}
	return nil
}

func sqliteUniquePhotoError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return fmt.Errorf("%w: node is already owned or file identity conflicts", ErrPhotoNodeOwned)
}

func photoAssetForMutationTx(ctx context.Context, tx *sql.Tx, assetID string, revision int64) (PhotoAsset, error) {
	asset, err := loadPhotoAssetTx(ctx, tx, assetID)
	if err != nil {
		return PhotoAsset{}, err
	}
	if asset.Revision != revision {
		return PhotoAsset{}, fmt.Errorf("asset %s at revision %d, expected %d: %w", assetID, asset.Revision, revision, ErrPhotoAssetRevision)
	}
	return asset, nil
}

func photoNodeForMutationTx(tx *sql.Tx, nodeID int64) (Node, PhotoNodeFacts, error) {
	node, err := nodeByIDTx(tx, nodeID)
	if err != nil {
		return Node{}, PhotoNodeFacts{}, err
	}
	if node.Kind != nodeKindFile || node.TrashedAt != nil {
		return Node{}, PhotoNodeFacts{}, fmt.Errorf("node %d: %w", nodeID, ErrPhotoNodeNotEligible)
	}
	return node, photoNodeFacts(node), nil
}

func photoAssetCreateTx(ctx context.Context, tx *sql.Tx, nodeID int64, explicitRole, explicitKind string, now string) (PhotoAsset, error) {
	node, facts, err := photoNodeForMutationTx(tx, nodeID)
	if err != nil {
		return PhotoAsset{}, err
	}
	var owned string
	if err := tx.QueryRowContext(ctx, `SELECT asset_id FROM photo_files WHERE node_id=?`, nodeID).Scan(&owned); err == nil {
		return PhotoAsset{}, fmt.Errorf("node %d belongs to asset %s: %w", nodeID, owned, ErrPhotoNodeOwned)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return PhotoAsset{}, fmt.Errorf("checking photo node ownership: %w", err)
	}
	role := explicitRole
	if role == "" {
		role = inferPhotoRole(facts)
	}
	if explicitRole == "" && !facts.Qualifies {
		return PhotoAsset{}, fmt.Errorf("node %d: %w", nodeID, ErrPhotoNodeNotEligible)
	}
	if role == PhotoRoleSidecar {
		return PhotoAsset{}, fmt.Errorf("%w: sidecars require a same-asset raw target", ErrInvalidPhotoAsset)
	}
	if role == PhotoRoleRAW && !facts.Qualifies && !isGenericRawPhotoName(node.Name, node.MimeType) {
		return PhotoAsset{}, fmt.Errorf("node %d: %w", nodeID, ErrPhotoNodeNotEligible)
	}
	if err := validatePhotoRoleForNode(role, facts); err != nil {
		return PhotoAsset{}, err
	}
	kind := explicitKind
	if kind == "" {
		kind = facts.AssetKind
	}
	if kind == "" {
		kind = PhotoKindPhoto
	}
	if !photoKindValid(kind) {
		return PhotoAsset{}, fmt.Errorf("%w: unknown kind %q", ErrInvalidPhotoAsset, kind)
	}
	if role == PhotoRoleImage && kind != PhotoKindPhoto ||
		role == PhotoRoleVideo && kind != PhotoKindVideo {
		return PhotoAsset{}, fmt.Errorf("%w: role %s does not match asset kind %s", ErrInvalidPhotoAsset, role, kind)
	}
	if facts.Qualifies && kind != facts.AssetKind {
		return PhotoAsset{}, fmt.Errorf("%w: node media does not match asset kind", ErrInvalidPhotoAsset)
	}
	if !facts.Qualifies && isGenericRawPhotoName(node.Name, node.MimeType) && kind != PhotoKindPhoto {
		return PhotoAsset{}, fmt.Errorf("%w: RAW files are photo assets", ErrInvalidPhotoAsset)
	}
	assetID, err := newUUIDv4()
	if err != nil {
		return PhotoAsset{}, fmt.Errorf("allocating photo asset ID: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO photo_assets(asset_id,kind,revision,created_at,updated_at)
		VALUES(?,?,1,?,?)`, assetID, kind, now, now); err != nil {
		return PhotoAsset{}, fmt.Errorf("creating photo asset: %w", err)
	}
	if err := insertPhotoFileTx(ctx, tx, assetID, node.ID, role, nil, now); err != nil {
		return PhotoAsset{}, err
	}
	files, err := loadPhotoFilesTx(ctx, tx, assetID)
	if err != nil {
		return PhotoAsset{}, err
	}
	asset := PhotoAsset{ID: assetID, Kind: kind, Revision: 1, Files: files}
	settings, err := photoSettingsTx(ctx, tx)
	if err != nil {
		return PhotoAsset{}, err
	}
	choice := selectPhotoDisplay(asset.Files, settings.Preference, nil)
	if _, err := tx.ExecContext(ctx, `UPDATE photo_assets SET display_file_id=? WHERE asset_id=?`, nullablePhotoString(choice.FileID), assetID); err != nil {
		return PhotoAsset{}, fmt.Errorf("selecting initial photo display: %w", err)
	}
	asset.DisplayFileID, asset.DisplaySource = choice.FileID, choice.Source
	if err := validatePhotoAssetPointers(asset, asset.Files); err != nil {
		return PhotoAsset{}, err
	}
	return asset, nil
}

// CreatePhotoAsset creates a singleton asset for one eligible node. Optional
// arguments may supply a role and kind for callers that explicitly classify a
// raw file.
func (s *Store) CreatePhotoAsset(ctx context.Context, nodeID int64, args ...any) (PhotoAsset, error) {
	var role, kind string
	for _, arg := range args {
		value, ok := arg.(string)
		if !ok {
			return PhotoAsset{}, fmt.Errorf("%w: create options must be text", ErrInvalidPhotoAsset)
		}
		if value == "" {
			continue
		}
		if photoRoleValid(value) {
			role = value
		} else if photoKindValid(value) {
			kind = value
		} else {
			return PhotoAsset{}, fmt.Errorf("%w: unknown create option %q", ErrInvalidPhotoAsset, value)
		}
	}
	var result PhotoAsset
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var ownedID string
		if err := tx.QueryRowContext(ctx,
			`SELECT asset_id FROM photo_files WHERE node_id=?`, nodeID).Scan(&ownedID); err == nil {
			return fmt.Errorf("node %d belongs to asset %s: %w", nodeID, ownedID, ErrPhotoNodeOwned)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("checking photo node ownership: %w", err)
		}
		var err error
		result, err = photoAssetCreateTx(ctx, tx, nodeID, role, kind, nowRFC3339())
		if err != nil {
			return err
		}
		if err := writePhotoReceiptTx(ctx, tx, "create", result.ID, "", 0, result.Revision, map[string]any{}, photoAssetState(result)); err != nil {
			return err
		}
		return validatePhotoGraphTx(ctx, tx)
	})
	if err != nil {
		return PhotoAsset{}, err
	}
	return result, nil
}

// PromotePhotoNode creates an explicitly requested asset for a live file. The
// optional role defaults to the node's qualifying media role and may be raw.
func (s *Store) PromotePhotoNode(ctx context.Context, nodeID int64, args ...any) (PhotoAsset, error) {
	var role, kind string
	for _, arg := range args {
		value, ok := arg.(string)
		if !ok {
			return PhotoAsset{}, fmt.Errorf("%w: promote options must be text", ErrInvalidPhotoAsset)
		}
		if value == "" {
			continue
		}
		if photoRoleValid(value) {
			role = value
		} else if photoKindValid(value) {
			kind = value
		} else {
			return PhotoAsset{}, fmt.Errorf("%w: unknown promote option %q", ErrInvalidPhotoAsset, value)
		}
	}
	var result PhotoAsset
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var ownedID string
		if err := tx.QueryRowContext(ctx,
			`SELECT asset_id FROM photo_files WHERE node_id=?`, nodeID).Scan(&ownedID); err == nil {
			asset, loadErr := loadPhotoAssetTx(ctx, tx, ownedID)
			if loadErr != nil {
				return loadErr
			}
			if asset.ExcludedAt == nil {
				result = asset
				return nil
			}
			before := asset
			asset.Revision++
			if _, err := tx.ExecContext(ctx,
				`UPDATE photo_assets SET excluded_at=NULL, revision=?, updated_at=? WHERE asset_id=?`,
				asset.Revision, nowRFC3339(), asset.ID); err != nil {
				return err
			}
			result, err = loadPhotoAssetTx(ctx, tx, asset.ID)
			if err != nil {
				return err
			}
			return writePhotoReceiptTx(ctx, tx, "promote", asset.ID, "", before.Revision,
				result.Revision, photoAssetState(before), photoAssetState(result))
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("checking photo node ownership: %w", err)
		}
		var err error
		result, err = photoAssetCreateTx(ctx, tx, nodeID, role, kind, nowRFC3339())
		if err != nil {
			return err
		}
		if err := writePhotoReceiptTx(ctx, tx, "promote", result.ID, "", 0, result.Revision, map[string]any{}, photoAssetState(result)); err != nil {
			return err
		}
		return validatePhotoGraphTx(ctx, tx)
	})
	if err != nil {
		return PhotoAsset{}, err
	}
	return result, nil
}

// AttachPhotoFile adds an ordinary node to an existing asset. Arguments are
// revision, node ID, optional role, and optional sidecar target in that order.
func (s *Store) AttachPhotoFile(ctx context.Context, assetID string, args ...any) (PhotoAsset, error) {
	revision, rest, err := photoRevision(args)
	if err != nil {
		return PhotoAsset{}, err
	}
	nodeID, rest, err := photoIntArg(rest, "node_id")
	if err != nil {
		return PhotoAsset{}, err
	}
	role := ""
	if len(rest) > 0 {
		role, rest, err = photoStringArg(rest, "role")
		if err != nil {
			return PhotoAsset{}, err
		}
	}
	var sidecar *string
	if len(rest) > 0 {
		sidecar, err = optionalPhotoString(rest[0])
		if err != nil {
			return PhotoAsset{}, fmt.Errorf("%w: sidecar_of_file_id: %w", ErrInvalidPhotoAsset, err)
		}
	}
	var result PhotoAsset
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		asset, err := photoAssetForMutationTx(ctx, tx, assetID, revision)
		if err != nil {
			return err
		}
		node, facts, err := photoNodeForMutationTx(tx, nodeID)
		if err != nil {
			return err
		}
		var ownedAssetID string
		if err := tx.QueryRowContext(ctx,
			`SELECT asset_id FROM photo_files WHERE node_id=?`, nodeID).Scan(&ownedAssetID); err == nil {
			return fmt.Errorf("node %d belongs to asset %s: %w", nodeID, ownedAssetID, ErrPhotoNodeOwned)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("checking photo node ownership: %w", err)
		}
		if role == "" {
			role = inferPhotoRole(facts)
			if !facts.Qualifies {
				role = PhotoRoleRAW
			}
		}
		if role == PhotoRoleRAW && !facts.Qualifies && !isGenericRawPhotoName(node.Name, node.MimeType) {
			return fmt.Errorf("node %d: %w", nodeID, ErrPhotoNodeNotEligible)
		}
		if err := validatePhotoRoleForNode(role, facts); err != nil && role != PhotoRoleRAW && role != PhotoRoleSidecar {
			return err
		}
		if role == PhotoRoleImage && asset.Kind != PhotoKindPhoto ||
			role == PhotoRoleVideo && asset.Kind != PhotoKindVideo {
			return fmt.Errorf("%w: role %s does not match asset kind %s", ErrInvalidPhotoAsset, role, asset.Kind)
		}
		if role == PhotoRoleSidecar {
			if sidecar == nil {
				return fmt.Errorf("%w: sidecar requires a raw target", ErrInvalidPhotoAsset)
			}
			var target PhotoFile
			if err := tx.QueryRowContext(ctx, `SELECT file_id, asset_id, node_id, role, sidecar_of_file_id, created_at FROM photo_files WHERE file_id=?`, *sidecar).Scan(&target.ID, &target.AssetID, &target.NodeID, &target.Role, new(sql.NullString), &target.CreatedAt); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%w: sidecar target not found", ErrInvalidPhotoAsset)
				}
				return err
			}
			if target.AssetID != asset.ID || target.Role != PhotoRoleRAW {
				return fmt.Errorf("%w: sidecar target must be same-asset raw", ErrInvalidPhotoAsset)
			}
		} else if sidecar != nil {
			return fmt.Errorf("%w: only sidecars may point to raw files", ErrInvalidPhotoAsset)
		}
		if err := insertPhotoFileTx(ctx, tx, asset.ID, node.ID, role, sidecar, nowRFC3339()); err != nil {
			return err
		}
		before := asset
		asset.Files, err = loadPhotoFilesTx(ctx, tx, asset.ID)
		if err != nil {
			return err
		}
		settings, err := photoSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		asset, err = updatePhotoDisplayTx(ctx, tx, asset, settings, nowRFC3339())
		if err != nil {
			return err
		}
		asset.Revision++
		if _, err := tx.ExecContext(ctx, `UPDATE photo_assets SET revision=?, updated_at=? WHERE asset_id=?`, asset.Revision, nowRFC3339(), asset.ID); err != nil {
			return err
		}
		result, err = loadPhotoAssetTx(ctx, tx, asset.ID)
		if err != nil {
			return err
		}
		if err := writePhotoReceiptTx(ctx, tx, "attach", asset.ID, "", before.Revision, result.Revision, photoAssetState(before), photoAssetState(result)); err != nil {
			return err
		}
		return validatePhotoGraphTx(ctx, tx)
	})
	if err != nil {
		return PhotoAsset{}, err
	}
	return result, nil
}

// DetachPhotoFile removes one member. Arguments are revision, file ID, and an
// optional replacement display file ID.
func (s *Store) DetachPhotoFile(ctx context.Context, assetID string, args ...any) (PhotoAsset, error) {
	revision, rest, err := photoRevision(args)
	if err != nil {
		return PhotoAsset{}, err
	}
	fileID, rest, err := photoStringArg(rest, "file_id")
	if err != nil {
		return PhotoAsset{}, err
	}
	var replacement *string
	if len(rest) > 0 {
		replacement, err = optionalPhotoString(rest[0])
		if err != nil {
			return PhotoAsset{}, err
		}
	}
	var result PhotoAsset
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		asset, err := photoAssetForMutationTx(ctx, tx, assetID, revision)
		if err != nil {
			return err
		}
		var file PhotoFile
		var sidecar sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT file_id, asset_id, node_id, role, sidecar_of_file_id, created_at FROM photo_files WHERE file_id=? AND asset_id=?`, fileID, assetID).Scan(&file.ID, &file.AssetID, &file.NodeID, &file.Role, &sidecar, &file.CreatedAt); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		before := asset
		if replacement != nil {
			var role string
			if err := tx.QueryRowContext(ctx, `SELECT role FROM photo_files WHERE file_id=? AND asset_id=?`, *replacement, assetID).Scan(&role); err != nil {
				return fmt.Errorf("%w: replacement file is not a member", ErrInvalidPhotoAsset)
			}
			if role == PhotoRoleSidecar {
				return fmt.Errorf("%w: replacement cannot be a sidecar", ErrInvalidPhotoAsset)
			}
			asset.DisplayOverrideFileID = replacement
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_files SET sidecar_of_file_id=NULL WHERE sidecar_of_file_id=?`, fileID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM photo_files WHERE file_id=? AND asset_id=?`, fileID, assetID); err != nil {
			return err
		}
		if asset.DisplayOverrideFileID != nil && *asset.DisplayOverrideFileID == fileID {
			asset.DisplayOverrideFileID = nil
		}
		if asset.DisplayFileID != nil && *asset.DisplayFileID == fileID {
			asset.DisplayFileID = nil
		}
		asset.Files, err = loadPhotoFilesTx(ctx, tx, asset.ID)
		if err != nil {
			return err
		}
		settings, err := photoSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		asset, err = updatePhotoDisplayTx(ctx, tx, asset, settings, nowRFC3339())
		if err != nil {
			return err
		}
		asset.Revision++
		if _, err := tx.ExecContext(ctx, `UPDATE photo_assets SET display_override_file_id=?, revision=?, updated_at=? WHERE asset_id=?`, nullablePhotoString(asset.DisplayOverrideFileID), asset.Revision, nowRFC3339(), asset.ID); err != nil {
			return err
		}
		result, err = loadPhotoAssetTx(ctx, tx, asset.ID)
		if err != nil {
			return err
		}
		if err := writePhotoReceiptTx(ctx, tx, "detach", asset.ID, "", before.Revision, result.Revision, photoAssetState(before), photoAssetState(result)); err != nil {
			return err
		}
		return validatePhotoGraphTx(ctx, tx)
	})
	if err != nil {
		return PhotoAsset{}, err
	}
	return result, nil
}

// SetPhotoAssetExcluded sets or clears an asset's exclusion marker. Arguments
// are revision and a boolean.
func (s *Store) SetPhotoAssetExcluded(ctx context.Context, assetID string, args ...any) (PhotoAsset, error) {
	revision, rest, err := photoRevision(args)
	if err != nil {
		return PhotoAsset{}, err
	}
	if len(rest) == 0 {
		return PhotoAsset{}, fmt.Errorf("%w: excluded flag is required", ErrInvalidPhotoAsset)
	}
	excluded, ok := rest[0].(bool)
	if !ok {
		return PhotoAsset{}, fmt.Errorf("%w: excluded flag must be boolean", ErrInvalidPhotoAsset)
	}
	var result PhotoAsset
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		asset, err := photoAssetForMutationTx(ctx, tx, assetID, revision)
		if err != nil {
			return err
		}
		if (asset.ExcludedAt != nil) == excluded {
			result = asset
			return nil
		}
		before := asset
		var stamp any
		if excluded {
			stamp = nowRFC3339()
		}
		asset.Revision++
		if _, err := tx.ExecContext(ctx, `UPDATE photo_assets SET excluded_at=?, revision=?, updated_at=? WHERE asset_id=?`, stamp, asset.Revision, nowRFC3339(), asset.ID); err != nil {
			return err
		}
		result, err = loadPhotoAssetTx(ctx, tx, asset.ID)
		if err != nil {
			return err
		}
		return writePhotoReceiptTx(ctx, tx, "exclude", asset.ID, "", before.Revision, result.Revision, photoAssetState(before), photoAssetState(result))
	})
	if err != nil {
		return PhotoAsset{}, err
	}
	return result, nil
}

// SetPhotoDisplay sets an asset override. The argument after revision is a
// member file ID, *string, or nil to inherit the vault setting.
func (s *Store) SetPhotoDisplay(ctx context.Context, assetID string, args ...any) (PhotoAsset, error) {
	revision, rest, err := photoRevision(args)
	if err != nil {
		return PhotoAsset{}, err
	}
	var override *string
	if len(rest) > 0 {
		override, err = optionalPhotoString(rest[0])
		if err != nil {
			return PhotoAsset{}, err
		}
	}
	var result PhotoAsset
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		asset, err := photoAssetForMutationTx(ctx, tx, assetID, revision)
		if err != nil {
			return err
		}
		if override != nil {
			var role string
			if err := tx.QueryRowContext(ctx, `SELECT role FROM photo_files WHERE file_id=? AND asset_id=?`, *override, assetID).Scan(&role); err != nil {
				return fmt.Errorf("%w: display override is not an asset member", ErrInvalidPhotoAsset)
			}
			if role == PhotoRoleSidecar {
				return fmt.Errorf("%w: sidecars cannot display", ErrInvalidPhotoAsset)
			}
		}
		if equalPhotoString(asset.DisplayOverrideFileID, override) {
			result = asset
			return nil
		}
		before := asset
		asset.DisplayOverrideFileID = override
		settings, err := photoSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		asset, err = updatePhotoDisplayTx(ctx, tx, asset, settings, nowRFC3339())
		if err != nil {
			return err
		}
		asset.Revision++
		if _, err := tx.ExecContext(ctx, `UPDATE photo_assets SET display_override_file_id=?, revision=?, updated_at=? WHERE asset_id=?`, nullablePhotoString(override), asset.Revision, nowRFC3339(), asset.ID); err != nil {
			return err
		}
		result, err = loadPhotoAssetTx(ctx, tx, asset.ID)
		if err != nil {
			return err
		}
		return writePhotoReceiptTx(ctx, tx, "display", asset.ID, "", before.Revision, result.Revision, photoAssetState(before), photoAssetState(result))
	})
	if err != nil {
		return PhotoAsset{}, err
	}
	return result, nil
}

// SetPhotoSettings changes the vault display preference and atomically fans
// the inherited choice across every asset. Arguments are revision and a raw,
// image, nil, or *string preference.
func (s *Store) SetPhotoSettings(ctx context.Context, args ...any) (PhotoSettings, error) {
	revision, rest, err := photoRevision(args)
	if err != nil {
		return PhotoSettings{}, err
	}
	var preference *string
	if len(rest) > 0 {
		preference, err = photoPreferenceValue(rest[0])
		if err != nil {
			return PhotoSettings{}, err
		}
	}
	var result PhotoSettings
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		current, err := photoSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		if current.Revision != revision {
			return fmt.Errorf("photo settings at revision %d, expected %d: %w", current.Revision, revision, ErrPhotoAssetRevision)
		}
		if equalPhotoString(current.Preference, preference) {
			result = current
			return nil
		}
		var beforeAssets []PhotoAsset
		rows, err := tx.QueryContext(ctx, `SELECT asset_id FROM photo_assets ORDER BY asset_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close() //nolint:sqlclosecheck // close before returning the scan error.
				return err
			}
			asset, err := loadPhotoAssetTx(ctx, tx, id)
			if err != nil {
				_ = rows.Close()
				return err
			}
			beforeAssets = append(beforeAssets, asset)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		newSettings := PhotoSettings{Preference: preference, Revision: current.Revision + 1, UpdatedAt: nowRFC3339()}
		if _, err := tx.ExecContext(ctx, `INSERT INTO photo_library_settings(singleton,preference,revision,updated_at) VALUES(1,?,?,?) ON CONFLICT(singleton) DO UPDATE SET preference=excluded.preference, revision=excluded.revision, updated_at=excluded.updated_at`, nullablePhotoString(preference), newSettings.Revision, newSettings.UpdatedAt); err != nil {
			return err
		}
		if err := writePhotoReceiptTx(ctx, tx, "settings", "", "library", current.Revision, newSettings.Revision, photoSettingsState(current), photoSettingsState(newSettings)); err != nil {
			return err
		}
		for _, before := range beforeAssets {
			asset := before
			choice := selectPhotoDisplay(asset.Files, preference, asset.DisplayOverrideFileID)
			if equalPhotoString(asset.DisplayFileID, choice.FileID) {
				continue
			}
			asset.DisplayFileID = choice.FileID
			asset.DisplaySource = choice.Source
			asset.Revision++
			if _, err := tx.ExecContext(ctx, `UPDATE photo_assets SET display_file_id=?, revision=?, updated_at=? WHERE asset_id=?`, nullablePhotoString(asset.DisplayFileID), asset.Revision, newSettings.UpdatedAt, asset.ID); err != nil {
				return err
			}
			if err := writePhotoReceiptTx(ctx, tx, "settings_recompute", asset.ID, "", before.Revision, asset.Revision, photoAssetState(before), photoAssetState(asset)); err != nil {
				return err
			}
		}
		if err := validatePhotoGraphTx(ctx, tx); err != nil {
			return err
		}
		result = newSettings
		return nil
	})
	if err != nil {
		return PhotoSettings{}, err
	}
	return result, nil
}

// PhotoChangeReceipts returns a bounded newest-first receipt page.
func (s *Store) PhotoChangeReceipts(ctx context.Context, assetID string, limit, offset int) ([]PhotoChangeReceipt, int, error) {
	if limit < 1 || limit > 1000 || offset < 0 {
		return nil, 0, errors.New("photo receipt bounds are invalid")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT receipt_id, operation, COALESCE(asset_id,''), COALESCE(settings_key,''),
		       before_revision, after_revision, before_json, after_json, created_at
		FROM photo_change_receipts WHERE (?='' OR asset_id=?)
		ORDER BY created_at DESC, receipt_id DESC LIMIT ? OFFSET ?`, assetID, assetID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var receipts []PhotoChangeReceipt
	for rows.Next() {
		var receipt PhotoChangeReceipt
		if err := rows.Scan(&receipt.ID, &receipt.Operation, &receipt.AssetID, &receipt.SettingsKey, &receipt.BeforeRevision, &receipt.AfterRevision, &receipt.BeforeJSON, &receipt.AfterJSON, &receipt.CreatedAt); err != nil {
			return nil, 0, err
		}
		receipts = append(receipts, receipt)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts WHERE (?='' OR asset_id=?)`, assetID, assetID).Scan(&total); err != nil {
		return nil, 0, err
	}
	return receipts, total, nil
}
