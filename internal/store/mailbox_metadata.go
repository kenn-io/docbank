package store

import (
	"context"
	"database/sql"
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
	if c.State != mailboxContainerSealed || c.ManifestSHA256 != h {
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

// mailboxMetadataTables registers the mailbox records for import. Their keyset
// exporters stay in exportMailboxMetadata and exportMailboxJobs.
var mailboxMetadataTables = []metadataRecordCodec{
	newMetadataTable(metadataTable[metadataMailboxJob]{record: metadataMailboxJob{Type: "mailbox_job"}, table: "mailbox_jobs",
		insert: func(ctx context.Context, tx *sql.Tx, r metadataMailboxJob) error {
			return saveMailboxJob(ctx, tx, r.Job, true)
		}}),
	newMetadataTable(metadataTable[metadataMailboxOccurrence]{
		record: metadataMailboxOccurrence{Type: "mailbox_occurrence"}, table: "mailbox_occurrences",
		insert: func(ctx context.Context, tx *sql.Tx, r metadataMailboxOccurrence) error {
			return insertMailboxOccurrence(ctx, tx, r.Occurrence)
		}}),
	newMetadataTable(metadataTable[metadataMailboxArchive]{record: metadataMailboxArchive{Type: "mailbox_archive"},
		table: "mailbox_archives", insert: importMailboxArchive}),
	newMetadataTable(metadataTable[metadataMailboxTransfer]{
		record: metadataMailboxTransfer{Type: "mailbox_transfer_receipt"}, table: "mailbox_transfer_receipts",
		insert: func(ctx context.Context, tx *sql.Tx, r metadataMailboxTransfer) error {
			return insertMailboxTransferReceipt(ctx, tx, r.Receipt)
		}}),
	newMetadataTable(metadataTable[metadataMailboxContainer]{record: metadataMailboxContainer{Type: "mailbox_container"},
		table: "mailbox_containers", insert: importMailboxContainer}),
}

func importMailboxArchive(ctx context.Context, tx *sql.Tx, r metadataMailboxArchive) error {
	a := r.Archive
	if !mailboxText(a.ID, 128) || !mailboxText(a.Owner, 256) || !mailboxText(a.Description, 1024) {
		return ErrMailboxInvalid
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO mailbox_archives(id,owner,description) VALUES(?,?,?)`, a.ID, a.Owner, a.Description)
	return err
}

func importMailboxContainer(ctx context.Context, tx *sql.Tx, r metadataMailboxContainer) error {
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
}

type metadataMailboxArchive struct {
	Type    string         `json:"type"`
	Archive MailboxArchive `json:"archive"`
}
type metadataMailboxTransfer struct {
	Type    string                 `json:"type"`
	Receipt MailboxTransferReceipt `json:"receipt"`
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
			return exportMailboxJobs(ctx, q, write)
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
}
