package store

import (
	"context"
	"database/sql"
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
	"unicode/utf16"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/filename"
	"go.kenn.io/docbank/internal/query"
	"golang.org/x/sync/semaphore"
	"golang.org/x/text/unicode/norm"
)

var ErrInvalidPhotoCheckout = errors.New("invalid photo checkout")

var ErrPhotoCheckoutConflict = errors.New("photo checkout conflict")

var ErrPhotoCheckoutSourceUnavailable = fmt.Errorf("%w: selected photo is hidden, excluded, detached, or trashed", ErrInvalidPhotoCheckout)

var ErrPhotoCheckoutPublicationUnrecorded = errors.New("installed checkout copy has not been recorded")

const StorageOperationKindPhotoCheckout = "photo_checkout"
const StorageOperationKindPhotoCheckoutCommit = "photo_checkout_commit"
const StorageOperationKindPhotoCheckoutScan = "photo_checkout_scan"
const StorageOperationKindPhotoCheckoutRebind = "photo_checkout_rebind"

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

type PhotoCheckoutSkip struct {
	AssetID string `json:"asset_id"`
	FileID  string `json:"file_id"`
	Name    string `json:"name"`
	Reason  string `json:"reason"`
}

type PhotoCheckoutEstimate struct {
	SkippedPhotos     []PhotoCheckoutSkip `json:"skipped_photos" maxItems:"20"`
	SkippedPhotoCount int64               `json:"skipped_photo_count"`
	Assets            int64               `json:"assets"`
	Files             int64               `json:"files"`
	Bytes             int64               `json:"bytes"`
}

type PhotoCheckout struct {
	SkippedPhotos       []PhotoCheckoutSkip    `json:"skipped_photos" maxItems:"20"`
	SkippedPhotoCount   int64                  `json:"skipped_photo_count"`
	LastOperationState  string                 `json:"last_operation_state"`
	LastOperationID     string                 `json:"last_operation_id"`
	LastOperationKind   string                 `json:"last_operation_kind"`
	DestinationIdentity string                 `json:"-"`
	RetirementRequested bool                   `json:"retirement_requested"`
	RetiredAt           *string                `json:"retired_at"`
	DestinationReserved bool                   `json:"-"`
	RetentionReleased   bool                   `json:"retention_released"`
	ID                  string                 `json:"id"`
	Selection           PhotoCheckoutSelection `json:"selection"`
	ByteLimit           int64                  `json:"byte_limit"`
	CreatedAt           string                 `json:"created_at"`
	Destination         string                 `json:"destination"`
	OperationID         string                 `json:"operation_id"`
	State               string                 `json:"state"`
	Error               string                 `json:"error"`
}

type PhotoCheckoutEntry struct {
	ObservedIdentity string  `json:"-"`
	ObservedHash     string  `json:"observed_hash"`
	ObservedSize     int64   `json:"observed_size"`
	ObservedMtime    string  `json:"-"`
	ObservedSince    string  `json:"-"`
	WorkingState     string  `json:"working_state"`
	SidecarOfID      *string `json:"sidecar_of_file_id"`
	CheckoutID       string  `json:"checkout_id"`
	FileID           string  `json:"file_id"`
	AssetID          string  `json:"asset_id"`
	VersionID        string  `json:"version_id"`
	Hash             string  `json:"hash"`
	Size             int64   `json:"size"`
	Path             string  `json:"path"`
	Published        bool    `json:"published"`
}

type PhotoCheckoutStatus struct {
	Entries        []PhotoCheckoutEntry `json:"entries"`
	NextAfter      string               `json:"next_after,omitempty"`
	Checkout       PhotoCheckout        `json:"checkout"`
	Files          int64                `json:"files"`
	Bytes          int64                `json:"bytes"`
	PublishedFiles int64                `json:"published_files"`
	PublishedBytes int64                `json:"published_bytes"`
}

