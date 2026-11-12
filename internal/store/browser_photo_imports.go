package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path"
	"uuid"
)

// BrowserPhotoGroupMember identifies one member of a saved browser group.
type BrowserPhotoGroupMember struct {
	Path         string `json:"path"`
	Role         string `json:"role"`
	SourcePath   string `json:"source_path"`
	Size         int64  `json:"size"`
	Mtime        string `json:"mtime"`
	DeclaredHash string `json:"declared_hash"`
}

// BrowserPhotoImport is the durable declaration of a browser import.
type BrowserPhotoImport struct {
	ID          uuid.UUID
	SourceID    uuid.UUID
	Label       string
	Destination string
	Directory   IngestDirectoryPlan
	CreatedAt   string
	GroupCount  int
	Groups      []BrowserPhotoImportGroup
	Skipped     []BrowserPhotoSkippedFile
}

// BrowserPhotoSkippedFile records a file excluded by preview planning.
type BrowserPhotoSkippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// BrowserPhotoImportGroup preserves the members and result of one group.
type BrowserPhotoImportGroup struct {
	Members []BrowserPhotoGroupMember
	Hashes  []string
	Receipt *PhotoImportResult
}

// BrowserPhotoGroupRef identifies the saved group committed by an upload.
type BrowserPhotoGroupRef struct {
	ImportID uuid.UUID
	Ordinal  int
}

type browserPhotoReceipt struct {
	Version int    `json:"version"`
	Status  string `json:"status"`
	AssetID string `json:"asset_id,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func decodeBrowserPhotoReceipt(data string) (*PhotoImportResult, error) {
	var receipt browserPhotoReceipt
	if err := json.Unmarshal([]byte(data), &receipt); err != nil {
		return nil, err
	}
	result := &PhotoImportResult{}
	result.Asset.ID = receipt.AssetID
	if receipt.Version != 1 {
		return nil, errors.New("unsupported browser photo receipt version")
	}
	switch receipt.Status {
	case "added":
		result.Added = true
	case "skipped":
		result.Skipped = true
	case "ambiguous":
		if receipt.Reason != PhotoImportMultipleRAW && receipt.Reason != PhotoImportSeparatePhotos {
			return nil, errors.New("invalid browser photo receipt reason")
		}
		result.Added = true
		result.Ambiguity = &PhotoImportAmbiguity{Reason: receipt.Reason}
	default:
		return nil, errors.New("invalid browser photo receipt status")
	}
	if receipt.Status != "ambiguous" && receipt.Reason != "" {
		return nil, errors.New("invalid browser photo receipt reason")
	}
	return result, nil
}

// BrowserPhotoGroupReceipt reads a published receipt without waiting for an active upload.
func (s *Store) BrowserPhotoGroupReceipt(ctx context.Context, ref BrowserPhotoGroupRef) (*PhotoImportResult, error) {
	var receipt sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT g.receipt_json FROM browser_photo_import_groups g
		JOIN browser_photo_imports i ON i.import_id=g.import_id
		WHERE g.import_id=$1 AND g.ordinal=$2 AND i.vault_id=$3 AND i.format_version=1`,
		ref.ImportID.String(), ref.Ordinal, s.VaultID()).Scan(&receipt)
	if err != nil || !receipt.Valid {
		return nil, err
	}
	return decodeBrowserPhotoReceipt(receipt.String)
}

// ErrInvalidBrowserPhotoImportPage identifies invalid saved-import pagination.
var ErrInvalidBrowserPhotoImportPage = errors.New("invalid browser photo import page")

