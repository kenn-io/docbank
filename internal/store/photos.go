package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
)

const maxPhotoReceiptBytes = 16 << 10

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

func (s *Store) photoReadTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("beginning photo read transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing photo read transaction: %w", err)
	}
	return nil
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
	var asset PhotoAsset
	if err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		asset, err = photoAssetByIDQuery(ctx, tx, id)
		return err
	}); err != nil {
		return PhotoAsset{}, err
	}
	return asset, nil
}

// PhotoAssetForNode resolves the asset containing one ordinary file node.
func (s *Store) PhotoAssetForNode(ctx context.Context, nodeID int64) (PhotoAsset, error) {
	if nodeID < 1 {
		return PhotoAsset{}, fmt.Errorf("invalid photo node: %w", ErrNotFound)
	}
	var asset PhotoAsset
	if err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		var id string
		if err := tx.QueryRowContext(ctx, `SELECT asset_id FROM photo_files WHERE node_id=?`, nodeID).Scan(&id); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("finding photo asset for node %d: %w", nodeID, err)
		}
		var err error
		asset, err = photoAssetByIDQuery(ctx, tx, id)
		return err
	}); err != nil {
		return PhotoAsset{}, err
	}
	return asset, nil
}

// PhotoSettings returns the virtual or stored vault preference.
func (s *Store) PhotoSettings(ctx context.Context) (PhotoSettings, error) {
	var settings PhotoSettings
	if err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		settings, err = photoSettingsTx(ctx, tx)
		return err
	}); err != nil {
		return PhotoSettings{}, err
	}
	return settings, nil
}

const maxPhotoReceiptMemberChanges = 32

type photoReceiptMember struct {
	FileID      string  `json:"file_id"`
	NodeID      int64   `json:"node_id"`
	Role        string  `json:"role"`
	SidecarOfID *string `json:"sidecar_of_file_id,omitzero"`
}

type photoReceiptMemberChange struct {
	FileID string              `json:"file_id"`
	Before *photoReceiptMember `json:"before,omitzero"`
	After  *photoReceiptMember `json:"after,omitzero"`
}

type photoReceiptAssetState struct {
	ID                    string                     `json:"id"`
	Kind                  string                     `json:"kind"`
	Revision              int64                      `json:"revision"`
	ExcludedAt            *string                    `json:"excluded_at"`
	DisplayFileID         *string                    `json:"display_file_id"`
	DisplayOverrideFileID *string                    `json:"display_override_file_id"`
	FileCount             int                        `json:"file_count"`
	ChangedMemberCount    int                        `json:"changed_member_count,omitzero"`
	MemberChanges         []photoReceiptMemberChange `json:"member_changes,omitzero"`
	ChangesTruncated      bool                       `json:"changes_truncated,omitzero"`
}

func photoReceiptMemberValue(file PhotoFile) *photoReceiptMember {
	return &photoReceiptMember{FileID: file.ID, NodeID: file.NodeID, Role: file.Role, SidecarOfID: file.SidecarOfID}
}