func ValidatePhotoCheckoutSelection(selection PhotoCheckoutSelection) error {
	if selection.All && (len(selection.AssetIDs)+len(selection.AlbumIDs) > 0 || selection.Years != nil) {
		return fmt.Errorf("%w: all cannot be combined with assets, albums, or years", ErrInvalidPhotoCheckout)
	}
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
	rows, err := tx.QueryContext(ctx, `SELECT f.file_id,f.asset_id,f.role,v.version_id,v.blob_hash,v.size,n.name,COALESCE(timeline.day,'')
 FROM photo_files f JOIN nodes n ON n.id=f.node_id JOIN content_versions v ON v.version_id=n.current_version_id
 JOIN photo_assets a ON a.asset_id=f.asset_id JOIN photo_files display ON display.file_id=a.display_file_id
 LEFT JOIN photo_timeline timeline ON timeline.file_id=display.file_id
 WHERE f.asset_id IN (SELECT value FROM json_each(?)) AND n.trashed_at IS NULL ORDER BY f.asset_id,f.file_id`, string(encodedIDs))
	if err != nil {
		return nil, estimate, err
	}
	defer func() { _ = rows.Close() }()
	type checkoutRow struct {
		entry      PhotoCheckoutEntry
		name, date string
	}
	var scanned []checkoutRow
	// Originals by asset and file, known before their sidecars are named.
	originals := map[string]map[string]checkoutOriginal{}
	for rows.Next() {
		var r checkoutRow
		var role string
		if err := rows.Scan(&r.entry.FileID, &r.entry.AssetID, &role, &r.entry.VersionID, &r.entry.Hash, &r.entry.Size, &r.name, &r.date); err != nil {
			_ = rows.Close()
			return nil, estimate, err
		}
		r.name = photoDeliveredName(sources, r.entry.FileID, r.name)
		if role != PhotoRoleSidecar {
			if originals[r.entry.AssetID] == nil {
				originals[r.entry.AssetID] = map[string]checkoutOriginal{}
			}
			originals[r.entry.AssetID][r.entry.FileID] = checkoutOriginal{r.name, role == PhotoRoleRAW}
		}
		scanned = append(scanned, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, estimate, err
	}
	skipped := map[string]PhotoCheckoutSkip{}
	pathOwners := map[string]string{}
	for _, r := range scanned {
		entry, name := r.entry, r.name
		source := sources[entry.FileID]
		if source.target != "" {
			target := sources[source.target]
			if !target.eligible || target.asset != entry.AssetID {
				continue
			}
			entry.SidecarOfID = new(source.target)
			if source.identity == "" {
				name = checkoutSidecarName(source.target, originals[entry.AssetID])
			}
		}
		folder := "undated"
		if len(r.date) >= 10 {
			folder = strings.ReplaceAll(r.date[:10], "-", "/")
		}
		if err := validateCheckoutFilename(name); err != nil {
			skipped[entry.AssetID] = PhotoCheckoutSkip{entry.AssetID, entry.FileID, name, err.Error()}
			continue
		}
		entry.Path = path.Join(folder, entry.AssetID, name)
		if owner, exists := pathOwners[entry.Path]; exists {
			skipped[entry.AssetID] = PhotoCheckoutSkip{entry.AssetID, entry.FileID, name, fmt.Sprintf("file %s has the same name", owner)}
			continue
		}
		pathOwners[entry.Path] = entry.FileID
		entries = append(entries, entry)
	}
	for _, id := range ids {
		if skip, exists := skipped[id]; exists {
			estimate.SkippedPhotoCount++
			if len(estimate.SkippedPhotos) < 20 {
				estimate.SkippedPhotos = append(estimate.SkippedPhotos, skip)
			}
		}
	}
	kept := entries[:0]
	clear(assets)
	for _, entry := range entries {
		if _, skip := skipped[entry.AssetID]; skip {
			continue
		}
		if entry.Size < 0 || entry.Size > math.MaxInt64-estimate.Bytes {
			return nil, estimate, fmt.Errorf("%w: selected bytes overflow", ErrInvalidPhotoCheckout)
		}
		kept = append(kept, entry)
		assets[entry.AssetID] = true
		estimate.Files++
		estimate.Bytes += entry.Size
	}
	estimate.Assets = int64(len(assets))
	if len(kept) == 0 {
		if estimate.SkippedPhotoCount > 0 {
			details := make([]string, 0, len(estimate.SkippedPhotos))
			for _, skip := range estimate.SkippedPhotos {
				details = append(details, fmt.Sprintf("photo %s, file %s (%q): %s", skip.AssetID, skip.FileID, skip.Name, skip.Reason))
			}
			return nil, estimate, fmt.Errorf("%w: all selected photos were skipped (%d): %s", ErrInvalidPhotoCheckout, estimate.SkippedPhotoCount, strings.Join(details, "; "))
		}
		return nil, estimate, fmt.Errorf("%w: selection contains no files", ErrInvalidPhotoCheckout)
	}
	return kept, estimate, nil
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

var errPhotoCheckoutSelectionStale = errors.New("photo checkout selection changed before creation")

// CreatePhotoCheckout freezes all bases and the local operation in one
// transaction. It resolves the selection before taking the write lock, because
// a full-library selection can take seconds, then verifies the resolved bases
// still exist. If pruning or purging removed one in between, it resolves again
// under the write lock.
func (s *Store) CreatePhotoCheckout(ctx context.Context, selection PhotoCheckoutSelection, destination, identity string, byteLimit int64) (PhotoCheckout, StorageOperation, PhotoCheckoutEstimate, error) {
	if byteLimit < 0 {
		return PhotoCheckout{}, StorageOperation{}, PhotoCheckoutEstimate{}, fmt.Errorf("%w: byte_limit must be nonnegative", ErrInvalidPhotoCheckout)
	}
	if selection.All && byteLimit == 0 {
		return PhotoCheckout{}, StorageOperation{}, PhotoCheckoutEstimate{}, fmt.Errorf("%w: all requires a positive byte_limit", ErrInvalidPhotoCheckout)
	}
	var entries []PhotoCheckoutEntry
	var estimate PhotoCheckoutEstimate
	if err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		entries, estimate, err = s.checkoutEntriesTx(ctx, tx, selection)
		return err
	}); err != nil {
		return PhotoCheckout{}, StorageOperation{}, PhotoCheckoutEstimate{}, err
	}
	checkout, operation, estimate, err := s.insertPhotoCheckout(ctx, selection, destination, identity, byteLimit, func(*sql.Tx) ([]PhotoCheckoutEntry, PhotoCheckoutEstimate, error) {
		return entries, estimate, nil
	})
	if errors.Is(err, errPhotoCheckoutSelectionStale) {
		checkout, operation, estimate, err = s.insertPhotoCheckout(ctx, selection, destination, identity, byteLimit, func(tx *sql.Tx) ([]PhotoCheckoutEntry, PhotoCheckoutEstimate, error) {
			return s.checkoutEntriesTx(ctx, tx, selection)
		})
	}
	return checkout, operation, estimate, err
}

