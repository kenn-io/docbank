package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/filename"
	"go.kenn.io/docbank/internal/query"
	"golang.org/x/sync/semaphore"
)

var ErrInvalidPhotoCheckout = errors.New("invalid photo checkout")

var ErrPhotoCheckoutSourceUnavailable = fmt.Errorf("%w: selected photo is hidden, excluded, detached, or trashed", ErrInvalidPhotoCheckout)

var ErrPhotoCheckoutPublicationUnrecorded = errors.New("installed checkout copy has not been recorded")

const StorageOperationKindPhotoCheckout = "photo_checkout"

type PhotoCheckoutYears struct {
	From int `json:"from" minimum:"1" maximum:"9999"`
	To   int `json:"to" minimum:"1" maximum:"9999"`
}

type PhotoCheckoutSelection struct {
	AssetIDs []string            `json:"asset_ids,omitzero" maxItems:"64"`
	AlbumIDs []string            `json:"album_ids,omitzero" maxItems:"64"`
	Years    *PhotoCheckoutYears `json:"years,omitzero"`
	All      bool                `json:"all,omitzero"`
}

type PhotoCheckoutEstimate struct {
	Assets int64 `json:"assets"`
	Files  int64 `json:"files"`
	Bytes  int64 `json:"bytes"`
}

type PhotoCheckout struct {
	RetiredAt         *string                `json:"retired_at"`
	RetentionReleased bool                   `json:"retention_released"`
	ID                string                 `json:"id"`
	Selection         PhotoCheckoutSelection `json:"selection"`
	ByteLimit         int64                  `json:"byte_limit"`
	CreatedAt         string                 `json:"created_at"`
	Destination       string                 `json:"destination"`
	OperationID       string                 `json:"operation_id"`
	State             string                 `json:"state"`
	Error             string                 `json:"error"`
}

type PhotoCheckoutEntry struct {
	SidecarOfID *string `json:"sidecar_of_file_id"`
	CheckoutID  string  `json:"checkout_id"`
	FileID      string  `json:"file_id"`
	AssetID     string  `json:"asset_id"`
	VersionID   string  `json:"version_id"`
	Hash        string  `json:"hash"`
	Size        int64   `json:"size"`
	Path        string  `json:"path"`
	Published   bool    `json:"published"`
}

type PhotoCheckoutStatus struct {
	Checkout       PhotoCheckout `json:"checkout"`
	Files          int64         `json:"files"`
	Bytes          int64         `json:"bytes"`
	PublishedFiles int64         `json:"published_files"`
	PublishedBytes int64         `json:"published_bytes"`
}

func ValidatePhotoCheckoutSelection(selection PhotoCheckoutSelection) error {
	if !selection.All && len(selection.AssetIDs)+len(selection.AlbumIDs) == 0 && selection.Years == nil {
		return fmt.Errorf("%w: select assets, albums, years, or all", ErrInvalidPhotoCheckout)
	}
	for _, ids := range [][]string{selection.AssetIDs, selection.AlbumIDs} {
		if len(ids) > 64 {
			return fmt.Errorf("%w: at most 64 IDs per selector", ErrInvalidPhotoCheckout)
		}
		for _, id := range ids {
			if validateUUIDv4(id) != nil {
				return fmt.Errorf("%w: invalid selection ID", ErrInvalidPhotoCheckout)
			}
		}
	}
	if y := selection.Years; y != nil && (y.From < 1 || y.To > 9999 || y.To < y.From) {
		return fmt.Errorf("%w: invalid inclusive year range", ErrInvalidPhotoCheckout)
	}
	return nil
}