func photoAssetMemberChanges(before, after PhotoAsset) []photoReceiptMemberChange {
	beforeByID := make(map[string]PhotoFile, len(before.Files))
	afterByID := make(map[string]PhotoFile, len(after.Files))
	ids := make(map[string]struct{}, len(before.Files)+len(after.Files))
	for _, file := range before.Files {
		beforeByID[file.ID] = file
		ids[file.ID] = struct{}{}
	}
	for _, file := range after.Files {
		afterByID[file.ID] = file
		ids[file.ID] = struct{}{}
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	changes := make([]photoReceiptMemberChange, 0, len(ordered))
	for _, id := range ordered {
		beforeFile, hadBefore := beforeByID[id]
		afterFile, hadAfter := afterByID[id]
		if hadBefore && hadAfter && beforeFile.NodeID == afterFile.NodeID && beforeFile.Role == afterFile.Role && equalPhotoString(beforeFile.SidecarOfID, afterFile.SidecarOfID) {
			continue
		}
		change := photoReceiptMemberChange{FileID: id}
		if hadBefore {
			change.Before = photoReceiptMemberValue(beforeFile)
		}
		if hadAfter {
			change.After = photoReceiptMemberValue(afterFile)
		}
		changes = append(changes, change)
	}
	return changes
}

func photoAssetState(asset PhotoAsset, changes []photoReceiptMemberChange) any {
	state := photoReceiptAssetState{
		ID: asset.ID, Kind: asset.Kind, Revision: asset.Revision,
		ExcludedAt: asset.ExcludedAt, DisplayFileID: asset.DisplayFileID,
		DisplayOverrideFileID: asset.DisplayOverrideFileID, FileCount: len(asset.Files),
		ChangedMemberCount: len(changes),
	}
	if len(changes) > maxPhotoReceiptMemberChanges {
		state.ChangesTruncated = true
		changes = changes[:maxPhotoReceiptMemberChanges]
	}
	state.MemberChanges = changes
	return state
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

func (s *Store) insertPhotoFileTx(ctx context.Context, tx *sql.Tx, assetID string, nodeID int64, role string, sidecarOf *string, createdAt string) error {
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
		return s.classifyPhotoFileInsertError(err)
	}
	return nil
}

func photoRoleValidOrError(role string) error {
	if !photoRoleValid(role) {
		return fmt.Errorf("%w: unknown role %q", ErrInvalidPhotoAsset, role)
	}
	return nil
}

func (s *Store) classifyPhotoFileInsertError(err error) error {
	if s.driver.IsUniqueViolation(err) {
		return fmt.Errorf("%w: node is already owned or file identity conflicts", ErrPhotoNodeOwned)
	}
	return fmt.Errorf("creating photo file: %w", err)
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

func (s *Store) photoAssetCreateTx(ctx context.Context, tx *sql.Tx, nodeID int64, explicitRole, explicitKind string, now string) (PhotoAsset, error) {
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
	if err := validatePhotoFileForAsset(kind, role, facts); err != nil {
		return PhotoAsset{}, err
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
	if err := s.insertPhotoFileTx(ctx, tx, assetID, node.ID, role, nil, now); err != nil {
		return PhotoAsset{}, err
	}
	files, err := loadPhotoFilesTx(ctx, tx, assetID)
	if err != nil {
		return PhotoAsset{}, err
	}
	asset := PhotoAsset{ID: assetID, Kind: kind, Revision: 1, CreatedAt: now, UpdatedAt: now, Files: files}
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

// CreatePhotoAsset creates a singleton asset for one eligible node. A role and
// kind are optional only for callers that explicitly classify a RAW node.
func (s *Store) CreatePhotoAsset(ctx context.Context, nodeID int64, role, kind string) (PhotoAsset, error) {
	if role != "" && !photoRoleValid(role) {
		return PhotoAsset{}, fmt.Errorf("%w: unknown create role %q", ErrInvalidPhotoAsset, role)
	}
	if kind != "" && !photoKindValid(kind) {
		return PhotoAsset{}, fmt.Errorf("%w: unknown create kind %q", ErrInvalidPhotoAsset, kind)
	}
	var result PhotoAsset
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.photoAssetCreateTx(ctx, tx, nodeID, role, kind, nowRFC3339())
		if err != nil {
			return err
		}
		changes := photoAssetMemberChanges(PhotoAsset{}, result)
		if err := writePhotoReceiptTx(ctx, tx, "create", result.ID, "", 0, result.Revision, photoAssetState(PhotoAsset{}, changes), photoAssetState(result, changes)); err != nil {
			return err
		}
		return validatePhotoGraphTx(ctx, tx)
	})
	if err != nil {
		return PhotoAsset{}, err
	}
	return result, nil
}

// PromotePhotoNode creates an explicitly requested asset for a live file. An
// already-owned excluded asset requires its current revision; an unowned node
// starts at revision one without a prior asset revision.
func (s *Store) PromotePhotoNode(ctx context.Context, nodeID int64, expectedRevision *int64, role, kind string) (PhotoAsset, error) {
	if expectedRevision != nil && *expectedRevision < 1 {
		return PhotoAsset{}, fmt.Errorf("%w: revision must be positive", ErrPhotoAssetRevision)
	}
	if role != "" && !photoRoleValid(role) {
		return PhotoAsset{}, fmt.Errorf("%w: unknown promote role %q", ErrInvalidPhotoAsset, role)
	}
	if kind != "" && !photoKindValid(kind) {
		return PhotoAsset{}, fmt.Errorf("%w: unknown promote kind %q", ErrInvalidPhotoAsset, kind)
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
			_, facts, factsErr := photoNodeForMutationTx(tx, nodeID)
			if factsErr != nil {
				return factsErr
			}
			if role != "" || kind != "" {
				if role != "" {
					if err := validatePhotoRoleForNode(role, facts); err != nil {
						return err
					}
				}
				if kind != "" && facts.Qualifies && facts.AssetKind != kind {
					return fmt.Errorf("%w: node media does not match asset kind", ErrInvalidPhotoAsset)
				}
			}
			if asset.ExcludedAt == nil {
				result = asset
				return nil
			}
			if expectedRevision == nil || asset.Revision != *expectedRevision {
				want := int64(0)
				if expectedRevision != nil {
					want = *expectedRevision
				}
				return fmt.Errorf("asset %s at revision %d, expected %d: %w", asset.ID, asset.Revision, want, ErrPhotoAssetRevision)
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
			changes := photoAssetMemberChanges(before, result)
			return writePhotoReceiptTx(ctx, tx, "promote", asset.ID, "", before.Revision,
				result.Revision, photoAssetState(before, changes), photoAssetState(result, changes))
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("checking photo node ownership: %w", err)
		}
		var err error
		result, err = s.photoAssetCreateTx(ctx, tx, nodeID, role, kind, nowRFC3339())
		if err != nil {
			return err
		}
		changes := photoAssetMemberChanges(PhotoAsset{}, result)
		if err := writePhotoReceiptTx(ctx, tx, "promote", result.ID, "", 0, result.Revision, photoAssetState(PhotoAsset{}, changes), photoAssetState(result, changes)); err != nil {
			return err
		}
		return validatePhotoGraphTx(ctx, tx)
	})
	if err != nil {
		return PhotoAsset{}, err
	}
	return result, nil
}

// AttachPhotoFile adds an ordinary node to an existing asset.
func (s *Store) AttachPhotoFile(ctx context.Context, assetID string, revision, nodeID int64, role string, sidecar *string) (PhotoAsset, error) {
	if revision < 1 {
		return PhotoAsset{}, fmt.Errorf("%w: revision must be positive", ErrPhotoAssetRevision)
	}
	if nodeID < 1 {
		return PhotoAsset{}, fmt.Errorf("%w: node_id must be positive", ErrInvalidPhotoAsset)
	}
	if role != "" && !photoRoleValid(role) {
		return PhotoAsset{}, fmt.Errorf("%w: unknown role %q", ErrInvalidPhotoAsset, role)
	}
	var result PhotoAsset
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
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
			if !facts.Qualifies {
				return fmt.Errorf("node %d: %w", nodeID, ErrPhotoNodeNotEligible)
			}
			role = inferPhotoRole(facts)
		}
		if err := validatePhotoFileForAsset(asset.Kind, role, facts); err != nil {
			return err
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
		if err := s.insertPhotoFileTx(ctx, tx, asset.ID, node.ID, role, sidecar, nowRFC3339()); err != nil {
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
		changes := photoAssetMemberChanges(before, result)
		if err := writePhotoReceiptTx(ctx, tx, "attach", asset.ID, "", before.Revision, result.Revision, photoAssetState(before, changes), photoAssetState(result, changes)); err != nil {
			return err
		}
		return validatePhotoGraphTx(ctx, tx)
	})
	if err != nil {
		return PhotoAsset{}, err
	}
	return result, nil
}