func (s *Store) insertPhotoCheckout(ctx context.Context, selection PhotoCheckoutSelection, destination, identity string, byteLimit int64, resolve func(*sql.Tx) ([]PhotoCheckoutEntry, PhotoCheckoutEstimate, error)) (PhotoCheckout, StorageOperation, PhotoCheckoutEstimate, error) {
	var checkout PhotoCheckout
	var operation StorageOperation
	var estimate PhotoCheckoutEstimate
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		entries, resolved, err := resolve(tx)
		estimate = resolved
		if err != nil {
			return err
		}
		if byteLimit > 0 && estimate.Bytes > byteLimit {
			return fmt.Errorf("%w: selected bytes exceed byte_limit", ErrInvalidPhotoCheckout)
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
		input := localOperationCreate(StorageOperationKindPhotoCheckout, string(request))
		input.TotalObjects = estimate.Files
		operation, err = createStorageOperationTx(ctx, tx, input)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(selection)
		if err != nil {
			return err
		}
		skipped, err := json.Marshal(estimate.SkippedPhotos)
		if err != nil {
			return err
		}
		checkout = PhotoCheckout{SkippedPhotos: estimate.SkippedPhotos, SkippedPhotoCount: estimate.SkippedPhotoCount, ID: id, Selection: selection, ByteLimit: byteLimit, CreatedAt: nowRFC3339(), Destination: destination, OperationID: operation.ID, State: "queued", LastOperationKind: StorageOperationKindPhotoCheckout, LastOperationState: string(StorageOperationQueued), DestinationReserved: true, DestinationIdentity: identity}
		if checkout.SkippedPhotos == nil {
			checkout.SkippedPhotos = []PhotoCheckoutSkip{}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO photo_checkouts(checkout_id,selection_json,skipped_photos_json,skipped_photo_count,byte_limit,created_at,destination,operation_id,state,destination_reserved,destination_identity) VALUES(?,?,?,?,?,?,?,?,?,1,?)`, id, string(encoded), string(skipped), estimate.SkippedPhotoCount, byteLimit, checkout.CreatedAt, destination, operation.ID, checkout.State, identity); err != nil {
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
		var stale bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_checkout_entries e LEFT JOIN content_versions v ON v.version_id=e.version_id LEFT JOIN photo_files f ON f.file_id=e.file_id
 WHERE e.checkout_id=? AND (v.version_id IS NULL OR f.file_id IS NULL OR v.node_id<>f.node_id))`, id).Scan(&stale); err != nil {
			return err
		}
		if stale {
			return errPhotoCheckoutSelectionStale
		}
		return nil
	})
	return checkout, operation, estimate, err
}

