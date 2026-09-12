package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
)

type metadataMailboxContainer struct {
	Type      string           `json:"type"`
	Container MailboxContainer `json:"container"`
}

func validateRetainedMailboxContainer(ctx context.Context, q metadataQuerier, c MailboxContainer) error {
	h, err := mailboxManifest(c)
	if err != nil {
		return err
	}
	if c.State != "sealed" || c.ManifestSHA256 != h {
		return ErrMailboxInvalid
	}
	if err = validateMetadataTime("mailbox container created_at", c.CreatedAt); err != nil {
		return err
	}
	for _, ch := range c.Chunks {
		var size int64
		if err = q.QueryRowContext(ctx, `SELECT size FROM blobs WHERE hash=?`, ch.SHA256).Scan(&size); err != nil {
			return err
		}
		if size != ch.Size {
			return ErrMailboxInvalid
		}
	}
	return nil
}
func exportMailboxMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	after := ""
	for {
		var id, owner string
		err := q.QueryRowContext(ctx, `SELECT id,owner FROM mailbox_containers WHERE id>? ORDER BY id LIMIT 1`, after).Scan(&id, &owner)
		if errors.Is(err, sql.ErrNoRows) {
			return exportMailboxTransfers(ctx, q, write)
		}
		if err != nil {
			return err
		}
		c, err := loadMailboxContainer(ctx, q, owner, id)
		if err != nil {
			return err
		}
		after = id
		if c.State == "uploading" {
			continue // Valid transient uploads are not portable authority.
		}
		if err = validateRetainedMailboxContainer(ctx, q, c); err != nil {
			return err
		}
		if err = write(metadataMailboxContainer{Type: "mailbox_container", Container: c}); err != nil {
			return err
		}
	}
}
func importMailboxMetadataRecord(ctx context.Context, tx *sql.Tx, kind string, raw jsontext.Value) error {
	switch kind {
	case "mailbox_job":
		var r metadataMailboxJob
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		return saveMailboxJob(ctx, tx, r.Job, true)
	case "mailbox_occurrence":
		var r metadataMailboxOccurrence
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		return insertMailboxOccurrence(ctx, tx, r.Occurrence)
	case "mailbox_archive":
		var r metadataMailboxArchive
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		a := r.Archive
		if !mailboxText(a.ID, 128) || !mailboxText(a.Owner, 256) || !mailboxText(a.Description, 1024) {
			return ErrMailboxInvalid
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO mailbox_archives(id,owner,description) VALUES(?,?,?)`, a.ID, a.Owner, a.Description)
		return err
	case "mailbox_transfer_receipt":
		var r metadataMailboxTransfer
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		return insertMailboxTransferReceipt(ctx, tx, r.Receipt)
	case "mailbox_transfer_head":
		var r metadataMailboxTransferHead
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO mailbox_transfer_heads(archive_id,source_ref,receipt_id) VALUES(?,?,?)`, r.ArchiveID, r.Reference, r.ReceiptID)
		return err
	case "mailbox_container":
		var r metadataMailboxContainer
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		c := r.Container
		if err := validateRetainedMailboxContainer(ctx, tx, c); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_containers(id,owner,sha256,size,format,state,created_at,manifest_sha256) VALUES(?,?,?,?,?,?,?,?)`, c.ID, c.Owner, c.SHA256, c.Size, c.Format, c.State, c.CreatedAt, c.ManifestSHA256); err != nil {
			return err
		}
		for _, ch := range c.Chunks {
			if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_chunks(container_id,chunk_index,blob_hash,size) VALUES(?,?,?,?)`, c.ID, ch.Index, ch.SHA256, ch.Size); err != nil {
				return err
			}
		}
		return nil
	default:
		return ErrMailboxInvalid
	}
}

type metadataMailboxArchive struct {
	Type    string         `json:"type"`
	Archive MailboxArchive `json:"archive"`
}
type metadataMailboxTransfer struct {
	Type    string                 `json:"type"`
	Receipt MailboxTransferReceipt `json:"receipt"`
}
type metadataMailboxTransferHead struct {
	Type      string `json:"type"`
	ArchiveID string `json:"archive_id"`
	Reference string `json:"reference"`
	ReceiptID string `json:"receipt_id"`
}

func exportMailboxTransfers(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	after := ""
	for {
		var a MailboxArchive
		err := q.QueryRowContext(ctx, `SELECT id,owner,description FROM mailbox_archives WHERE id>? ORDER BY id LIMIT 1`, after).Scan(&a.ID, &a.Owner, &a.Description)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		if !mailboxText(a.ID, 128) || !mailboxText(a.Owner, 256) || !mailboxText(a.Description, 1024) {
			return ErrMailboxInvalid
		}
		if err = write(metadataMailboxArchive{Type: "mailbox_archive", Archive: a}); err != nil {
			return err
		}
		after = a.ID
	}
	after = ""
	for {
		var id string
		err := q.QueryRowContext(ctx, `SELECT id FROM mailbox_transfer_receipts WHERE id>? ORDER BY id LIMIT 1`, after).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		r, err := loadMailboxTransferReceipt(ctx, q, id)
		if err != nil {
			return err
		}
		v, err := emailVersion(ctx, q, r.Target.VersionID)
		if err != nil {
			return err
		}
		if documentIdentity(v) != r.Target {
			return ErrMailboxInvalid
		}
		p, err := loadEmailDocumentPublication(ctx, q, r.DocumentPublicationID)
		if err != nil {
			return err
		}
		if p.Request.Parent != r.Target || p.Request.AttachmentID != r.EmailAttachmentID {
			return ErrMailboxInvalid
		}
		if err = write(metadataMailboxTransfer{Type: "mailbox_transfer_receipt", Receipt: r}); err != nil {
			return err
		}
		after = id
	}
	archive, reference := "", ""
	for {
		r := metadataMailboxTransferHead{Type: "mailbox_transfer_head"}
		err := q.QueryRowContext(ctx, `SELECT archive_id,source_ref,receipt_id FROM mailbox_transfer_heads WHERE (archive_id,source_ref)>(?,?) ORDER BY archive_id,source_ref LIMIT 1`, archive, reference).Scan(&r.ArchiveID, &r.Reference, &r.ReceiptID)
		if errors.Is(err, sql.ErrNoRows) {
			return exportMailboxJobs(ctx, q, write)
		}
		if err != nil {
			return err
		}
		receipt, err := loadMailboxTransferReceipt(ctx, q, r.ReceiptID)
		if err != nil {
			return err
		}
		if receipt.Request.ArchiveID != r.ArchiveID || receipt.Request.Reference != r.Reference {
			return ErrMailboxInvalid
		}
		if err = write(r); err != nil {
			return err
		}
		archive = r.ArchiveID
		reference = r.Reference
	}
}