func (s *Store) checkoutEntriesTx(ctx context.Context, tx *sql.Tx, selection PhotoCheckoutSelection) ([]PhotoCheckoutEntry, PhotoCheckoutEstimate, error) {
	var estimate PhotoCheckoutEstimate
	if err := ValidatePhotoCheckoutSelection(selection); err != nil {
		return nil, estimate, err
	}
	for _, id := range selection.AssetIDs {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_assets WHERE asset_id=?)`, id).Scan(&exists); err != nil {
			return nil, estimate, err
		}
		if !exists {
			return nil, estimate, ErrNotFound
		}
	}
	for _, id := range selection.AlbumIDs {
		if _, err := photoSetByID(ctx, tx, id); err != nil {
			return nil, estimate, err
		}
	}
	base := query.Query{V: 1, Syntax: "simple", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}}
	var queries []query.Query
	if selection.All {
		queries = append(queries, base)
	} else {
		if len(selection.AssetIDs) > 0 {
			q := base
			q.Filters.AssetIDs = selection.AssetIDs
			queries = append(queries, q)
		}
		if len(selection.AlbumIDs) > 0 {
			q := base
			q.Filters.SetIDs = selection.AlbumIDs
			queries = append(queries, q)
		}
		if y := selection.Years; y != nil {
			q := base
			q.Filters.CaptureAfter = fmt.Sprintf("%04d-01-01", y.From)
			if y.To < 9999 {
				q.Filters.CaptureBefore = fmt.Sprintf("%04d-01-01", y.To+1)
			}
			queries = append(queries, q)
		}
	}
	assets := map[string]bool{}
	for _, q := range queries {
		ids, err := s.photoSetSelectionIDs(ctx, tx, PhotoSetSelection{Query: &q})
		if err != nil {
			return nil, estimate, err
		}
		for _, id := range ids {
			assets[id] = true
		}
	}
	ids := make([]string, 0, len(assets))
	for id := range assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	entries := make([]PhotoCheckoutEntry, 0)
	encodedIDs, err := json.Marshal(ids)
	if err != nil {
		return nil, estimate, err
	}
	sources, err := photoSourcesTx(ctx, tx, string(encodedIDs), true)
	if err != nil {
		return nil, estimate, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT f.file_id,f.asset_id,v.version_id,v.blob_hash,v.size,n.name,COALESCE(p.capture_date,'')
 FROM photo_files f JOIN nodes n ON n.id=f.node_id JOIN content_versions v ON v.version_id=n.current_version_id
 JOIN photo_assets a ON a.asset_id=f.asset_id JOIN photo_files display ON display.file_id=a.display_file_id
 JOIN nodes display_node ON display_node.id=display.node_id JOIN content_versions display_version ON display_version.version_id=display_node.current_version_id
 LEFT JOIN source_metadata_heads h ON h.source_sha256=display_version.blob_hash LEFT JOIN photo_technical_metadata p ON p.generation_id=h.generation_id
 WHERE f.asset_id IN (SELECT value FROM json_each(?)) AND n.trashed_at IS NULL ORDER BY f.asset_id,f.file_id`, string(encodedIDs))
	if err != nil {
		return nil, estimate, err
	}
	defer func() { _ = rows.Close() }()
	var invalidNames []error
	for rows.Next() {
		var entry PhotoCheckoutEntry
		var name, date string
		if err := rows.Scan(&entry.FileID, &entry.AssetID, &entry.VersionID, &entry.Hash, &entry.Size, &name, &date); err != nil {
			_ = rows.Close()
			return nil, estimate, err
		}
		source := sources[entry.FileID]
		if source.target != "" {
			target := sources[source.target]
			if !target.eligible || target.asset != entry.AssetID {
				continue
			}
			entry.SidecarOfID = new(source.target)
		}
		if source.path != "" {
			name = path.Base(source.path)
		}
		if entry.Size < 0 || entry.Size > math.MaxInt64-estimate.Bytes {
			_ = rows.Close()
			return nil, estimate, fmt.Errorf("%w: selected bytes overflow", ErrInvalidPhotoCheckout)
		}
		folder := "undated"
		if len(date) >= 10 {
			folder = strings.ReplaceAll(date[:10], "-", "/")
		}
		if err := validateCheckoutFilename(name); err != nil {
			invalidNames = append(invalidNames, fmt.Errorf("file %s in asset %s, filename %q: %w", entry.FileID, entry.AssetID, name, err))
			continue
		}
		entry.Path = path.Join(folder, entry.AssetID, name)
		entries = append(entries, entry)
		estimate.Files++
		estimate.Bytes += entry.Size
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, estimate, err
	}
	if len(invalidNames) != 0 {
		return nil, estimate, errors.Join(invalidNames...)
	}
	estimate.Assets = int64(len(assets))
	return entries, estimate, nil
}