// ListBrowserPhotoImports lists saved imports in reverse creation order.
func (s *Store) ListBrowserPhotoImports(ctx context.Context, offset, limit int) ([]BrowserPhotoImport, error) {
	if offset < 0 || limit < 1 || limit > 100 {
		return nil, ErrInvalidBrowserPhotoImportPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.import_id,i.source_id,i.label,i.destination,i.created_at,
		(SELECT COUNT(*) FROM browser_photo_import_groups g WHERE g.import_id=i.import_id)
		FROM browser_photo_imports i WHERE i.vault_id=$1 ORDER BY i.created_at DESC,i.import_id DESC LIMIT $2 OFFSET $3`,
		s.VaultID(), limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var imports []BrowserPhotoImport
	for rows.Next() {
		var item BrowserPhotoImport
		var id, source string
		if err := rows.Scan(&id, &source, &item.Label, &item.Destination, &item.CreatedAt, &item.GroupCount); err != nil {
			return nil, err
		}
		item.ID, err = uuid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("parse saved browser photo import ID: %w", err)
		}
		item.SourceID, err = uuid.Parse(source)
		if err != nil {
			return nil, fmt.Errorf("parse saved browser photo source ID: %w", err)
		}
		imports = append(imports, item)
	}
	return imports, rows.Err()
}

// BrowserPhotoImportIDForSource resolves a checkpoint to its saved import.
func (s *Store) BrowserPhotoImportIDForSource(ctx context.Context, source uuid.UUID) (uuid.UUID, error) {
	var text string
	err := s.db.QueryRowContext(ctx, `SELECT import_id FROM browser_photo_imports WHERE vault_id=$1 AND source_id=$2`, s.VaultID(), source.String()).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil(), ErrNotFound
	}
	if err != nil {
		return uuid.Nil(), err
	}
	id, err := uuid.Parse(text)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("parse saved browser photo import ID: %w", err)
	}
	return id, nil
}

// InspectBrowserPhotoImport waits for admitted writers and reads saved facts.
func (s *Store) InspectBrowserPhotoImport(ctx context.Context, id uuid.UUID) (BrowserPhotoImport, error) {
	return s.readBrowserPhotoImport(ctx, id, true)
}

// BrowserPhotoImportRun reconstructs the authority-free run for a saved import.
func (s *Store) BrowserPhotoImportRun(ctx context.Context, id uuid.UUID) (IngestRun, error) {
	record := metadataIngest{Type: metadataIngestType, ID: id.String(), SourceKind: "photo-import"}
	err := s.db.QueryRowContext(ctx, `SELECT created_at,label FROM browser_photo_imports WHERE import_id=$1 AND vault_id=$2`,
		id.String(), s.VaultID()).Scan(&record.StartedAt, &record.SourceDesc)
	if err != nil {
		return IngestRun{}, fmt.Errorf("reading browser photo import run: %w", err)
	}
	if err := validateIngestRecord(record); err != nil {
		return IngestRun{}, fmt.Errorf("validating browser photo import run: %w", err)
	}
	return IngestRun{record: record}, nil
}

// ReadBrowserPhotoImport reads saved facts without waiting for a writer.
func (s *Store) ReadBrowserPhotoImport(ctx context.Context, id uuid.UUID) (BrowserPhotoImport, error) {
	return s.readBrowserPhotoImport(ctx, id, false)
}

func (s *Store) readBrowserPhotoImport(ctx context.Context, id uuid.UUID, barrier bool) (BrowserPhotoImport, error) {
	var item BrowserPhotoImport
	read := func(tx *sql.Tx) error {
		var source, suffix, skipped string
		var anchor, formatVersion int64
		var destination sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT source_id,label,destination,created_at,anchor_id,suffix_json,destination_id,skipped_json,format_version
			FROM browser_photo_imports WHERE import_id=$1 AND vault_id=$2`, id.String(), s.VaultID()).Scan(
			&source, &item.Label, &item.Destination, &item.CreatedAt, &anchor, &suffix, &destination, &skipped, &formatVersion); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if formatVersion != 1 {
			return errors.New("unsupported browser photo import version")
		}
		var err error
		item.ID = id
		item.SourceID, err = uuid.Parse(source)
		if err != nil {
			return fmt.Errorf("parse saved browser photo source ID: %w", err)
		}
		item.Directory.anchorID = anchor
		if err := json.Unmarshal([]byte(suffix), &item.Directory.segments); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(skipped), &item.Skipped); err != nil {
			return err
		}
		if destination.Valid {
			item.Directory = IngestDirectoryPlan{anchorID: destination.Int64}
		}
		rows, err := tx.QueryContext(ctx, `SELECT members_json,hashes_json,receipt_json
			FROM browser_photo_import_groups WHERE import_id=$1 ORDER BY ordinal`, id.String())
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var group BrowserPhotoImportGroup
			var members string
			var hashes, receipt sql.NullString
			if err := rows.Scan(&members, &hashes, &receipt); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(members), &group.Members); err != nil {
				return err
			}
			if hashes.Valid {
				if err := json.Unmarshal([]byte(hashes.String), &group.Hashes); err != nil || len(group.Hashes) != len(group.Members) {
					return errors.New("invalid saved browser photo hashes")
				}
			}
			if receipt.Valid {
				group.Receipt, err = decodeBrowserPhotoReceipt(receipt.String)
				if err != nil {
					return err
				}
			}
			item.Groups = append(item.Groups, group)
		}
		item.GroupCount = len(item.Groups)
		return rows.Err()
	}
	var err error
	if barrier {
		err = s.withBookkeepingTx(ctx, read)
	} else {
		err = s.withReadTx(ctx, read)
	}
	return item, err
}