func validateCheckoutFilename(name string) error {
	if !utf8.ValidString(name) || name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("%w: invalid original filename", ErrInvalidPhotoCheckout)
	}
	// Common filesystems cap a name at 255 bytes, or 255 UTF-16 units on Windows.
	length := len(name)
	if runtime.GOOS == "windows" {
		length = 0
		for _, r := range name {
			length += utf16.RuneLen(r)
		}
	}
	if length > 255 {
		return fmt.Errorf("%w: original filename is too long", ErrInvalidPhotoCheckout)
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

const photoCheckoutSelect = `SELECT c.checkout_id,c.selection_json,c.skipped_photos_json,c.skipped_photo_count,c.byte_limit,c.created_at,COALESCE(c.destination,''),COALESCE(c.operation_id,''),CASE WHEN COALESCE(c.destination,'')='' OR c.last_operation_id IS NOT NULL THEN c.state ELSE COALESCE(o.state,c.state) END,CASE WHEN c.last_operation_id IS NOT NULL AND o.state='failed' THEN o.error WHEN COALESCE(c.destination,'')='' OR c.last_operation_id IS NOT NULL THEN c.error ELSE COALESCE(o.error,c.error) END,c.retention_released,c.retired_at,c.destination_identity,c.retirement_requested,c.destination_reserved,COALESCE(c.last_operation_id,''),COALESCE(o.kind,''),COALESCE(o.state,'') FROM photo_checkouts c LEFT JOIN storage_operations o ON o.operation_id=COALESCE(c.last_operation_id,c.operation_id)`

func scanPhotoCheckout(row scanner) (PhotoCheckout, error) {
	var out PhotoCheckout
	var encoded, skipped string
	if err := row.Scan(&out.ID, &encoded, &skipped, &out.SkippedPhotoCount, &out.ByteLimit, &out.CreatedAt, &out.Destination, &out.OperationID, &out.State, &out.Error, &out.RetentionReleased, &out.RetiredAt, &out.DestinationIdentity, &out.RetirementRequested, &out.DestinationReserved, &out.LastOperationID, &out.LastOperationKind, &out.LastOperationState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, ErrNotFound
		}
		return out, err
	}
	if err := json.Unmarshal([]byte(skipped), &out.SkippedPhotos); err != nil {
		return out, err
	}
	return out, json.Unmarshal([]byte(encoded), &out.Selection)
}

func (s *Store) PhotoCheckout(ctx context.Context, id string) (PhotoCheckout, error) {
	return scanPhotoCheckout(s.db.QueryRowContext(ctx, photoCheckoutSelect+` WHERE c.checkout_id=?`, id))
}

