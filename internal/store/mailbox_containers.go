package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

const MailboxChunkBytes int64 = 64 << 20
const MailboxContainerBytes int64 = 256 << 30
const MailboxMaxChunks = 4096

var ErrMailboxConflict = errors.New("mailbox source, settings or target conflicts")
var ErrMailboxLimit = errors.New("mailbox resource limit exceeded")
var ErrMailboxInvalid = errors.New("invalid mailbox input")

type MailboxContainerRequest struct {
	ID     string `json:"id"`
	Owner  string `json:"owner"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Format string `json:"format"`
}
type MailboxChunk struct {
	Index  int    `json:"index"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type MailboxContainer struct {
	MailboxContainerRequest

	State          string         `json:"state"`
	CreatedAt      string         `json:"created_at"`
	ManifestSHA256 string         `json:"manifest_sha256"`
	Chunks         []MailboxChunk `json:"chunks"`
}

func mailboxText(v string, limit int) bool {
	return len(v) > 0 && len(v) <= limit && utf8.ValidString(v)
}
func mailboxHash(v string) bool {
	if len(v) != 64 {
		return false
	}
	b, e := hex.DecodeString(v)
	return e == nil && hex.EncodeToString(b) == v
}
func validateMailboxContainerRequest(r MailboxContainerRequest) error {
	if !mailboxText(r.ID, 128) || !mailboxText(r.Owner, 256) || !mailboxHash(r.SHA256) || r.Size < 1 || r.Size > MailboxContainerBytes || (r.Format != "mbox" && r.Format != "zip") {
		return ErrMailboxInvalid
	}
	return nil
}
func validateMailboxChunk(size int64, ch MailboxChunk) error {
	if ch.Index < 0 || ch.Index >= MailboxMaxChunks || int64(ch.Index)*MailboxChunkBytes >= size || ch.Size != min(MailboxChunkBytes, size-int64(ch.Index)*MailboxChunkBytes) || !mailboxHash(ch.SHA256) {
		return ErrMailboxInvalid
	}
	return nil
}

func mailboxManifest(c MailboxContainer) (string, error) {
	if err := validateMailboxContainerRequest(c.MailboxContainerRequest); err != nil {
		return "", err
	}
	count := (c.Size + MailboxChunkBytes - 1) / MailboxChunkBytes
	if len(c.Chunks) != int(count) || len(c.Chunks) > MailboxMaxChunks {
		return "", ErrMailboxInvalid
	}
	for i, ch := range c.Chunks {
		if ch.Index != i || validateMailboxChunk(c.Size, ch) != nil {
			return "", ErrMailboxInvalid
		}
	}
	b, err := json.Marshal(struct {
		SHA256 string         `json:"sha256"`
		Size   int64          `json:"size"`
		Chunks []MailboxChunk `json:"chunks"`
	}{c.SHA256, c.Size, c.Chunks})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func loadMailboxContainer(ctx context.Context, q metadataQuerier, owner, id string) (MailboxContainer, error) {
	var c MailboxContainer
	err := q.QueryRowContext(ctx, `SELECT id,owner,sha256,size,format,state,created_at,manifest_sha256 FROM mailbox_containers WHERE id=? AND owner=?`, id, owner).Scan(&c.ID, &c.Owner, &c.SHA256, &c.Size, &c.Format, &c.State, &c.CreatedAt, &c.ManifestSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	if err = validateMailboxContainerRequest(c.MailboxContainerRequest); err != nil {
		return c, err
	}
	if c.State != "uploading" && c.State != "sealed" {
		return c, ErrMailboxInvalid
	}
	rows, err := q.QueryContext(ctx, `SELECT chunk_index,blob_hash,size FROM mailbox_chunks WHERE container_id=? ORDER BY chunk_index LIMIT 4097`, id)
	if err != nil {
		return c, err
	}
	defer func() { _ = rows.Close() }()
	c.Chunks = []MailboxChunk{}
	for rows.Next() {
		var ch MailboxChunk
		if err = rows.Scan(&ch.Index, &ch.SHA256, &ch.Size); err != nil {
			return c, err
		}
		if err = validateMailboxChunk(c.Size, ch); err != nil {
			return c, err
		}
		c.Chunks = append(c.Chunks, ch)
	}
	if err = rows.Err(); err != nil {
		return c, err
	}
	if len(c.Chunks) > MailboxMaxChunks {
		return c, ErrMailboxLimit
	}
	if c.State == "sealed" {
		h, e := mailboxManifest(c)
		if e != nil || h != c.ManifestSHA256 {
			return c, ErrMailboxInvalid
		}
	}
	return c, nil
}
func mailboxSessionActive(c MailboxContainer) bool {
	t, e := time.Parse(time.RFC3339Nano, c.CreatedAt)
	return e == nil && c.State == "uploading" && time.Now().UTC().Before(t.Add(24*time.Hour))
}
func cleanupMailboxContainersTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM mailbox_containers WHERE id IN (SELECT id FROM mailbox_containers WHERE state='uploading' AND created_at<=? LIMIT 8)`, time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339Nano))
	return err
}
func (s *Store) CleanupMailboxContainers(ctx context.Context) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error { return cleanupMailboxContainersTx(ctx, tx) })
}
func (s *Store) BeginMailboxContainer(ctx context.Context, r MailboxContainerRequest) (MailboxContainer, error) {
	if err := validateMailboxContainerRequest(r); err != nil {
		return MailboxContainer{}, err
	}
	var c MailboxContainer
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := cleanupMailboxContainersTx(ctx, tx); err != nil {
			return err
		}
		old, err := loadMailboxContainer(ctx, tx, r.Owner, r.ID)
		if err == nil {
			if old.MailboxContainerRequest != r {
				return ErrMailboxConflict
			}
			c = old
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		var all, owned int
		if err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(owner=?),0) FROM mailbox_containers WHERE state='uploading'`, r.Owner).Scan(&all, &owned); err != nil {
			return err
		}
		if all >= 8 || owned >= 2 {
			return ErrMailboxLimit
		}
		c = MailboxContainer{MailboxContainerRequest: r, State: "uploading", CreatedAt: nowRFC3339(), Chunks: []MailboxChunk{}}
		_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_containers(id,owner,sha256,size,format,state,created_at) VALUES(?,?,?,?,?,'uploading',?)`, r.ID, r.Owner, r.SHA256, r.Size, r.Format, c.CreatedAt)
		return err
	})
	return c, err
}
func (s *Store) MailboxContainer(ctx context.Context, owner, id string) (MailboxContainer, error) {
	return loadMailboxContainer(ctx, s.db, owner, id)
}

// PutMailboxChunk records a verified durable blob under the caller's mutation
// lease. It never replaces an accepted index with a different digest.
func (s *Store) PutMailboxChunk(ctx context.Context, owner, id string, ch MailboxChunk) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		c, err := loadMailboxContainer(ctx, tx, owner, id)
		if err != nil {
			return err
		}
		if err = validateMailboxChunk(c.Size, ch); err != nil {
			return err
		}
		for _, old := range c.Chunks {
			if old.Index == ch.Index {
				if old == ch && (c.State == "sealed" || mailboxSessionActive(c)) {
					return nil
				}
				return ErrMailboxConflict
			}
		}
		if !mailboxSessionActive(c) {
			return ErrMailboxConflict
		}
		var size int64
		if err = tx.QueryRowContext(ctx, `SELECT size FROM blobs WHERE hash=?`, ch.SHA256).Scan(&size); err != nil {
			return err
		}
		if size != ch.Size {
			return ErrMailboxConflict
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_chunks(container_id,chunk_index,blob_hash,size) VALUES(?,?,?,?)`, id, ch.Index, ch.SHA256, ch.Size)
		return err
	})
}