func (s *Store) EstimatePhotoCheckout(ctx context.Context, selection PhotoCheckoutSelection) (PhotoCheckoutEstimate, error) {
	var out PhotoCheckoutEstimate
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		_, estimate, err := s.checkoutEntriesTx(ctx, tx, selection)
		out = estimate
		return err
	})
	return out, err
}

// CreatePhotoCheckout freezes all bases and the local operation in one transaction.
func (s *Store) CreatePhotoCheckout(ctx context.Context, selection PhotoCheckoutSelection, destination string, byteLimit int64) (PhotoCheckout, StorageOperation, error) {
	var checkout PhotoCheckout
	var operation StorageOperation
	if byteLimit < 0 {
		return checkout, operation, fmt.Errorf("%w: byte_limit must be nonnegative", ErrInvalidPhotoCheckout)
	}
	if selection.All && byteLimit == 0 {
		return checkout, operation, fmt.Errorf("%w: all requires a positive byte_limit", ErrInvalidPhotoCheckout)
	}
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		entries, estimate, err := s.checkoutEntriesTx(ctx, tx, selection)
		if err != nil {
			return err
		}
		if byteLimit > 0 && estimate.Bytes > byteLimit {
			return fmt.Errorf("%w: selected bytes exceed byte_limit", ErrInvalidPhotoCheckout)
		}
		if len(entries) == 0 {
			return fmt.Errorf("%w: selection contains no files", ErrInvalidPhotoCheckout)
		}
		id, err := newUUIDv4()
		if err != nil {
			return err
		}
		request, err := json.Marshal(struct {
			CheckoutID string `json:"checkout_id"`
		}{id})
		if err != nil {
			return err
		}
		digest := sha256.Sum256(request)
		operation, err = createStorageOperationTx(ctx, tx, StorageOperationCreate{Kind: StorageOperationKindPhotoCheckout, RequestJSON: string(request), RequestDigest: hex.EncodeToString(digest[:]), PlanJSON: "{}", TotalObjects: estimate.Files})
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(selection)
		if err != nil {
			return err
		}
		checkout = PhotoCheckout{ID: id, Selection: selection, ByteLimit: byteLimit, CreatedAt: nowRFC3339(), Destination: destination, OperationID: operation.ID, State: "queued"}
		if _, err := tx.ExecContext(ctx, `INSERT INTO photo_checkouts(checkout_id,selection_json,byte_limit,created_at,destination,operation_id,state) VALUES(?,?,?,?,?,?,?)`, id, string(encoded), byteLimit, checkout.CreatedAt, destination, operation.ID, checkout.State); err != nil {
			return err
		}
		insert, err := tx.PrepareContext(ctx, `INSERT INTO photo_checkout_entries(checkout_id,file_id,asset_id,version_id,blob_hash,size,relative_path,sidecar_of_file_id) VALUES(?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer func() { _ = insert.Close() }()
		for _, e := range entries {
			if _, err := insert.ExecContext(ctx, id, e.FileID, e.AssetID, e.VersionID, e.Hash, e.Size, e.Path, e.SidecarOfID); err != nil {
				return err
			}
		}
		return nil
	})
	return checkout, operation, err
}

func validateCheckoutFilename(name string) error {
	if !utf8.ValidString(name) || name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("%w: invalid original filename", ErrInvalidPhotoCheckout)
	}
	if runtime.GOOS != "windows" {
		return nil
	}
	if strings.ContainsAny(name, `<>:"\|?*`) || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return fmt.Errorf("%w: original filename is not portable", ErrInvalidPhotoCheckout)
	}
	if filename.IsWindowsDeviceName(name) {
		return fmt.Errorf("%w: reserved Windows device filename", ErrInvalidPhotoCheckout)
	}
	for _, r := range name {
		if r >= 1 && r <= 31 {
			return fmt.Errorf("%w: original filename contains a control character", ErrInvalidPhotoCheckout)
		}
	}
	return nil
}

