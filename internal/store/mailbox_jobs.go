package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/emailmime"
	"reflect"
)

const MailboxSegmentMessages int64 = 100000

const (
	mailboxJobQueued         = "queued"
	mailboxJobRunning        = "running"
	mailboxJobFailed         = "failed"
	mailboxOccurrencePending = "pending"
)

type MailboxSettings struct {
	Recipe        string            `json:"recipe"`
	Dialect       string            `json:"dialect"`
	DestinationID int64             `json:"destination_id"`
	LabelTags     map[string]string `json:"label_tags"`
}

func normalizeMailboxSettings(s MailboxSettings) (MailboxSettings, error) {
	if s.Recipe == "" {
		s.Recipe = mailboxRecipe()
	}
	if !mailboxText(s.Recipe, 256) {
		return s, ErrMailboxInvalid
	}
	if s.Dialect == "" {
		s.Dialect = "mboxrd"
	}
	if s.LabelTags == nil {
		s.LabelTags = map[string]string{}
	}
	if (s.Dialect != "mboxrd" && s.Dialect != "mboxo") || s.DestinationID < 1 || len(s.LabelTags) > 100 {
		return s, ErrMailboxInvalid
	}
	for k, v := range s.LabelTags {
		if !mailboxText(k, 256) || !mailboxText(v, 128) {
			return s, ErrMailboxInvalid
		}
	}
	return s, nil
}

func validateMailboxLabelTagsTx(tx *sql.Tx, tags map[string]string) error {
	for label, id := range tags {
		if _, err := tagByIDTx(tx, id); err != nil {
			return fmt.Errorf("mailbox label %q: %w", label, err)
		}
	}
	return nil
}

func mailboxRecipe() string {
	fingerprint, _ := document.EmailRecipeFingerprint(emailmime.Recipe())
	return "docbank-mailbox/v1:" + fingerprint
}

func (s MailboxSettings) ValidateExecution() error {
	if s.Recipe != mailboxRecipe() {
		return ErrMailboxConflict
	}
	return nil
}
func (s MailboxSettings) Canonical() (string, error) {
	v, err := normalizeMailboxSettings(s)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(v, json.Deterministic(true))
	return string(b), err
}

type MailboxJobRequest struct {
	ID              string          `json:"id"`
	ContainerID     string          `json:"container_id"`
	ContainerSHA256 string          `json:"container_sha256"`
	Settings        MailboxSettings `json:"settings"`
}
type MailboxJob struct {
	MailboxJobRequest

	Owner        string   `json:"owner"`
	State        string   `json:"state"`
	Claim        string   `json:"-"`
	CollectionID string   `json:"collection_id"`
	StartedAt    string   `json:"started_at"`
	Checkpoint   int64    `json:"checkpoint"`
	SegmentStart int64    `json:"segment_start"`
	Imported     int64    `json:"imported"`
	Rejected     int64    `json:"rejected"`
	EntryHashes  []string `json:"entry_hashes,omitempty"`
	Pending      int64    `json:"pending"`
	Canceled     int64    `json:"canceled"`
	ScannedTail  bool     `json:"scanned_tail"`
	Reason       string   `json:"reason"`
}

func (j MailboxJob) IngestRun() IngestRun {
	return IngestRun{record: metadataIngest{Type: metadataIngestType, ID: j.CollectionID, StartedAt: j.StartedAt, SourceKind: "mailbox", SourceDesc: j.ContainerID}}
}

type MailboxOccurrence struct {
	JobID     string                          `json:"job_id"`
	Ordinal   int64                           `json:"ordinal"`
	Outcome   string                          `json:"outcome"`
	Reason    string                          `json:"reason"`
	Location  MailboxLocation                 `json:"location"`
	ReceiptID string                          `json:"receipt_id,omitempty"`
	Target    *document.EmailDocumentIdentity `json:"target,omitempty"`
}