// SealMailboxContainer accepts the full digest/size freshly verified by the
// streaming service under its blob mutation lease, then binds ordered identity.
func (s *Store) SealMailboxContainer(ctx context.Context, owner, id, verifiedHash string, verifiedSize int64) (MailboxContainer, error) {
	var c MailboxContainer
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		c, err = loadMailboxContainer(ctx, tx, owner, id)
		if err != nil {
			return err
		}
		if c.SHA256 != verifiedHash || c.Size != verifiedSize {
			return ErrMailboxConflict
		}
		h, err := mailboxManifest(c)
		if err != nil {
			return err
		}
		if c.State == "sealed" {
			return nil
		}
		if !mailboxSessionActive(c) {
			return ErrMailboxConflict
		}
		c.State = "sealed"
		c.ManifestSHA256 = h
		_, err = tx.ExecContext(ctx, `UPDATE mailbox_containers SET state='sealed',manifest_sha256=? WHERE id=?`, h, id)
		return err
	})
	return c, err
}
func (s *Store) AbortMailboxContainer(ctx context.Context, owner, id string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		c, err := loadMailboxContainer(ctx, tx, owner, id)
		if err != nil {
			return err
		}
		if c.State != "uploading" {
			return fmt.Errorf("%w: sealed source authority cannot be aborted", ErrMailboxConflict)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM mailbox_containers WHERE id=?`, id)
		return err
	})
}