func (s *Store) PhotoCheckoutStatus(ctx context.Context, id, after string, limit int) (PhotoCheckoutStatus, error) {
	var out PhotoCheckoutStatus
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		out.Checkout, err = scanPhotoCheckout(tx.QueryRowContext(ctx, photoCheckoutSelect+` WHERE c.checkout_id=?`, id))
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(size),0),COALESCE(SUM(published),0),COALESCE(SUM(CASE WHEN published=1 THEN size ELSE 0 END),0) FROM photo_checkout_entries WHERE checkout_id=?`, id).Scan(&out.Files, &out.Bytes, &out.PublishedFiles, &out.PublishedBytes); err != nil {
			return err
		}
		out.Entries, out.NextAfter, err = photoCheckoutEntriesPage(ctx, tx, id, after, limit)
		return err
	})
	return out, err
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

// PhotoCheckoutWorkingSummary counts all states and bounds the non-clean details.
func (s *Store) PhotoCheckoutWorkingSummary(ctx context.Context, id string, limit int) (map[string]int64, []PhotoCheckoutEntry, error) {
	if limit < 1 || limit > 250 {
		return nil, nil, ErrInvalidPhotoCheckout
	}
	counts := make(map[string]int64)
	var entries []PhotoCheckoutEntry
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT working_state,COUNT(*) FROM photo_checkout_entries WHERE checkout_id=? GROUP BY working_state`, id)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var state string
			var count int64
			if err := rows.Scan(&state, &count); err != nil {
				return err
			}
			counts[state] = count
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		entries, err = readPhotoCheckoutEntries(tx.QueryContext(ctx, photoCheckoutEntrySelect+` WHERE checkout_id=? AND working_state<>'clean' ORDER BY sidecar_of_file_id IS NOT NULL,relative_path,file_id LIMIT ?`, id, limit))
		return err
	})
	return counts, entries, err
}

func (s *Store) PhotoCheckoutEntries(ctx context.Context, id string) ([]PhotoCheckoutEntry, error) {
	rows, err := s.db.QueryContext(ctx, photoCheckoutEntrySelect+` WHERE checkout_id=? ORDER BY sidecar_of_file_id IS NOT NULL,relative_path,file_id`, id)
	return readPhotoCheckoutEntries(rows, err)
}

const photoCheckoutEntrySelect = `SELECT checkout_id,file_id,asset_id,version_id,blob_hash,size,relative_path,published,sidecar_of_file_id,observed_identity,observed_hash,observed_size,observed_mtime,observed_since,working_state FROM photo_checkout_entries`

