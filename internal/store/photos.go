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
	files, err := loadPhotoFiles(ctx, q, id)
	if err != nil {
		return PhotoAsset{}, err
	}
	asset.Files = files
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

// photoReceipt is one before/after decision. An asset receipt sets AssetID;
// the settings receipt sets SettingsKey.
type photoReceipt struct {
	Operation      string
	AssetID        string
	SettingsKey    string
	BeforeRevision int64
	AfterRevision  int64
	Before         any
	After          any
}

func writePhotoReceiptTx(ctx context.Context, tx *sql.Tx, receipt photoReceipt) error {
	beforeJSON, err := marshalPhotoState(receipt.Before)
	if err != nil {
		return err
	}
	afterJSON, err := marshalPhotoState(receipt.After)
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
		) VALUES(?,?,?,?,?,?,?,?,?)`, receiptID, receipt.Operation, nullablePhotoText(receipt.AssetID),
		nullablePhotoText(receipt.SettingsKey), receipt.BeforeRevision, receipt.AfterRevision,
		beforeJSON, afterJSON, nowRFC3339()); err != nil {
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

func loadPhotoFiles(ctx context.Context, q metadataQuerier, assetID string) ([]PhotoFile, error) {
	rows, err := q.QueryContext(ctx, `
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

func nullablePhotoString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// insertPhotoFileTx allocates the member's file ID; member.ID is ignored.
func (s *Store) insertPhotoFileTx(ctx context.Context, tx *sql.Tx, member PhotoFile) error {
	if err := photoRoleValidOrError(member.Role); err != nil {
		return err
	}
	fileID, err := newUUIDv4()
	if err != nil {
		return fmt.Errorf("allocating photo file ID: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO photo_files(file_id,asset_id,node_id,role,sidecar_of_file_id,created_at)
		VALUES(?,?,?,?,?,?)`, fileID, member.AssetID, member.NodeID, member.Role,
		nullablePhotoString(member.SidecarOfID), member.CreatedAt); err != nil {
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
	asset, err := photoAssetByIDQuery(ctx, tx, assetID)
	if err != nil {
		return PhotoAsset{}, err
	}
	if asset.Revision != revision {
		return PhotoAsset{}, fmt.Errorf("asset %s at revision %d, expected %d: %w", assetID, asset.Revision, revision, ErrStaleRevision)
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

func (s *Store) photoAssetCreateTx(ctx context.Context, tx *sql.Tx, nodeID int64, explicitRole, explicitKind string) (PhotoAsset, error) {
	now := nowRFC3339()
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
	if kind == "" && role == PhotoRoleVideo {
		kind = PhotoKindVideo
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
	if err := s.insertPhotoFileTx(ctx, tx, PhotoFile{AssetID: assetID, NodeID: node.ID, Role: role, CreatedAt: now}); err != nil {
		return PhotoAsset{}, err
	}
	files, err := loadPhotoFiles(ctx, tx, assetID)
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
		result, err = s.createPhotoAssetWithReceiptTx(ctx, tx, nodeID, role, kind, "create")
		return err
	})
	if err != nil {
		return PhotoAsset{}, err
	}
	return result, nil
}

func (s *Store) createPhotoAssetWithReceiptTx(ctx context.Context, tx *sql.Tx, nodeID int64, role, kind, operation string) (PhotoAsset, error) {
	result, err := s.photoAssetCreateTx(ctx, tx, nodeID, role, kind)
	if err != nil {
		return PhotoAsset{}, err
	}
	changes := photoAssetMemberChanges(PhotoAsset{}, result)
	if err := writePhotoReceiptTx(ctx, tx, photoReceipt{
		Operation: operation, AssetID: result.ID, AfterRevision: result.Revision,
		Before: photoAssetState(PhotoAsset{}, changes), After: photoAssetState(result, changes),
	}); err != nil {
		return PhotoAsset{}, err
	}
	return result, validatePhotoAssetGraph(ctx, tx, result.ID)
}

// photoMutation edits a loaded asset in place. It returns false for a
// canonical no-op, which keeps the revision and writes no receipt.
type photoMutation func(tx *sql.Tx, asset *PhotoAsset) (bool, error)

// mutatePhotoAssetTx applies one revisioned decision to an existing asset:
// check the revision, run the edit, recompute display, advance the revision
// once, write the receipt, and validate the asset's graph.
func (s *Store) mutatePhotoAssetTx(ctx context.Context, tx *sql.Tx, assetID string, revision int64, operation string, edit photoMutation) (PhotoAsset, error) {
	if revision < 1 {
		return PhotoAsset{}, fmt.Errorf("%w: revision must be positive", ErrStaleRevision)
	}
	asset, err := photoAssetForMutationTx(ctx, tx, assetID, revision)
	if err != nil {
		return PhotoAsset{}, err
	}
	before := asset
	changed, err := edit(tx, &asset)
	if err != nil {
		return PhotoAsset{}, err
	}
	if !changed {
		return asset, nil
	}
	result, err := commitPhotoAssetTx(ctx, tx, before, asset, operation)
	if err != nil {
		return PhotoAsset{}, err
	}
	return result, validatePhotoAssetGraph(ctx, tx, result.ID)
}

func (s *Store) mutatePhotoAsset(ctx context.Context, assetID string, revision int64, operation string, edit photoMutation) (PhotoAsset, error) {
	var result PhotoAsset
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.mutatePhotoAssetTx(ctx, tx, assetID, revision, operation, edit)
		return err
	})
	if err != nil {
		return PhotoAsset{}, err
	}
	return result, nil
}

// commitPhotoAssetTx persists an edited asset: it reloads members, drops an
// override that no longer names a member, recomputes the display choice,
// advances the revision once, and records the before and after states.
func commitPhotoAssetTx(ctx context.Context, tx *sql.Tx, before, asset PhotoAsset, operation string) (PhotoAsset, error) {
	files, err := loadPhotoFiles(ctx, tx, asset.ID)
	if err != nil {
		return PhotoAsset{}, err
	}
	asset.Files = files
	if asset.DisplayOverrideFileID != nil {
		if _, ok := photoFileByID(files, *asset.DisplayOverrideFileID); !ok {
			asset.DisplayOverrideFileID = nil
		}
	}
	settings, err := photoSettingsTx(ctx, tx)
	if err != nil {
		return PhotoAsset{}, err
	}
	choice := selectPhotoDisplay(files, settings.Preference, asset.DisplayOverrideFileID)
	asset.DisplayFileID, asset.DisplaySource = choice.FileID, choice.Source
	asset.Revision++
	if _, err := tx.ExecContext(ctx, `
		UPDATE photo_assets SET excluded_at=?, display_file_id=?, display_override_file_id=?,
			revision=?, updated_at=? WHERE asset_id=?`,
		nullablePhotoString(asset.ExcludedAt), nullablePhotoString(asset.DisplayFileID),
		nullablePhotoString(asset.DisplayOverrideFileID), asset.Revision, nowRFC3339(), asset.ID); err != nil {
		return PhotoAsset{}, fmt.Errorf("updating photo asset %s: %w", asset.ID, err)
	}
	result, err := photoAssetByIDQuery(ctx, tx, asset.ID)
	if err != nil {
		return PhotoAsset{}, err
	}
	changes := photoAssetMemberChanges(before, result)
	if err := writePhotoReceiptTx(ctx, tx, photoReceipt{
		Operation: operation, AssetID: asset.ID, BeforeRevision: before.Revision,
		AfterRevision: result.Revision, Before: photoAssetState(before, changes),
		After: photoAssetState(result, changes),
	}); err != nil {
		return PhotoAsset{}, err
	}
	return result, nil
}

func photoAssetOwningNodeTx(ctx context.Context, tx *sql.Tx, nodeID int64) (string, bool, error) {
	var assetID string
	err := tx.QueryRowContext(ctx, `SELECT asset_id FROM photo_files WHERE node_id=?`, nodeID).Scan(&assetID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("checking photo node ownership: %w", err)
	}
	return assetID, true, nil
}

// PromotePhotoNode creates an explicitly requested asset for a live file. An
// already-owned asset requires its current revision and clears exclusion; an
// unowned node starts at revision one without a prior asset revision.
func (s *Store) PromotePhotoNode(ctx context.Context, nodeID int64, expectedRevision *int64, role, kind string) (PhotoAsset, error) {
	if expectedRevision != nil && *expectedRevision < 1 {
		return PhotoAsset{}, fmt.Errorf("%w: revision must be positive", ErrStaleRevision)
	}
	if role != "" && !photoRoleValid(role) {
		return PhotoAsset{}, fmt.Errorf("%w: unknown promote role %q", ErrInvalidPhotoAsset, role)
	}
	if kind != "" && !photoKindValid(kind) {
		return PhotoAsset{}, fmt.Errorf("%w: unknown promote kind %q", ErrInvalidPhotoAsset, kind)
	}
	var result PhotoAsset
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		ownedID, owned, err := photoAssetOwningNodeTx(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		if !owned {
			if expectedRevision != nil {
				return fmt.Errorf("node %d does not own an asset at expected revision %d: %w", nodeID, *expectedRevision, ErrStaleRevision)
			}
			result, err = s.createPhotoAssetWithReceiptTx(ctx, tx, nodeID, role, kind, "promote")
			return err
		}
		if expectedRevision == nil {
			return fmt.Errorf("node %d already belongs to asset %s and needs its revision: %w", nodeID, ownedID, ErrStaleRevision)
		}
		result, err = s.mutatePhotoAssetTx(ctx, tx, ownedID, *expectedRevision, "promote", func(tx *sql.Tx, asset *PhotoAsset) (bool, error) {
			for _, file := range asset.Files {
				if file.NodeID == nodeID && role != "" && file.Role != role {
					return false, fmt.Errorf("%w: existing member role is %s, requested %s", ErrInvalidPhotoAsset, file.Role, role)
				}
			}
			if kind != "" && asset.Kind != kind {
				return false, fmt.Errorf("%w: existing asset kind is %s, requested %s", ErrInvalidPhotoAsset, asset.Kind, kind)
			}
			if _, _, err := photoNodeForMutationTx(tx, nodeID); err != nil {
				return false, err
			}
			if asset.ExcludedAt == nil {
				return false, nil
			}
			asset.ExcludedAt = nil
			return true, nil
		})
		return err
	})
	if err != nil {
		return PhotoAsset{}, err
	}
	return result, nil
}

// AttachPhotoFile adds an ordinary node to an existing asset.
func (s *Store) AttachPhotoFile(ctx context.Context, assetID string, revision, nodeID int64, role string, sidecar *string) (PhotoAsset, error) {
	if nodeID < 1 {
		return PhotoAsset{}, fmt.Errorf("%w: node_id must be positive", ErrInvalidPhotoAsset)
	}
	if role != "" && !photoRoleValid(role) {
		return PhotoAsset{}, fmt.Errorf("%w: unknown role %q", ErrInvalidPhotoAsset, role)
	}
	return s.mutatePhotoAsset(ctx, assetID, revision, "attach", func(tx *sql.Tx, asset *PhotoAsset) (bool, error) {
		node, facts, err := photoNodeForMutationTx(tx, nodeID)
		if err != nil {
			return false, err
		}
		if ownedID, owned, err := photoAssetOwningNodeTx(ctx, tx, nodeID); err != nil {
			return false, err
		} else if owned {
			return false, fmt.Errorf("node %d belongs to asset %s: %w", nodeID, ownedID, ErrPhotoNodeOwned)
		}
		if role == "" {
			if !facts.Qualifies {
				return false, fmt.Errorf("node %d: %w", nodeID, ErrPhotoNodeNotEligible)
			}
			role = inferPhotoRole(facts)
		}
		if err := validatePhotoFileForAsset(asset.Kind, role, facts); err != nil {
			return false, err
		}
		if role == PhotoRoleSidecar {
			if sidecar == nil {
				return false, fmt.Errorf("%w: sidecar requires a raw target", ErrInvalidPhotoAsset)
			}
			target, ok := photoFileByID(asset.Files, *sidecar)
			if !ok {
				return false, fmt.Errorf("%w: sidecar target not found", ErrInvalidPhotoAsset)
			}
			if target.Role != PhotoRoleRAW {
				return false, fmt.Errorf("%w: sidecar target must be same-asset raw", ErrInvalidPhotoAsset)
			}
		} else if sidecar != nil {
			return false, fmt.Errorf("%w: only sidecars may point to raw files", ErrInvalidPhotoAsset)
		}
		return true, s.insertPhotoFileTx(ctx, tx, PhotoFile{
			AssetID: asset.ID, NodeID: node.ID, Role: role, SidecarOfID: sidecar, CreatedAt: nowRFC3339(),
		})
	})
}

// DetachPhotoFile removes one member. Dependent sidecars are removed only
// when ClearDependentSidecars is explicit; otherwise a RAW target is refused.
func (s *Store) DetachPhotoFile(ctx context.Context, assetID string, revision int64, fileID string, options PhotoDetachOptions) (PhotoAsset, error) {
	if fileID == "" {
		return PhotoAsset{}, fmt.Errorf("%w: file_id is required", ErrInvalidPhotoAsset)
	}
	return s.mutatePhotoAsset(ctx, assetID, revision, "detach", func(tx *sql.Tx, asset *PhotoAsset) (bool, error) {
		file, ok := photoFileByID(asset.Files, fileID)
		if !ok {
			return false, ErrNotFound
		}
		if file.Role == PhotoRoleRAW {
			dependents := 0
			for _, member := range asset.Files {
				if member.SidecarOfID != nil && *member.SidecarOfID == fileID {
					dependents++
				}
			}
			if dependents > 0 && !options.ClearDependentSidecars {
				return false, fmt.Errorf("%w: RAW file has dependent sidecars", ErrInvalidPhotoAsset)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM photo_files WHERE asset_id=? AND sidecar_of_file_id=?`, asset.ID, fileID); err != nil {
				return false, err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM photo_files WHERE file_id=? AND asset_id=?`, fileID, asset.ID); err != nil {
			return false, err
		}
		return true, nil
	})
}

// SetPhotoAssetExcluded sets or clears an asset's exclusion marker.
func (s *Store) SetPhotoAssetExcluded(ctx context.Context, assetID string, revision int64, excluded bool) (PhotoAsset, error) {
	return s.mutatePhotoAsset(ctx, assetID, revision, "exclude", func(_ *sql.Tx, asset *PhotoAsset) (bool, error) {
		if (asset.ExcludedAt != nil) == excluded {
			return false, nil
		}
		asset.ExcludedAt = nil
		if excluded {
			asset.ExcludedAt = new(nowRFC3339())
		}
		return true, nil
	})
}

// SetPhotoDisplay sets an asset override. A nil file ID inherits the vault
// setting.
func (s *Store) SetPhotoDisplay(ctx context.Context, assetID string, revision int64, override *string) (PhotoAsset, error) {
	return s.mutatePhotoAsset(ctx, assetID, revision, "display", func(_ *sql.Tx, asset *PhotoAsset) (bool, error) {
		if override != nil {
			file, ok := photoFileByID(asset.Files, *override)
			if !ok {
				return false, fmt.Errorf("%w: display override is not an asset member", ErrInvalidPhotoAsset)
			}
			if file.Role == PhotoRoleSidecar {
				return false, fmt.Errorf("%w: sidecars cannot display", ErrInvalidPhotoAsset)
			}
		}
		if equalPhotoString(asset.DisplayOverrideFileID, override) {
			return false, nil
		}
		asset.DisplayOverrideFileID = override
		return true, nil
	})
}

func photoFileByID(files []PhotoFile, id string) (PhotoFile, bool) {
	for _, file := range files {
		if file.ID == id {
			return file, true
		}
	}
	return PhotoFile{}, false
}

// SetPhotoSettings changes the vault display preference and atomically fans
// the inherited choice across every asset. A nil preference resets to default.
func (s *Store) SetPhotoSettings(ctx context.Context, revision int64, preference *string) (PhotoSettings, error) {
	if revision < 1 {
		return PhotoSettings{}, fmt.Errorf("%w: revision must be positive", ErrStaleRevision)
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
			return fmt.Errorf("photo settings at revision %d, expected %d: %w", current.Revision, revision, ErrStaleRevision)
		}
		if equalPhotoString(current.Preference, preference) {
			result = current
			return nil
		}
		assets, err := allPhotoAssetsTx(ctx, tx)
		if err != nil {
			return err
		}
		next := PhotoSettings{Preference: preference, Revision: current.Revision + 1, UpdatedAt: nowRFC3339()}
		if _, err := tx.ExecContext(ctx, `INSERT INTO photo_library_settings(singleton,preference,revision,updated_at) VALUES(1,?,?,?) ON CONFLICT(singleton) DO UPDATE SET preference=excluded.preference, revision=excluded.revision, updated_at=excluded.updated_at`, nullablePhotoString(preference), next.Revision, next.UpdatedAt); err != nil {
			return err
		}
		if err := writePhotoReceiptTx(ctx, tx, photoReceipt{
			Operation: "settings", SettingsKey: "library", BeforeRevision: current.Revision,
			AfterRevision: next.Revision, Before: photoSettingsState(current),
			After: photoSettingsState(next),
		}); err != nil {
			return err
		}
		for _, asset := range assets {
			choice := selectPhotoDisplay(asset.Files, preference, asset.DisplayOverrideFileID)
			if equalPhotoString(asset.DisplayFileID, choice.FileID) {
				continue
			}
			if _, err := commitPhotoAssetTx(ctx, tx, asset, asset, "settings_recompute"); err != nil {
				return err
			}
		}
		if err := validatePhotoGraph(ctx, tx); err != nil {
			return err
		}
		result = next
		return nil
	})
	if err != nil {
		return PhotoSettings{}, err
	}
	return result, nil
}

func allPhotoAssetsTx(ctx context.Context, tx *sql.Tx) ([]PhotoAsset, error) {
	rows, err := tx.QueryContext(ctx, `SELECT asset_id FROM photo_assets ORDER BY asset_id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close() //nolint:sqlclosecheck // close before returning the scan error.
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	assets := make([]PhotoAsset, 0, len(ids))
	for _, id := range ids {
		asset, err := photoAssetByIDQuery(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		assets = append(assets, asset)
	}
	return assets, nil
}