func (s *Store) prepareBrowserPhotoGroupTx(
	ctx context.Context, tx *sql.Tx, group *PhotoImportGroup, result *PhotoImportResult,
) (bool, error) {
	ref := group.BrowserGroup
	var membersJSON, suffixJSON string
	var hashesJSON, receiptJSON sql.NullString
	var anchorID int64
	var destinationID sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT g.members_json,g.hashes_json,g.receipt_json,
		i.anchor_id,i.suffix_json,i.destination_id FROM browser_photo_import_groups g
		JOIN browser_photo_imports i ON i.import_id=g.import_id
		WHERE g.import_id=$1 AND g.ordinal=$2 AND i.vault_id=$3 AND i.format_version=1`,
		ref.ImportID.String(), ref.Ordinal, s.VaultID()).Scan(
		&membersJSON, &hashesJSON, &receiptJSON, &anchorID, &suffixJSON, &destinationID)
	if err != nil {
		return false, err
	}
	var members []BrowserPhotoGroupMember
	var hashes []string
	if err := json.Unmarshal([]byte(membersJSON), &members); err != nil {
		return false, err
	}
	if !hashesJSON.Valid {
		return false, errors.New("browser photo group hashes are not pinned")
	}
	if err := json.Unmarshal([]byte(hashesJSON.String), &hashes); err != nil {
		return false, err
	}
	if len(members) != len(group.Members) || len(hashes) != len(members) {
		return false, errors.New("browser photo group declaration changed")
	}
	for i, member := range members {
		actual := group.Members[i]
		role, _, _, err := photoImportMemberRole(actual)
		name, nameErr := NormalizeName(path.Base(member.Path))
		if err != nil || nameErr != nil || role != member.Role || actual.Name != name || actual.OriginalPath != member.SourcePath ||
			actual.OriginalMtime != member.Mtime || actual.Size != member.Size || actual.BlobHash != hashes[i] {
			return false, errors.New("browser photo group member changed")
		}
	}
	if receiptJSON.Valid {
		saved, err := decodeBrowserPhotoReceipt(receiptJSON.String)
		if err != nil {
			return false, err
		}
		*result = *saved
		return true, nil
	}
	var suffix []string
	if err := json.Unmarshal([]byte(suffixJSON), &suffix); err != nil {
		return false, err
	}
	if destinationID.Valid {
		anchorID, suffix = destinationID.Int64, nil
	}
	directory := IngestDirectoryPlan{anchorID: anchorID, segments: suffix}
	group.DestinationPlan = &directory
	return false, nil
}

func saveBrowserPhotoReceiptTx(ctx context.Context, tx *sql.Tx, ref BrowserPhotoGroupRef, result PhotoImportResult) error {
	saved := browserPhotoReceipt{Version: 1, Status: "skipped", AssetID: result.Asset.ID}
	if result.Ambiguity != nil {
		saved.Status, saved.Reason = "ambiguous", result.Ambiguity.Reason
	} else if result.Added {
		saved.Status = "added"
	}
	receipt, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE browser_photo_import_groups SET receipt_json=$1
		WHERE import_id=$2 AND ordinal=$3 AND receipt_json IS NULL`, string(receipt), ref.ImportID.String(), ref.Ordinal)
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("browser photo receipt was not saved")
	}
	return nil
}

func sameBrowserPhotoMembers(encoded string, proposed []BrowserPhotoGroupMember) bool {
	var existing []BrowserPhotoGroupMember
	if json.Unmarshal([]byte(encoded), &existing) != nil || len(existing) != len(proposed) {
		return false
	}
	for i := range existing {
		if existing[i].DeclaredHash == "" {
			existing[i].DeclaredHash = proposed[i].DeclaredHash
		}
		if existing[i] != proposed[i] {
			return false
		}
	}
	return true
}