func readPhotoCheckoutEntries(rows *sql.Rows, err error) ([]PhotoCheckoutEntry, error) {
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]PhotoCheckoutEntry, 0)
	for rows.Next() {
		var e PhotoCheckoutEntry
		if err := rows.Scan(&e.CheckoutID, &e.FileID, &e.AssetID, &e.VersionID, &e.Hash, &e.Size, &e.Path, &e.Published, &e.SidecarOfID, &e.ObservedIdentity, &e.ObservedHash, &e.ObservedSize, &e.ObservedMtime, &e.ObservedSince, &e.WorkingState); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func photoCheckoutEntriesPage(ctx context.Context, db interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, id, after string, limit int) ([]PhotoCheckoutEntry, string, error) {
	if limit < 1 || limit > 250 || after != "" && validateUUIDv4(after) != nil {
		return nil, "", ErrInvalidPhotoCheckout
	}
	entries, err := readPhotoCheckoutEntries(db.QueryContext(ctx, photoCheckoutEntrySelect+` WHERE checkout_id=? AND file_id>? ORDER BY file_id LIMIT ?`, id, after, limit+1))
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(entries) > limit {
		entries = entries[:limit]
		next = entries[len(entries)-1].FileID
	}
	return entries, next, nil
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

// CheckPhotoCheckoutEntry checks whether the frozen member is still eligible.
func (s *Store) CheckPhotoCheckoutEntry(ctx context.Context, e PhotoCheckoutEntry) error {
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
	return nil
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
	if err := s.CheckPhotoCheckoutEntry(ctx, e); err != nil {
		return err
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
	_, err = s.writeDB.ExecContext(context.WithoutCancel(ctx), `UPDATE photo_checkout_entries SET published=1,base_held=1 WHERE checkout_id=? AND file_id=?`, e.CheckoutID, e.FileID)
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
		if _, err := tx.ExecContext(ctx, `UPDATE photo_checkout_entries SET base_held=1 WHERE checkout_id=? AND (published=1 OR file_id=(SELECT cursor FROM storage_operations WHERE operation_id=?))`, id, operationID); err != nil {
			return err
		}
		if err := finishStorageOperationTx(ctx, tx, operationID, state, receipt, failure, retention); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE photo_checkouts SET state=?,error=?,retention_released=?,destination_reserved=? WHERE checkout_id=? AND operation_id=?`, string(state), boundedStorageOperationError(failure), state == StorageOperationFailed || state == StorageOperationCancelled, state != StorageOperationFailed && state != StorageOperationCancelled, id, operationID)
		if err := requireOneStorageOperationRow(result, err, operationID, "finishing checkout"); err != nil {
			return err
		}
		return retireRequestedCheckoutTx(ctx, tx, id)
	})
}

// PreserveUncertainPhotoCheckoutHolds marks every entry that is currently
// published, or that the operation was mid-copy on, as independently held.
// Call it before an ObservePhotoCheckout that may clear those entries'
// published flag because their current state can no longer be confirmed
// (a moved or inaccessible destination, or a file missing from an
// otherwise-reachable one): base_held keeps photoCheckoutHoldPredicate
// protecting their source versions until the checkout is retired, even
// after publication can no longer be observed directly.
func (s *Store) PreserveUncertainPhotoCheckoutHolds(ctx context.Context, id, operationID string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE photo_checkout_entries SET base_held=1 WHERE checkout_id=? AND (published=1 OR file_id=(SELECT cursor FROM storage_operations WHERE operation_id=? AND cursor<>''))`, id, operationID)
		return err
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
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = scanPhotoCheckout(tx.QueryRowContext(ctx, photoCheckoutSelect+` WHERE c.checkout_id=?`, id))
		if err != nil {
			return err
		}
		switch out.LastOperationState {
		case string(StorageOperationQueued), string(StorageOperationRunning):
			_, err := tx.ExecContext(ctx, `UPDATE photo_checkouts SET retirement_requested=1 WHERE checkout_id=?`, id)
			out.RetirementRequested = true
			return err
		case "", "completed", exportFailedState, "cancelled":
		default:
			return fmt.Errorf("%w: checkout cannot retire in its current state", ErrInvalidPhotoCheckout)
		}
		if out.RetiredAt != nil {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_checkouts SET retirement_requested=1 WHERE checkout_id=?`, id); err != nil {
			return err
		}
		if err := retireRequestedCheckoutTx(ctx, tx, id); err != nil {
			return err
		}
		out, err = scanPhotoCheckout(tx.QueryRowContext(ctx, photoCheckoutSelect+` WHERE c.checkout_id=?`, id))
		return err
	})
	return out, err
}

// ReleaseCheckoutDestination clears a retired checkout's destination once its
// folder has been confirmed empty and admitted for reuse by a new store
// registration. Persisting the release here, before that registration writes
// into the folder, keeps a later admission check from mistaking the new
// store's own data for leftover checkout content and rejecting a reuse it
// already approved.
func (s *Store) ReleaseCheckoutDestination(ctx context.Context, id string) error {
	_, err := s.writeDB.ExecContext(ctx, `UPDATE photo_checkouts SET destination='' WHERE checkout_id=? AND destination_reserved=0`, id)
	return err
}

// e and c are the entry and checkout aliases in every retained-version query.
const photoCheckoutHoldPredicate = `c.retired_at IS NULL AND (c.retention_released=0 OR e.base_held=1 OR EXISTS(SELECT 1 FROM storage_operations pending WHERE pending.operation_id=c.operation_id AND pending.cursor=e.file_id))`

func (c PhotoCheckout) ActiveOperationID() string {
	if c.LastOperationID != "" {
		return c.LastOperationID
	}
	return c.OperationID
}

type checkoutOriginal struct {
	name string
	raw  bool
}

// photoDeliveredName is the name checkout gives a file: its source path's base, else its node name.
func photoDeliveredName(sources map[string]photoSource, fileID, nodeName string) string {
	if p := sources[fileID].path; p != "" {
		return path.Base(p)
	}
	return nodeName
}

// checkoutSidecarName names a sidecar with no source filename after the original it ships beside.
// It is <stem>.xmp, which Adobe apps read, for the only original or only RAW with that stem, as import folds names.
func checkoutSidecarName(target string, originals map[string]checkoutOriginal) string {
	original := originals[target]
	stem := strings.TrimSuffix(original.name, path.Ext(original.name))
	for id, other := range originals {
		if id != target && (other.raw || !original.raw) && strings.EqualFold(norm.NFC.String(strings.TrimSuffix(other.name, path.Ext(other.name))), norm.NFC.String(stem)) {
			return original.name + ".xmp"
		}
	}
	return stem + ".xmp"
}