// DetachPhotoFile removes one member. Dependent sidecars are removed only
// when ClearDependentSidecars is explicit; otherwise a RAW target is refused.
func (s *Store) DetachPhotoFile(ctx context.Context, assetID string, revision int64, fileID string, options PhotoDetachOptions) (PhotoAsset, error) {
	if revision < 1 {
		return PhotoAsset{}, fmt.Errorf("%w: revision must be positive", ErrPhotoAssetRevision)
	}
	if fileID == "" {
		return PhotoAsset{}, fmt.Errorf("%w: file_id is required", ErrInvalidPhotoAsset)
	}
	var result PhotoAsset
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
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
		if options.ReplacementFileID != nil {
			if *options.ReplacementFileID == fileID {
				return fmt.Errorf("%w: replacement file is being detached", ErrInvalidPhotoAsset)
			}
			var role string
			if err := tx.QueryRowContext(ctx, `SELECT role FROM photo_files WHERE file_id=? AND asset_id=?`, *options.ReplacementFileID, assetID).Scan(&role); err != nil {
				return fmt.Errorf("%w: replacement file is not a member", ErrInvalidPhotoAsset)
			}
			if role == PhotoRoleSidecar {
				return fmt.Errorf("%w: replacement cannot be a sidecar", ErrInvalidPhotoAsset)
			}
			asset.DisplayOverrideFileID = options.ReplacementFileID
		}
		if file.Role == PhotoRoleRAW {
			var dependents int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_files WHERE asset_id=? AND sidecar_of_file_id=?`, assetID, fileID).Scan(&dependents); err != nil {
				return err
			}
			if dependents > 0 && !options.ClearDependentSidecars {
				return fmt.Errorf("%w: RAW file has dependent sidecars", ErrInvalidPhotoAsset)
			}
			if options.ClearDependentSidecars {
				if _, err := tx.ExecContext(ctx, `DELETE FROM photo_files WHERE asset_id=? AND sidecar_of_file_id=?`, assetID, fileID); err != nil {
					return err
				}
			}
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
		changes := photoAssetMemberChanges(before, result)
		if err := writePhotoReceiptTx(ctx, tx, "detach", asset.ID, "", before.Revision, result.Revision, photoAssetState(before, changes), photoAssetState(result, changes)); err != nil {
			return err
		}
		return validatePhotoGraphTx(ctx, tx)
	})
	if err != nil {
		return PhotoAsset{}, err
	}
	return result, nil
}

// SetPhotoAssetExcluded sets or clears an asset's exclusion marker.
func (s *Store) SetPhotoAssetExcluded(ctx context.Context, assetID string, revision int64, excluded bool) (PhotoAsset, error) {
	if revision < 1 {
		return PhotoAsset{}, fmt.Errorf("%w: revision must be positive", ErrPhotoAssetRevision)
	}
	var result PhotoAsset
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
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
		changes := photoAssetMemberChanges(before, result)
		return writePhotoReceiptTx(ctx, tx, "exclude", asset.ID, "", before.Revision, result.Revision, photoAssetState(before, changes), photoAssetState(result, changes))
	})
	if err != nil {
		return PhotoAsset{}, err
	}
	return result, nil
}

// SetPhotoDisplay sets an asset override. A nil file ID inherits the vault
// setting.
func (s *Store) SetPhotoDisplay(ctx context.Context, assetID string, revision int64, override *string) (PhotoAsset, error) {
	if revision < 1 {
		return PhotoAsset{}, fmt.Errorf("%w: revision must be positive", ErrPhotoAssetRevision)
	}
	var result PhotoAsset
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
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
		changes := photoAssetMemberChanges(before, result)
		return writePhotoReceiptTx(ctx, tx, "display", asset.ID, "", before.Revision, result.Revision, photoAssetState(before, changes), photoAssetState(result, changes))
	})
	if err != nil {
		return PhotoAsset{}, err
	}
	return result, nil
}

// SetPhotoSettings changes the vault display preference and atomically fans
// the inherited choice across every asset. A nil preference resets to default.
func (s *Store) SetPhotoSettings(ctx context.Context, revision int64, preference *string) (PhotoSettings, error) {
	if revision < 1 {
		return PhotoSettings{}, fmt.Errorf("%w: revision must be positive", ErrPhotoAssetRevision)
	}
	if !photoPreferenceValid(preference) {
		return PhotoSettings{}, fmt.Errorf("%w: invalid preference", ErrInvalidPhotoAsset)
	}
	var result PhotoSettings
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
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
			changes := photoAssetMemberChanges(before, asset)
			if err := writePhotoReceiptTx(ctx, tx, "settings_recompute", asset.ID, "", before.Revision, asset.Revision, photoAssetState(before, changes), photoAssetState(asset, changes)); err != nil {
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
	var receipts []PhotoChangeReceipt
	var total int
	if err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT receipt_id, operation, COALESCE(asset_id,''), COALESCE(settings_key,''),
			       before_revision, after_revision, before_json, after_json, created_at
			FROM photo_change_receipts WHERE (?='' OR asset_id=?)
			ORDER BY created_at DESC, receipt_id DESC LIMIT ? OFFSET ?`, assetID, assetID, limit, offset)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var receipt PhotoChangeReceipt
			if err := rows.Scan(&receipt.ID, &receipt.Operation, &receipt.AssetID, &receipt.SettingsKey, &receipt.BeforeRevision, &receipt.AfterRevision, &receipt.BeforeJSON, &receipt.AfterJSON, &receipt.CreatedAt); err != nil {
				return err
			}
			receipts = append(receipts, receipt)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts WHERE (?='' OR asset_id=?)`, assetID, assetID).Scan(&total)
	}); err != nil {
		return nil, 0, err
	}
	return receipts, total, nil
}