// RegisterBrowserPhotoImport saves the complete plan before any upload starts.
func (s *Store) RegisterBrowserPhotoImport(ctx context.Context, plan BrowserPhotoImport) (uuid.UUID, error) {
	if plan.SourceID == uuid.Nil() || len(plan.Groups) == 0 {
		return uuid.Nil(), errors.New("invalid browser photo import declaration")
	}
	if err := validateIngestDirectoryPlan(plan.Directory); err != nil {
		return uuid.Nil(), err
	}
	segments, err := json.Marshal(plan.Directory.segments)
	if err != nil {
		return uuid.Nil(), err
	}
	skipped, err := json.Marshal(plan.Skipped)
	if err != nil {
		return uuid.Nil(), err
	}
	id := uuid.New()
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		inserted, err := tx.ExecContext(ctx, `INSERT INTO browser_photo_imports
			(import_id, vault_id, source_id, label, destination, anchor_id, suffix_json, skipped_json, created_at, format_version)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,1) ON CONFLICT (vault_id,source_id) DO NOTHING`, id.String(), s.VaultID(), plan.SourceID.String(),
			plan.Label, plan.Destination, plan.Directory.anchorID, string(segments), string(skipped), nowRFC3339())
		if err != nil {
			return err
		}
		count, err := inserted.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			var existingID, label, destination, existingSkipped string
			var version int64
			if err := tx.QueryRowContext(ctx, `SELECT import_id,label,destination,skipped_json,format_version FROM browser_photo_imports WHERE vault_id=$1 AND source_id=$2`, s.VaultID(), plan.SourceID.String()).Scan(&existingID, &label, &destination, &existingSkipped, &version); err != nil {
				return err
			}
			if version != 1 || label != plan.Label || destination != plan.Destination || existingSkipped != string(skipped) {
				return errors.New("browser photo import declaration changed")
			}
			rows, err := tx.QueryContext(ctx, `SELECT members_json FROM browser_photo_import_groups WHERE import_id=$1 ORDER BY ordinal`, existingID)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			index := 0
			for rows.Next() {
				var existing string
				if err := rows.Scan(&existing); err != nil {
					return err
				}
				if index >= len(plan.Groups) {
					return errors.New("browser photo import declaration changed")
				}
				if !sameBrowserPhotoMembers(existing, plan.Groups[index].Members) {
					return errors.New("browser photo import declaration changed")
				}
				index++
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if index != len(plan.Groups) {
				return errors.New("browser photo import declaration changed")
			}
			id, err = uuid.Parse(existingID)
			if err != nil {
				return fmt.Errorf("parse saved browser photo import ID: %w", err)
			}
			return nil
		}
		for index, group := range plan.Groups {
			if len(group.Members) == 0 {
				return errors.New("invalid browser photo group")
			}
			for _, member := range group.Members {
				if member.Path == "" || member.SourcePath == "" || member.Size < 0 || !photoRoleValid(member.Role) {
					return errors.New("invalid browser photo group member")
				}
			}
			members, err := json.Marshal(group.Members)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO browser_photo_import_groups
				(import_id, ordinal, members_json) VALUES ($1,$2,$3)`, id.String(), index, string(members)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return uuid.Nil(), fmt.Errorf("registering browser photo import: %w", err)
	}
	return id, nil
}

// PinBrowserPhotoGroupHashes binds all members to their first attempted bytes.
func (s *Store) PinBrowserPhotoGroupHashes(ctx context.Context, id uuid.UUID, ordinal int, hashes []string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var membersJSON string
		var pinned sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT g.members_json, g.hashes_json FROM browser_photo_import_groups g JOIN browser_photo_imports i ON i.import_id=g.import_id
			WHERE g.import_id=$1 AND g.ordinal=$2 AND i.format_version=1`, id.String(), ordinal).Scan(&membersJSON, &pinned); err != nil {
			return err
		}
		var members []BrowserPhotoGroupMember
		if err := json.Unmarshal([]byte(membersJSON), &members); err != nil {
			return err
		}
		if len(hashes) != len(members) {
			return errors.New("browser photo group hashes do not match members")
		}
		encoded, err := json.Marshal(hashes)
		if err != nil {
			return err
		}
		if pinned.Valid {
			if pinned.String != string(encoded) {
				return errors.New("browser photo group content changed")
			}
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE browser_photo_import_groups SET hashes_json=$1
			WHERE import_id=$2 AND ordinal=$3 AND hashes_json IS NULL`, string(encoded), id.String(), ordinal)
		return err
	})
}