func validateMailboxJob(j MailboxJob) error {
	if j.Pending < 0 || j.Pending > 1 || j.Canceled < 0 || j.Canceled > 1 || j.Pending+j.Canceled > 1 {
		return ErrMailboxInvalid
	}
	if !mailboxText(j.ID, 128) || !mailboxText(j.Owner, 256) ||
		!mailboxText(j.ContainerID, 128) || !mailboxHash(j.ContainerSHA256) ||
		j.Checkpoint < 0 || j.SegmentStart < 0 || j.SegmentStart > j.Checkpoint ||
		j.Imported < 0 || j.Rejected < 0 || j.Checkpoint != j.Imported+j.Rejected ||
		len(j.Reason) > 4096 {
		return ErrMailboxInvalid
	}
	if len(j.EntryHashes) > 1000 {
		return ErrMailboxLimit
	}
	for _, hash := range j.EntryHashes {
		if !mailboxHash(hash) {
			return ErrMailboxInvalid
		}
	}
	if _, err := normalizeMailboxSettings(j.Settings); err != nil {
		return err
	}
	if err := validateUUIDv4(j.CollectionID); err != nil {
		return err
	}
	if err := validateMetadataTime("mailbox job started_at", j.StartedAt); err != nil {
		return err
	}
	switch j.State {
	case mailboxJobQueued, mailboxJobRunning, "complete", "partial", mailboxJobFailed, "canceled":
	default:
		return ErrMailboxInvalid
	}
	if j.State == "complete" && (!j.ScannedTail || j.Rejected != 0 || j.Pending != 0 || j.Canceled != 0) {
		return ErrMailboxInvalid
	}
	return nil
}
func saveMailboxJob(ctx context.Context, tx *sql.Tx, j MailboxJob, insert bool) error {
	if err := validateMailboxJob(j); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if len(b) > 128<<10 {
		return ErrMailboxLimit
	}
	if insert {
		_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_jobs(id,owner,container_id,state,claim,job_json) VALUES(?,?,?,?,?,?)`, j.ID, j.Owner, j.ContainerID, j.State, j.Claim, b)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE mailbox_jobs SET state=?,claim=?,job_json=? WHERE id=?`, j.State, j.Claim, b, j.ID)
	}
	return err
}
func loadMailboxJob(ctx context.Context, q metadataQuerier, owner, id string) (MailboxJob, error) {
	var j MailboxJob
	var b []byte
	var state, claim, container string
	err := q.QueryRowContext(ctx, `SELECT state,claim,container_id,job_json FROM mailbox_jobs WHERE id=? AND owner=?`, id, owner).Scan(&state, &claim, &container, &b)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	if len(b) > 128<<10 {
		return j, ErrMailboxLimit
	}
	if err = json.Unmarshal(b, &j, json.RejectUnknownMembers(true)); err != nil {
		return j, err
	}
	if j.ID != id || j.Owner != owner || j.State != state || j.ContainerID != container {
		return j, ErrMailboxInvalid
	}
	j.Claim = claim
	return j, validateMailboxJob(j)
}
func (s *Store) MailboxJob(ctx context.Context, owner, id string) (MailboxJob, error) {
	return loadMailboxJob(ctx, s.db, owner, id)
}
func (s *Store) MailboxJobs(ctx context.Context, owner, after string, limit int) ([]MailboxJob, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrMailboxLimit
	}
	var started string
	if after != "" {
		cursor, err := s.MailboxJob(ctx, owner, after)
		if err != nil {
			return nil, err
		}
		started = cursor.StartedAt
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM mailbox_jobs WHERE owner=?
		AND (?='' OR (json_extract(job_json, '$.started_at'), id) < (?, ?))
		ORDER BY json_extract(job_json, '$.started_at') DESC, id DESC LIMIT ?`, owner, after, started, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	out := make([]MailboxJob, 0, len(ids))
	for _, id := range ids {
		j, err := s.MailboxJob(ctx, owner, id)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}
func mailboxJobQuota(ctx context.Context, tx *sql.Tx, owner string) error {
	var all, owned int
	err := tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(owner=?),0) FROM mailbox_jobs WHERE state IN ('queued','running')`, owner).Scan(&all, &owned)
	if err != nil {
		return err
	}
	if all >= 8 || owned >= 2 {
		return ErrMailboxLimit
	}
	return nil
}
func (s *Store) BeginMailboxJob(ctx context.Context, owner string, r MailboxJobRequest) (MailboxJob, error) {
	var j MailboxJob
	settings, err := normalizeMailboxSettings(r.Settings)
	if err != nil {
		return j, err
	}
	r.Settings = settings
	if err := settings.ValidateExecution(); err != nil {
		return j, err
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		old, err := loadMailboxJob(ctx, tx, owner, r.ID)
		if err == nil {
			if !reflect.DeepEqual(old.MailboxJobRequest, r) {
				return ErrMailboxConflict
			}
			j = old
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		c, err := loadMailboxContainer(ctx, tx, owner, r.ContainerID)
		if err != nil {
			return err
		}
		if c.State != mailboxContainerSealed || c.SHA256 != r.ContainerSHA256 {
			return ErrMailboxConflict
		}
		if _, err = liveDirTx(tx, r.Settings.DestinationID); err != nil {
			return err
		}
		if err = validateMailboxLabelTagsTx(tx, r.Settings.LabelTags); err != nil {
			return err
		}
		if err = mailboxJobQuota(ctx, tx, owner); err != nil {
			return err
		}
		collection, err := newUUIDv4()
		if err != nil {
			return err
		}
		j = MailboxJob{MailboxJobRequest: r, Owner: owner, State: mailboxJobQueued, CollectionID: collection, StartedAt: nowRFC3339()}
		return saveMailboxJob(ctx, tx, j, true)
	})
	return j, err
}
func (s *Store) ClaimMailboxJob(ctx context.Context) (MailboxJob, error) {
	var j MailboxJob
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_jobs WHERE state='running'`).Scan(&count); err != nil {
			return err
		}
		if count >= 2 {
			return ErrMailboxLimit
		}
		var owner, id string
		err := tx.QueryRowContext(ctx, `SELECT owner,id FROM mailbox_jobs WHERE state='queued' ORDER BY id LIMIT 1`).Scan(&owner, &id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		j, err = loadMailboxJob(ctx, tx, owner, id)
		if err != nil {
			return err
		}
		j.Claim, err = newUUIDv4()
		if err != nil {
			return err
		}
		j.State = mailboxJobRunning
		return saveMailboxJob(ctx, tx, j, false)
	})
	return j, err
}
func claimedMailboxJob(ctx context.Context, tx *sql.Tx, id, claim string) (MailboxJob, error) {
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT owner FROM mailbox_jobs WHERE id=?`, id).Scan(&owner); err != nil {
		return MailboxJob{}, err
	}
	j, err := loadMailboxJob(ctx, tx, owner, id)
	if err != nil {
		return j, err
	}
	if j.State != mailboxJobRunning || claim == "" || j.Claim != claim {
		return j, ErrMailboxConflict
	}
	return j, nil
}
func (s *Store) CancelMailboxJob(ctx context.Context, owner, id string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		j, err := loadMailboxJob(ctx, tx, owner, id)
		if err != nil {
			return err
		}
		if j.State == "complete" || j.State == "partial" || j.State == mailboxJobFailed {
			return ErrMailboxConflict
		}
		j.State = "canceled"
		j.Claim = ""
		if err = setMailboxPendingOutcome(ctx, tx, &j, "canceled"); err != nil {
			return err
		}
		return saveMailboxJob(ctx, tx, j, false)
	})
}
func (s *Store) ResumeMailboxJob(ctx context.Context, owner string, r MailboxJobRequest, continuation bool) (MailboxJob, error) {
	var j MailboxJob
	var err error
	r.Settings, err = normalizeMailboxSettings(r.Settings)
	if err != nil {
		return j, err
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		j, err = loadMailboxJob(ctx, tx, owner, r.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(j.MailboxJobRequest, r) || j.ScannedTail || r.Settings.ValidateExecution() != nil {
			return ErrMailboxConflict
		}
		if err = validateMailboxLabelTagsTx(tx, r.Settings.LabelTags); err != nil {
			return err
		}
		if j.State == mailboxJobQueued || j.State == mailboxJobRunning {
			return nil
		}
		if j.State == "partial" && !continuation {
			return ErrMailboxConflict
		}
		if err = mailboxJobQuota(ctx, tx, owner); err != nil {
			return err
		}
		if continuation {
			j.SegmentStart = j.Checkpoint
		}
		j.State = mailboxJobQueued
		j.Claim = ""
		j.Reason = ""
		if err = setMailboxPendingOutcome(ctx, tx, &j, mailboxOccurrencePending); err != nil {
			return err
		}
		return saveMailboxJob(ctx, tx, j, false)
	})
	return j, err
}
func (s *Store) FinishMailboxJob(ctx context.Context, id, claim, state, reason string, tail bool) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		j, err := claimedMailboxJob(ctx, tx, id, claim)
		if err != nil {
			return err
		}
		j.State = state
		j.Reason = reason
		j.ScannedTail = tail
		j.Claim = ""
		return saveMailboxJob(ctx, tx, j, false)
	})
}