// Checkout metadata uses slash paths independently of the restore host.
func validateCheckoutPath(name string) error {
	if !fs.ValidPath(name) || name == "." || strings.ContainsRune(name, 0) {
		return ErrInvalidPhotoCheckout
	}
	return nil
}

const photoCheckoutSelect = `SELECT c.checkout_id,c.selection_json,c.byte_limit,c.created_at,COALESCE(c.destination,''),COALESCE(c.operation_id,''),COALESCE(o.state,c.state),COALESCE(o.error,c.error),c.retention_released,c.retired_at FROM photo_checkouts c LEFT JOIN storage_operations o ON o.operation_id=c.operation_id`

func scanPhotoCheckout(row scanner) (PhotoCheckout, error) {
	var out PhotoCheckout
	var encoded string
	if err := row.Scan(&out.ID, &encoded, &out.ByteLimit, &out.CreatedAt, &out.Destination, &out.OperationID, &out.State, &out.Error, &out.RetentionReleased, &out.RetiredAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, ErrNotFound
		}
		return out, err
	}
	return out, json.Unmarshal([]byte(encoded), &out.Selection)
}

func (s *Store) PhotoCheckout(ctx context.Context, id string) (PhotoCheckout, error) {
	return scanPhotoCheckout(s.db.QueryRowContext(ctx, photoCheckoutSelect+` WHERE c.checkout_id=?`, id))
}

