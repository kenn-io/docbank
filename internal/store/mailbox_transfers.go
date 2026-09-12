package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"sort"
	"strings"

	"go.kenn.io/docbank/document"
)

type MailboxArchive struct {
	ID          string `json:"id"`
	Owner       string `json:"owner"`
	Description string `json:"description"`
}
type MailboxTransferRequest struct {
	ArchiveID        string `json:"archive_id"`
	Reference        string `json:"reference"`
	SHA256           string `json:"sha256"`
	Size             int64  `json:"size"`
	Settings         string `json:"settings"`
	DestinationID    int64  `json:"destination_id"`
	Name             string `json:"name"`
	ExpectedRevision *int64 `json:"expected_revision,omitempty"`
}
type MailboxLocation struct {
	ContainerID string   `json:"container_id"`
	Entry       string   `json:"entry"`
	EntryIndex  int      `json:"entry_index"`
	EntrySHA256 string   `json:"entry_sha256"`
	Sequence    int64    `json:"sequence"`
	Start       int64    `json:"start"`
	End         int64    `json:"end"`
	Separator   string   `json:"separator"`
	RawSHA256   string   `json:"raw_sha256"`
	EMLSHA256   string   `json:"eml_sha256"`
	EMLSize     int64    `json:"eml_size"`
	Labels      []string `json:"labels"`
}
type MailboxTransferReceipt struct {
	ID                    string                         `json:"id"`
	Request               MailboxTransferRequest         `json:"request"`
	RequestDigest         string                         `json:"request_digest"`
	Target                document.EmailDocumentIdentity `json:"target"`
	TargetRevision        int64                          `json:"target_revision"`
	EmailAttachmentID     string                         `json:"email_attachment_id"`
	DocumentPublicationID string                         `json:"document_publication_id"`
	CreatedAt             string                         `json:"created_at"`
	Outcome               string                         `json:"outcome"`
	Location              *MailboxLocation               `json:"location,omitempty"`
}
type MailboxTransferPublication struct {
	Owner     string
	Run       IngestRun
	Request   MailboxTransferRequest
	Email     EmailPublication
	Location  *MailboxLocation
	LabelTags map[string]string
}

