package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash"
	"path"
	"strings"
	"uuid"

	"golang.org/x/text/unicode/norm"
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
	PickerKind  string
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
	Key     string
	Members []BrowserPhotoGroupMember
	Hashes  []string
	Receipt *PhotoImportResult
}

// BrowserPhotoGroupRef identifies the saved group committed by an upload.
type BrowserPhotoGroupRef struct {
	ImportID uuid.UUID
	GroupKey string
}

func validateBrowserPhotoReceipt(result PhotoImportResult, members int) error {
	if result.Added == result.Skipped || len(result.Nodes) == 0 || (members > 0 && len(result.Nodes) != members) {
		return errors.New("invalid saved browser photo receipt")
	}
	for _, node := range result.Nodes {
		if node.ID <= 0 {
			return errors.New("invalid saved browser photo receipt node")
		}
	}
	if result.Ambiguity != nil && result.Ambiguity.Reason != PhotoImportMultipleRAW && result.Ambiguity.Reason != PhotoImportSeparatePhotos {
		return errors.New("invalid saved browser photo ambiguity")
	}
	return nil
}

// BrowserPhotoGroupReceipt reads a published receipt without waiting for an active upload.
func (s *Store) BrowserPhotoGroupReceipt(ctx context.Context, ref BrowserPhotoGroupRef) (*PhotoImportResult, error) {
	var receipt sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT g.receipt_json FROM browser_photo_import_groups g
		JOIN browser_photo_imports i ON i.import_id=g.import_id
		WHERE g.import_id=$1 AND g.group_key=$2 AND i.vault_id=$3`,
		ref.ImportID.String(), ref.GroupKey, s.VaultID()).Scan(&receipt)
	if err != nil || !receipt.Valid {
		return nil, err
	}
	result := new(PhotoImportResult)
	if err := json.Unmarshal([]byte(receipt.String), result); err != nil {
		return nil, err
	}
	if err := validateBrowserPhotoReceipt(*result, 0); err != nil {
		return nil, err
	}
	return result, nil
}

// ListBrowserPhotoImports lists saved imports in reverse creation order.
func (s *Store) ListBrowserPhotoImports(ctx context.Context, offset, limit int) ([]BrowserPhotoImport, error) {
	if offset < 0 || limit < 1 || limit > 100 {
		return nil, errors.New("invalid browser photo import page")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.import_id,i.source_id,i.picker_kind,i.label,i.destination,i.created_at,
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
		if err := rows.Scan(&id, &source, &item.PickerKind, &item.Label, &item.Destination, &item.CreatedAt, &item.GroupCount); err != nil {
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

// InspectBrowserPhotoImport waits for admitted writers and reads saved facts.
func (s *Store) InspectBrowserPhotoImport(ctx context.Context, id uuid.UUID) (BrowserPhotoImport, error) {
	return s.readBrowserPhotoImport(ctx, id, true)
}

// ReadBrowserPhotoImport reads saved facts without waiting for a writer.
func (s *Store) ReadBrowserPhotoImport(ctx context.Context, id uuid.UUID) (BrowserPhotoImport, error) {
	return s.readBrowserPhotoImport(ctx, id, false)
}

func (s *Store) readBrowserPhotoImport(ctx context.Context, id uuid.UUID, barrier bool) (BrowserPhotoImport, error) {
	var item BrowserPhotoImport
	read := func(tx *sql.Tx) error {
		var source, suffix, skipped string
		var anchor int64
		var destination sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT source_id,picker_kind,label,destination,created_at,anchor_id,suffix_json,destination_id,skipped_json
			FROM browser_photo_imports WHERE import_id=$1 AND vault_id=$2`, id.String(), s.VaultID()).Scan(
			&source, &item.PickerKind, &item.Label, &item.Destination, &item.CreatedAt, &anchor, &suffix, &destination, &skipped); err != nil {
			return err
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
		rows, err := tx.QueryContext(ctx, `SELECT group_key,members_json,hashes_json,receipt_json
			FROM browser_photo_import_groups WHERE import_id=$1 ORDER BY ordinal`, id.String())
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var group BrowserPhotoImportGroup
			var members string
			var hashes, receipt sql.NullString
			if err := rows.Scan(&group.Key, &members, &hashes, &receipt); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(members), &group.Members); err != nil {
				return err
			}
			if group.Key != BrowserPhotoGroupKey(group.Members) {
				return errors.New("invalid saved browser photo group")
			}
			if hashes.Valid {
				if err := json.Unmarshal([]byte(hashes.String), &group.Hashes); err != nil || len(group.Hashes) != len(group.Members) {
					return errors.New("invalid saved browser photo hashes")
				}
			}
			if receipt.Valid {
				group.Receipt = new(PhotoImportResult)
				if err := json.Unmarshal([]byte(receipt.String), group.Receipt); err != nil {
					return err
				}
				if err := validateBrowserPhotoReceipt(*group.Receipt, len(group.Members)); err != nil {
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
		WHERE g.import_id=$1 AND g.group_key=$2 AND i.vault_id=$3`,
		ref.ImportID.String(), ref.GroupKey, s.VaultID()).Scan(
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
	if ref.GroupKey != BrowserPhotoGroupKey(members) || len(members) != len(group.Members) || len(hashes) != len(members) {
		return false, errors.New("browser photo group declaration changed")
	}
	for i, member := range members {
		actual := group.Members[i]
		role, _, _, err := photoImportMemberRole(actual)
		if err != nil || role != member.Role || actual.Name != path.Base(member.Path) || actual.OriginalPath != member.SourcePath ||
			actual.OriginalMtime != member.Mtime || actual.Size != member.Size || actual.BlobHash != hashes[i] {
			return false, errors.New("browser photo group member changed")
		}
	}
	if receiptJSON.Valid {
		if err := json.Unmarshal([]byte(receiptJSON.String), result); err != nil {
			return false, err
		}
		if err := validateBrowserPhotoReceipt(*result, len(members)); err != nil {
			return false, err
		}
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
	receipt, err := json.Marshal(result)
	if err != nil {
		return err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE browser_photo_import_groups SET receipt_json=$1
		WHERE import_id=$2 AND group_key=$3 AND receipt_json IS NULL`, string(receipt), ref.ImportID.String(), ref.GroupKey)
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

// RegisterBrowserPhotoImport saves the complete plan before any upload starts.
func (s *Store) RegisterBrowserPhotoImport(ctx context.Context, plan BrowserPhotoImport) (uuid.UUID, error) {
	if plan.SourceID == uuid.Nil() || len(plan.Groups) == 0 || plan.PickerKind == "" {
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
		_, err := tx.ExecContext(ctx, `INSERT INTO browser_photo_imports
			(import_id, vault_id, source_id, picker_kind, label, destination, anchor_id, suffix_json, skipped_json, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id.String(), s.VaultID(), plan.SourceID.String(),
			plan.PickerKind, plan.Label, plan.Destination, plan.Directory.anchorID, string(segments), string(skipped), nowRFC3339())
		if err != nil {
			return err
		}
		for index, group := range plan.Groups {
			if len(group.Members) == 0 || group.Key != BrowserPhotoGroupKey(group.Members) {
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
				(import_id, group_key, ordinal, members_json) VALUES ($1,$2,$3,$4)`, id.String(), group.Key, index, string(members)); err != nil {
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
func (s *Store) PinBrowserPhotoGroupHashes(ctx context.Context, id uuid.UUID, key string, hashes []string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var membersJSON string
		var pinned sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT members_json, hashes_json FROM browser_photo_import_groups
			WHERE import_id=$1 AND group_key=$2`, id.String(), key).Scan(&membersJSON, &pinned); err != nil {
			return err
		}
		var members []BrowserPhotoGroupMember
		if err := json.Unmarshal([]byte(membersJSON), &members); err != nil {
			return err
		}
		if len(hashes) != len(members) {
			return errors.New("browser photo group hashes do not match members")
		}
		for i, value := range hashes {
			if len(value) != 64 || value != strings.ToLower(value) {
				return errors.New("invalid browser photo group hash")
			}
			if _, err := hex.DecodeString(value); err != nil {
				return errors.New("invalid browser photo group hash")
			}
			if members[i].DeclaredHash != "" && members[i].DeclaredHash != value {
				return errors.New("browser photo group content changed")
			}
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
			WHERE import_id=$2 AND group_key=$3 AND hashes_json IS NULL`, string(encoded), id.String(), key)
		return err
	})
}

// BrowserPhotoGroupKey identifies an ordered group independently of the
// positions assigned to its files by a browser picker.
func BrowserPhotoGroupKey(members []BrowserPhotoGroupMember) string {
	h := sha256.New()
	_, _ = h.Write([]byte("docbank-browser-photo-group-v1\x00"))
	for _, member := range members {
		writeBrowserPhotoKeyPart(h, norm.NFC.String(member.Path))
		writeBrowserPhotoKeyPart(h, member.Role)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeBrowserPhotoKeyPart(h hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = h.Write(length[:])
	_, _ = h.Write([]byte(value))
}