func (s *Store) PhotoCheckoutStatus(ctx context.Context, id string) (PhotoCheckoutStatus, error) {
	var out PhotoCheckoutStatus
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		out.Checkout, err = scanPhotoCheckout(tx.QueryRowContext(ctx, photoCheckoutSelect+` WHERE c.checkout_id=?`, id))
		if err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(size),0),COALESCE(SUM(published),0),COALESCE(SUM(CASE WHEN published=1 THEN size ELSE 0 END),0) FROM photo_checkout_entries WHERE checkout_id=?`, id).Scan(&out.Files, &out.Bytes, &out.PublishedFiles, &out.PublishedBytes)
	})
	return out, err
}

func (s *Store) PhotoCheckoutOperation(ctx context.Context, id string) (PhotoCheckout, StorageOperation, error) {
	var checkout PhotoCheckout
	var operation StorageOperation
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		checkout, err = scanPhotoCheckout(tx.QueryRowContext(ctx, photoCheckoutSelect+` WHERE c.checkout_id=?`, id))
		if err != nil {
			return err
		}
		operation, err = scanStorageOperation(tx.QueryRowContext(ctx, storageOperationSelect+` WHERE operation_id=?`, checkout.OperationID))
		return err
	})
	return checkout, operation, err
}

func (s *Store) ListPhotoCheckouts(ctx context.Context) ([]PhotoCheckout, error) {
	rows, err := s.db.QueryContext(ctx, photoCheckoutSelect+` ORDER BY c.created_at,c.checkout_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]PhotoCheckout, 0)
	for rows.Next() {
		v, err := scanPhotoCheckout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) PhotoCheckoutEntries(ctx context.Context, id string) ([]PhotoCheckoutEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT checkout_id,file_id,asset_id,version_id,blob_hash,size,relative_path,published,sidecar_of_file_id FROM photo_checkout_entries WHERE checkout_id=? ORDER BY sidecar_of_file_id IS NOT NULL,relative_path,file_id`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]PhotoCheckoutEntry, 0)
	for rows.Next() {
		var e PhotoCheckoutEntry
		if err := rows.Scan(&e.CheckoutID, &e.FileID, &e.AssetID, &e.VersionID, &e.Hash, &e.Size, &e.Path, &e.Published, &e.SidecarOfID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type photoPublicationLock struct {
	gate  *semaphore.Weighted
	users int
}

// lockPhotoPublication orders eligibility changes against the no-replace install.
func (s *Store) lockPhotoPublication(ctx context.Context, assetID string) (func(), error) {
	s.photoPublicationMu.Lock()
	if s.photoPublicationLocks == nil {
		s.photoPublicationLocks = map[string]*photoPublicationLock{}
	}
	lock := s.photoPublicationLocks[assetID]
	if lock == nil {
		lock = &photoPublicationLock{gate: semaphore.NewWeighted(1)}
		s.photoPublicationLocks[assetID] = lock
	}
	lock.users++
	s.photoPublicationMu.Unlock()
	drop := func() {
		s.photoPublicationMu.Lock()
		lock.users--
		if lock.users == 0 {
			delete(s.photoPublicationLocks, assetID)
		}
		s.photoPublicationMu.Unlock()
	}
	if err := lock.gate.Acquire(ctx, 1); err != nil {
		drop()
		return nil, fmt.Errorf("waiting for photo publication: %w", err)
	}
	return func() { lock.gate.Release(1); drop() }, nil
}

// PublishPhotoCheckoutEntry checks visibility immediately before installing a copy.
func (s *Store) PublishPhotoCheckoutEntry(ctx context.Context, e PhotoCheckoutEntry, publish func() (bool, error)) error {
	// Subtree trash can invalidate members of several assets at once.
	s.photoPublicationTreeMu.RLock()
	defer s.photoPublicationTreeMu.RUnlock()
	unlock, err := s.lockPhotoPublication(ctx, e.AssetID)
	if err != nil {
		return err
	}
	defer unlock()
	var allowed bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_assets a JOIN photo_files f ON f.asset_id=a.asset_id JOIN nodes n ON n.id=f.node_id WHERE a.asset_id=? AND f.file_id=? AND a.hidden_at IS NULL AND a.excluded_at IS NULL AND n.trashed_at IS NULL
 AND f.sidecar_of_file_id IS ? AND (? IS NULL OR EXISTS(
 SELECT 1 FROM photo_files target JOIN nodes target_node ON target_node.id=target.node_id
 JOIN photo_checkout_entries frozen ON frozen.file_id=target.file_id AND frozen.checkout_id=?
 WHERE target.file_id=? AND target.asset_id=a.asset_id AND target_node.trashed_at IS NULL AND frozen.published=1)))`, e.AssetID, e.FileID, e.SidecarOfID, e.SidecarOfID, e.CheckoutID, e.SidecarOfID).Scan(&allowed); err != nil {
		return fmt.Errorf("checking checkout visibility: %w", err)
	}
	if !allowed {
		return ErrPhotoCheckoutSourceUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Recovery needs durable intent if install succeeds before its entry mark commits.
	result, err := s.writeDB.ExecContext(ctx, `UPDATE storage_operations SET cursor=?,updated_at=? WHERE operation_id=(SELECT operation_id FROM photo_checkouts WHERE checkout_id=?) AND state IN ('queued','running')`, e.FileID, nowRFC3339(), e.CheckoutID)
	if err := requireOneStorageOperationRow(result, err, e.CheckoutID, "recording checkout publication intent"); err != nil {
		return err
	}
	published, publicationErr := publish()
	if !published {
		_, err := s.writeDB.ExecContext(context.WithoutCancel(ctx), `UPDATE storage_operations SET cursor='' WHERE operation_id=(SELECT operation_id FROM photo_checkouts WHERE checkout_id=?) AND cursor=?`, e.CheckoutID, e.FileID)
		return errors.Join(publicationErr, err)
	}
	_, err = s.writeDB.ExecContext(context.WithoutCancel(ctx), `UPDATE photo_checkout_entries SET published=1 WHERE checkout_id=? AND file_id=?`, e.CheckoutID, e.FileID)
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrPhotoCheckoutPublicationUnrecorded, err)
	}
	return errors.Join(err, publicationErr)
}