func (s *Store) RegisterMailboxArchive(ctx context.Context, a MailboxArchive) error {
	if !mailboxText(a.ID, 128) || !mailboxText(a.Owner, 256) || !mailboxText(a.Description, 1024) {
		return ErrMailboxInvalid
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var old MailboxArchive
		err := tx.QueryRowContext(ctx, `SELECT id,owner,description FROM mailbox_archives WHERE id=?`, a.ID).Scan(&old.ID, &old.Owner, &old.Description)
		if err == nil {
			if old != a {
				return ErrMailboxConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_archives(id,owner,description) VALUES(?,?,?)`, a.ID, a.Owner, a.Description)
		return err
	})
}
func mailboxTransferDigest(r MailboxTransferRequest) (string, error) {
	if !mailboxText(r.ArchiveID, 128) || !mailboxText(r.Reference, 4096) || !mailboxHash(r.SHA256) || r.Size < 1 || r.Size > 128<<20 || !mailboxText(r.Settings, 64<<10) || r.DestinationID < 1 {
		return "", ErrMailboxInvalid
	}
	if _, err := NormalizeName(r.Name); err != nil {
		return "", err
	}
	r.ExpectedRevision = nil
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func loadMailboxTransferReceipt(ctx context.Context, q metadataQuerier, id string) (MailboxTransferReceipt, error) {
	var r MailboxTransferReceipt
	var raw []byte
	var archive, ref, version, publication string
	err := q.QueryRowContext(ctx, `SELECT archive_id,source_ref,target_version_id,document_publication_id,receipt_json FROM mailbox_transfer_receipts WHERE id=?`, id).Scan(&archive, &ref, &version, &publication, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if len(raw) > 1<<20 {
		return r, ErrMailboxLimit
	}
	if err = json.Unmarshal(raw, &r, json.RejectUnknownMembers(true)); err != nil {
		return r, err
	}
	digest, err := mailboxTransferDigest(r.Request)
	if err != nil {
		return r, err
	}
	if r.ID != id || r.Request.ArchiveID != archive || r.Request.Reference != ref || r.Target.VersionID != version || r.DocumentPublicationID != publication || r.RequestDigest != digest || r.Request.SHA256 != r.Target.SHA256 || r.Request.Size != r.Target.Size || r.Outcome != "imported" || r.TargetRevision < 1 {
		return r, ErrMailboxInvalid
	}
	if r.Location != nil && (r.Location.EMLSHA256 != r.Request.SHA256 || r.Location.EMLSize != r.Request.Size) {
		return r, ErrMailboxInvalid
	}
	if err = document.ValidateEmailDocumentIdentity(r.Target); err != nil {
		return r, err
	}
	return r, validateMetadataTime("mailbox receipt created_at", r.CreatedAt)
}
func mailboxTransferHead(ctx context.Context, q metadataQuerier, owner, archive, reference string) (MailboxTransferReceipt, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT h.receipt_id FROM mailbox_transfer_heads h JOIN mailbox_archives a ON a.id=h.archive_id WHERE h.archive_id=? AND h.source_ref=? AND a.owner=?`, archive, reference, owner).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return MailboxTransferReceipt{}, ErrNotFound
	}
	if err != nil {
		return MailboxTransferReceipt{}, err
	}
	return loadMailboxTransferReceipt(ctx, q, id)
}
func (s *Store) MailboxTransfer(ctx context.Context, owner, archive, reference string) (MailboxTransferReceipt, error) {
	return mailboxTransferHead(ctx, s.db, owner, archive, reference)
}

// PublishMailboxTransfer publishes source, complete decoded inventory, child
// relations, and retry identity in one storage transaction. Bytes are already
// durable and verified under the caller's blob mutation lease.
func (s *Store) PublishMailboxTransfer(ctx context.Context, p MailboxTransferPublication) (MailboxTransferReceipt, error) {
	var receipt MailboxTransferReceipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		receipt, err = s.publishMailboxTransferTx(ctx, tx, p)
		return err
	})
	return receipt, err
}
func (s *Store) publishMailboxTransferTx(ctx context.Context, tx *sql.Tx, p MailboxTransferPublication) (MailboxTransferReceipt, error) {
	if p.Location != nil && (p.Location.EMLSHA256 != p.Request.SHA256 || p.Location.EMLSize != p.Request.Size) {
		return MailboxTransferReceipt{}, ErrMailboxInvalid
	}
	digest, err := mailboxTransferDigest(p.Request)
	if err != nil {
		return MailboxTransferReceipt{}, err
	}
	var owner string
	if err = tx.QueryRowContext(ctx, `SELECT owner FROM mailbox_archives WHERE id=?`, p.Request.ArchiveID).Scan(&owner); err != nil {
		return MailboxTransferReceipt{}, err
	}
	if owner != p.Owner {
		return MailboxTransferReceipt{}, ErrNotFound
	}
	prior, err := mailboxTransferHead(ctx, tx, p.Owner, p.Request.ArchiveID, p.Request.Reference)
	existing := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return MailboxTransferReceipt{}, err
	}
	var target Node
	if existing {
		target, err = nodeByIDTx(tx, prior.Target.NodeID)
		if errors.Is(err, ErrNotFound) || (err == nil && target.TrashedAt != nil) {
			prior.Outcome = "tombstone"
			return prior, nil
		}
		if err != nil {
			return MailboxTransferReceipt{}, err
		}
		if target.Revision != prior.TargetRevision || target.CurrentVersionID != prior.Target.VersionID {
			return MailboxTransferReceipt{}, ErrMailboxConflict
		}
		if digest == prior.RequestDigest {
			return prior, nil
		}
		if p.Request.Settings != prior.Request.Settings || p.Request.DestinationID != prior.Request.DestinationID || p.Request.Name != prior.Request.Name || p.Request.ExpectedRevision == nil || *p.Request.ExpectedRevision != target.Revision || p.Request.SHA256 == prior.Request.SHA256 {
			return MailboxTransferReceipt{}, ErrMailboxConflict
		}
	}
	id, err := newUUIDv4()
	if err != nil {
		return MailboxTransferReceipt{}, err
	}
	var content ContentWriteReceipt
	if existing {
		content.Node, content.Version, err = s.replaceContentTx(ctx, tx, target, *p.Request.ExpectedRevision, p.Request.SHA256, p.Request.Size, "message/rfc822")
	} else {
		name := strings.TrimSuffix(p.Request.Name, ".eml")
		if len(name) > 180 {
			name = "message"
		}
		name += "-" + id + ".eml"
		content, _, _, err = s.ingestFileTx(ctx, tx, p.Run, p.Request.DestinationID, name, p.Request.SHA256, p.Request.Size, "message/rfc822", p.Request.Reference, "", ingestFileOptions{exact: true, completeReceipt: true})
	}
	if err != nil {
		return MailboxTransferReceipt{}, err
	}
	if p.Location != nil {
		ids := map[string]bool{}
		for _, label := range p.Location.Labels {
			if id := p.LabelTags[label]; id != "" {
				ids[id] = true
			}
		}
		ordered := make([]string, 0, len(ids))
		for id := range ids {
			ordered = append(ordered, id)
		}
		sort.Strings(ordered)
		active, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return MailboxTransferReceipt{}, err
		}
		for _, id := range ordered {
			var change TagAssignmentChange
			if active {
				change, err = s.changeAuditedTagAssignmentTx(ctx, tx, id, content.Node, content.Node.Revision, true)
			} else {
				change, err = changeTagAssignmentTx(ctx, tx, id, content.Node, content.Node.Revision, true, nowRFC3339())
			}
			if err != nil {
				return MailboxTransferReceipt{}, err
			}
			content.Node = change.Node
		}
	}
	p.Email.ContentVersionID = content.Version.ID
	view, err := s.publishEmailGenerationTx(ctx, tx, p.Email)
	if err != nil {
		return MailboxTransferReceipt{}, err
	}
	if view.Evidence.Inventory == nil {
		return MailboxTransferReceipt{}, ErrMailboxInvalid
	}
	dir, err := liveDirTx(tx, p.Request.DestinationID)
	if err != nil {
		return MailboxTransferReceipt{}, err
	}
	children, err := s.publishEmailDocumentsTx(ctx, tx, document.EmailDocumentPublicationRequest{OperationID: id, Parent: documentIdentity(content.Version), GenerationID: view.Generation.ID, AttachmentID: view.Attachment.ID, DestinationID: dir.ID, DestinationRevision: dir.Revision, Reuse: []document.EmailDocumentReuse{}})
	if err != nil {
		return MailboxTransferReceipt{}, err
	}
	receipt := MailboxTransferReceipt{ID: id, Request: p.Request, RequestDigest: digest, Target: documentIdentity(content.Version), TargetRevision: content.Node.Revision, EmailAttachmentID: view.Attachment.ID, DocumentPublicationID: children.OperationID, CreatedAt: nowRFC3339(), Outcome: "imported", Location: p.Location}
	if err = insertMailboxTransferReceipt(ctx, tx, receipt); err != nil {
		return MailboxTransferReceipt{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_transfer_heads(archive_id,source_ref,receipt_id) VALUES(?,?,?) ON CONFLICT(archive_id,source_ref) DO UPDATE SET receipt_id=excluded.receipt_id`, p.Request.ArchiveID, p.Request.Reference, id)
	return receipt, err
}
func insertMailboxTransferReceipt(ctx context.Context, tx *sql.Tx, r MailboxTransferReceipt) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > 1<<20 {
		return ErrMailboxLimit
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_transfer_receipts(id,archive_id,source_ref,target_version_id,document_publication_id,receipt_json) VALUES(?,?,?,?,?,?)`, r.ID, r.Request.ArchiveID, r.Request.Reference, r.Target.VersionID, r.DocumentPublicationID, b)
	return err
}