// VerifyMailboxJobEntries persists hashes only after the worker has verified
// every ZIP entry, before publishing any occurrence from this immutable source.
func (s *Store) VerifyMailboxJobEntries(ctx context.Context, id, claim string, hashes []string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		j, err := claimedMailboxJob(ctx, tx, id, claim)
		if err != nil {
			return err
		}
		if len(hashes) == 0 || j.Checkpoint != 0 || len(j.EntryHashes) != 0 {
			return ErrMailboxConflict
		}
		j.EntryHashes = hashes
		return saveMailboxJob(ctx, tx, j, false)
	})
}
func (s *Store) ResetMailboxClaims(ctx context.Context) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for range 2 {
			var owner, id string
			err := tx.QueryRowContext(ctx, `SELECT owner,id FROM mailbox_jobs WHERE state='running' LIMIT 1`).Scan(&owner, &id)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			j, err := loadMailboxJob(ctx, tx, owner, id)
			if err != nil {
				return err
			}
			j.State = mailboxJobQueued
			j.Claim = ""
			if err = saveMailboxJob(ctx, tx, j, false); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) CommitMailboxOccurrence(ctx context.Context, id, claim string, o MailboxOccurrence, p *MailboxTransferPublication) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		j, err := claimedMailboxJob(ctx, tx, id, claim)
		if err != nil {
			return err
		}
		if o.JobID != id || o.Ordinal != j.Checkpoint+1 || j.Checkpoint-j.SegmentStart >= MailboxSegmentMessages || o.Location.ContainerID != j.ContainerID {
			return ErrMailboxConflict
		}
		var published *MailboxTransferReceipt
		if p != nil {
			settings, err := j.Settings.Canonical()
			if err != nil {
				return err
			}
			if p.Owner != j.Owner || p.Location == nil || !reflect.DeepEqual(*p.Location, o.Location) || p.Request.ArchiveID != "mailbox:"+j.CollectionID || p.Request.Reference != fmt.Sprintf("%d:%d", o.Location.EntryIndex, o.Location.Sequence) || p.Request.Settings != settings || p.Request.DestinationID != j.Settings.DestinationID {
				return ErrMailboxConflict
			}
			p.Run = j.IngestRun()
			p.LabelTags = j.Settings.LabelTags
			p.jobOrigin = true
			r, err := s.publishMailboxTransferTx(ctx, tx, *p)
			if err != nil {
				return err
			}
			if r.Outcome != "imported" || r.Location == nil || !reflect.DeepEqual(*r.Location, o.Location) {
				return ErrMailboxConflict
			}
			o.ReceiptID = r.ID
			o.Target = &r.Target
			o.Outcome = "imported"
			published = &r
		}
		switch o.Outcome {
		case "imported":
			if o.ReceiptID == "" {
				return ErrMailboxInvalid
			}
			j.Imported++
		case "rejected":
			if !mailboxText(o.Reason, 4096) {
				return ErrMailboxInvalid
			}
			j.Rejected++
		default:
			return ErrMailboxInvalid
		}
		b, err := json.Marshal(o)
		if err != nil {
			return err
		}
		if len(b) > 64<<10 {
			return ErrMailboxLimit
		}
		var receipt any
		if o.ReceiptID != "" {
			receipt = o.ReceiptID
		}
		var priorReceipt sql.NullString
		lookupErr := tx.QueryRowContext(ctx, `SELECT receipt_id FROM mailbox_occurrences WHERE job_id=? AND ordinal=?`, id, o.Ordinal).Scan(&priorReceipt)
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			return lookupErr
		}
		if priorReceipt.Valid && priorReceipt.String != o.ReceiptID {
			return ErrMailboxConflict
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO mailbox_occurrences(job_id,ordinal,receipt_id,occurrence_json) VALUES(?,?,?,?) ON CONFLICT(job_id,ordinal) DO UPDATE SET receipt_id=excluded.receipt_id,occurrence_json=excluded.occurrence_json`, id, o.Ordinal, receipt, b); err != nil {
			return err
		}
		if o.ReceiptID != "" {
			if published == nil {
				r, loadErr := loadMailboxTransferReceipt(ctx, tx, o.ReceiptID)
				if loadErr != nil {
					return loadErr
				}
				published = &r
			}
			if err = appendMailboxReceiptOccurrenceTx(ctx, tx, *published, MailboxReceiptOccurrenceKey{JobID: id, Ordinal: o.Ordinal}); err != nil {
				return err
			}
		}
		j.Checkpoint = o.Ordinal
		j.Pending = 0
		j.Canceled = 0
		return saveMailboxJob(ctx, tx, j, false)
	})
}
func (s *Store) MailboxOccurrences(ctx context.Context, owner, id string, after int64, limit int) ([]MailboxOccurrence, error) {
	if after < 0 || limit < 1 || limit > 100 {
		return nil, ErrMailboxLimit
	}
	if _, err := s.MailboxJob(ctx, owner, id); err != nil {
		return nil, err
	}
	return mailboxOccurrences(ctx, s.db, id, after, limit)
}
func mailboxOccurrences(ctx context.Context, q metadataQuerier, id string, after int64, limit int) ([]MailboxOccurrence, error) {
	rows, err := q.QueryContext(ctx, `SELECT ordinal,receipt_id,occurrence_json FROM mailbox_occurrences WHERE job_id=? AND ordinal>? ORDER BY ordinal LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []MailboxOccurrence{}
	for rows.Next() {
		var ordinal int64
		var receipt sql.NullString
		var b []byte
		if err = rows.Scan(&ordinal, &receipt, &b); err != nil {
			return nil, err
		}
		if len(b) > 64<<10 {
			return nil, ErrMailboxLimit
		}
		var o MailboxOccurrence
		if err = json.Unmarshal(b, &o, json.RejectUnknownMembers(true)); err != nil {
			return nil, err
		}
		if o.JobID != id || o.Ordinal != ordinal || o.ReceiptID != receipt.String {
			return nil, ErrMailboxInvalid
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