// ClaimPhotoCheckout keeps the started marker when a paused operation returns to the queue.
func (s *Store) ClaimPhotoCheckout(ctx context.Context, id, operationID string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE storage_operations SET state='running',error='',updated_at=? WHERE operation_id=? AND state IN ('queued','running')`, nowRFC3339(), operationID)
		if err := requireOneStorageOperationRow(result, err, operationID, "claiming checkout"); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `UPDATE photo_checkouts SET state='running',error='' WHERE checkout_id=? AND operation_id=?`, id, operationID)
		return requireOneStorageOperationRow(result, err, operationID, "claiming checkout")
	})
}

func (s *Store) FinishPhotoCheckout(ctx context.Context, id, operationID string, state StorageOperationState, receipt, failure string, retention time.Time) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := finishStorageOperationTx(ctx, tx, operationID, state, receipt, failure, retention); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE photo_checkouts SET state=?,error=?,retention_released=? WHERE checkout_id=? AND operation_id=?`, string(state), boundedStorageOperationError(failure), state == StorageOperationFailed || state == StorageOperationCancelled, id, operationID)
		return requireOneStorageOperationRow(result, err, operationID, "finishing checkout")
	})
}

// ObservePhotoCheckout rebuilds machine-local copy observations from verified files.
func (s *Store) ObservePhotoCheckout(ctx context.Context, id, operationID string, fileIDs []string) error {
	encoded, err := json.Marshal(fileIDs)
	if err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE photo_checkout_entries SET published=(file_id IN (SELECT value FROM json_each(?))) WHERE checkout_id=?`, string(encoded), id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE storage_operations SET cursor='',completed_objects=(SELECT COUNT(*) FROM photo_checkout_entries WHERE checkout_id=? AND published=1),copied_objects=(SELECT COUNT(*) FROM photo_checkout_entries WHERE checkout_id=? AND published=1),copied_bytes=(SELECT COALESCE(SUM(size),0) FROM photo_checkout_entries WHERE checkout_id=? AND published=1) WHERE operation_id=?`, id, id, id, operationID)
		return err
	})
}

// PhotoCheckoutStarted survives operation deferral without marking never-admitted work started.
func (s *Store) PhotoCheckoutStarted(ctx context.Context, id string) (bool, error) {
	var started bool
	err := s.db.QueryRowContext(ctx, `SELECT state='running' FROM photo_checkouts WHERE checkout_id=?`, id).Scan(&started)
	return started, err
}

// RetirePhotoCheckout releases local holds without changing copies or job history.
func (s *Store) RetirePhotoCheckout(ctx context.Context, id string) (PhotoCheckout, error) {
	var out PhotoCheckout
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = scanPhotoCheckout(tx.QueryRowContext(ctx, photoCheckoutSelect+` WHERE c.checkout_id=?`, id))
		if err != nil {
			return err
		}
		switch out.State {
		case "unbound", "completed", "failed", "cancelled":
		default:
			return fmt.Errorf("%w: cancel active checkout work before retirement", ErrInvalidPhotoCheckout)
		}
		if out.RetiredAt != nil {
			return nil
		}
		retired := nowRFC3339()
		if _, err := tx.ExecContext(ctx, `UPDATE photo_checkouts SET retired_at=?,retention_released=1 WHERE checkout_id=?`, retired, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_checkout_entries SET base_held=0 WHERE checkout_id=?`, id); err != nil {
			return err
		}
		out.RetiredAt = &retired
		out.RetentionReleased = true
		return nil
	})
	return out, err
}

// e and c are the entry and checkout aliases in every retained-version query.
const photoCheckoutHoldPredicate = `c.retired_at IS NULL AND (c.retention_released=0 OR e.base_held=1 OR e.published=1 OR EXISTS(SELECT 1 FROM storage_operations pending WHERE pending.operation_id=c.operation_id AND pending.cursor=e.file_id))`
