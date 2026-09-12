package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/canonical"
)

const (
	exportFailedState     = "failed"
	exportQueuedState     = "queued"
	exportRunningState    = "running"
	exportSealedState     = "sealed"
	exportRoleAvailable   = "available"
	exportRoleUnavailable = "unavailable"
)

// ExportMemberHash shares the snapshot membership algorithm. Multiple retained
// versions of a node are distinct members; ordering is numeric node then version.
func ExportMemberHash(members []bundle.Member) string {
	converted := make([]SnapshotMember, len(members))
	for i, m := range members {
		converted[i] = SnapshotMember{NodeID: m.NodeID, ContentVersionID: m.VersionID}
	}
	return snapshotMemberHash(converted)
}

func exportTime(t time.Time) string         { return t.UTC().Format(timestampLayout) }
func exportDeadline(d time.Duration) string { return exportTime(time.Now().Add(d)) }
func exportExpired(value string) bool {
	t, err := time.Parse(timestampLayout, value)
	return err != nil || !time.Now().Before(t)
}

func validateExportSource(r bundle.SourceRequest) error {
	if validateUUIDv4(r.OperationID) != nil {
		return bundle.ErrConflict
	}
	choices := 0
	if len(r.Members) > 0 {
		choices++
	}
	if len(r.NodeIDs) > 0 {
		choices++
	}
	if r.Query != nil {
		choices++
	}
	if r.SavedQueryID != "" || r.SavedQueryRevision != 0 {
		choices++
	}
	if r.SnapshotID != "" {
		choices++
	}
	if r.Kind == "upload" {
		choices++
	}
	if choices != 1 {
		return bundle.ErrConflict
	}
	switch r.Kind {
	case "explicit":
		if len(r.Members) == 0 || len(r.Members) > bundle.ChunkMembers {
			return bundle.ErrLimit
		}
	case "nodes":
		if len(r.NodeIDs) == 0 || len(r.NodeIDs) > bundle.ChunkMembers {
			return bundle.ErrLimit
		}
	case "query":
		if r.Query == nil {
			return bundle.ErrConflict
		}
	case "saved_query":
		if validateUUIDv4(r.SavedQueryID) != nil || r.SavedQueryRevision < 1 {
			return bundle.ErrConflict
		}
	case "snapshot":
		if r.SnapshotID == "" || !canonical.IsSHA256Hex(r.MemberHash) {
			return bundle.ErrConflict
		}
	case "upload":
		if r.Total < 1 || r.Total > bundle.MaxMembers || !canonical.IsSHA256Hex(r.MemberHash) {
			return bundle.ErrLimit
		}
	default:
		return bundle.ErrConflict
	}
	if r.Kind != "upload" && r.Total != 0 || r.Kind != "upload" && r.Kind != "snapshot" && r.MemberHash != "" {
		return bundle.ErrConflict
	}
	return nil
}

func loadExportSource(ctx context.Context, q metadataQuerier, owner, id string) (bundle.Source, error) {
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT canonical_json FROM export_sources WHERE id=? AND owner=?`, id, owner).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return bundle.Source{}, ErrNotFound
	}
	if err != nil {
		return bundle.Source{}, err
	}
	var source bundle.Source
	if len(raw) > bundle.MaxMemberBytes {
		return source, bundle.ErrLimit
	}
	err = json.Unmarshal(raw, &source, json.RejectUnknownMembers(true))
	return source, err
}

func (s *Store) ExportSource(ctx context.Context, owner, id string) (bundle.Source, error) {
	source, err := loadExportSource(ctx, s.db, owner, id)
	if err == nil && exportExpired(source.ExpiresAt) {
		err = bundle.ErrExpired
	}
	return source, err
}

// CreateExportSource durably reserves idempotency before any query resolution.
// A failed/interrupted resolution cannot be rerun under the same operation ID.
func (s *Store) CreateExportSource(ctx context.Context, owner string, r bundle.SourceRequest, resolve func(context.Context) ([]bundle.Member, error)) (bundle.Source, error) {
	return s.createExportSource(ctx, owner, r, resolve, nil)
}

// CreateResolvedExportSource retains the query fingerprint returned by the
// snapshot resolver in the same seal transaction as its exact membership.
func (s *Store) CreateResolvedExportSource(ctx context.Context, owner string, r bundle.SourceRequest, resolve func(context.Context) ([]bundle.Member, string, error)) (bundle.Source, error) {
	var fingerprint string
	var resolver func(context.Context) ([]bundle.Member, error)
	if resolve != nil {
		resolver = func(ctx context.Context) ([]bundle.Member, error) {
			members, actual, err := resolve(ctx)
			fingerprint = actual
			return members, err
		}
	}
	return s.createExportSource(ctx, owner, r, resolver, func() string { return fingerprint })
}

func (s *Store) createExportSource(ctx context.Context, owner string, r bundle.SourceRequest, resolve func(context.Context) ([]bundle.Member, error), queryFingerprint func() string) (bundle.Source, error) {
	if owner == "" {
		return bundle.Source{}, ErrNotFound
	}
	if err := validateExportSource(r); err != nil {
		return bundle.Source{}, err
	}
	request, err := canonical.Marshal(r)
	if err != nil {
		return bundle.Source{}, err
	}
	if len(request) > 1<<20 {
		return bundle.Source{}, bundle.ErrLimit
	}
	digest := pageChecksum(request)
	var source bundle.Source
	created := false
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		previous, e := loadExportSource(ctx, tx, owner, r.OperationID)
		if e == nil {
			if previous.RequestSHA256 != digest {
				return bundle.ErrConflict
			}
			if exportExpired(previous.ExpiresAt) {
				return bundle.ErrExpired
			}
			source = previous
			return nil
		}
		if !errors.Is(e, ErrNotFound) {
			return e
		}
		var exists bool
		if e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM export_sources WHERE id=?)`, r.OperationID).Scan(&exists); e != nil {
			return e
		}
		if exists {
			return ErrNotFound
		}
		var count int
		if e = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM export_sources)+(SELECT count(*) FROM export_plans)`).Scan(&count); e != nil {
			return e
		}
		if count >= 32 {
			return bundle.ErrLimit
		}
		state := "resolving"
		if r.Kind == "upload" {
			state = "uploading"
		}
		source = bundle.Source{ID: r.OperationID, RequestSHA256: digest, Kind: r.Kind, State: state, Total: r.Total, MemberHash: r.MemberHash, CreatedAt: nowRFC3339(), ExpiresAt: exportDeadline(10 * time.Minute)}
		source.SavedQueryID = r.SavedQueryID
		source.SavedQueryRevision = r.SavedQueryRevision
		raw, e := canonical.Marshal(source)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO export_sources(id,owner,request_sha256,request_json,canonical_json,state,expires_at) VALUES(?,?,?,?,?,?,?)`, source.ID, owner, digest, request, raw, state, source.ExpiresAt)
		created = e == nil
		return e
	})
	if err != nil {
		return source, err
	}
	if !created {
		if source.State == "resolving" || source.State == exportFailedState {
			return source, bundle.ErrConflict
		}
		return source, nil
	}
	if r.Kind == "upload" {
		return source, nil
	}
	var members []bundle.Member
	if resolve != nil {
		members, err = resolve(ctx)
	} else if r.Kind == "explicit" {
		members = r.Members
	} else if r.Kind == "nodes" {
		for _, id := range r.NodeIDs {
			var n Node
			n, err = s.NodeByID(ctx, id)
			if err != nil {
				break
			}
			members = append(members, bundle.Member{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size})
		}
	} else {
		err = bundle.ErrConflict
	}
	if queryFingerprint != nil {
		source.QueryFingerprint = queryFingerprint()
		if source.QueryFingerprint != "" && validateSavedQueryRunFingerprint(source.QueryFingerprint, "export query fingerprint") != nil {
			err = bundle.ErrConflict
		}
	}
	if err == nil {
		err = s.withStorageTx(ctx, func(tx *sql.Tx) error { return sealExportMembers(ctx, tx, owner, &source, members) })
	}
	if err != nil {
		// The durable reservation is retained, including after caller cancellation.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		source.State = exportFailedState
		raw, _ := canonical.Marshal(source)
		_, markErr := s.db.ExecContext(cleanup, `UPDATE export_sources SET state='failed',canonical_json=? WHERE id=? AND state='resolving'`, raw, source.ID)
		return source, errors.Join(err, markErr)
	}
	return source, nil
}

func validateExportMembers(members []bundle.Member) error {
	if len(members) < 1 || len(members) > bundle.MaxMembers {
		return bundle.ErrLimit
	}
	var total int64
	seen := make(map[string]struct{}, len(members))
	for _, m := range members {
		if m.NodeID < 1 || validateUUIDv4(m.VersionID) != nil || !canonical.IsSHA256Hex(m.SHA256) || m.Size < 0 || m.Revision < 0 {
			return bundle.ErrConflict
		}
		key := fmt.Sprintf("%d:%s", m.NodeID, m.VersionID)
		if _, exists := seen[key]; exists {
			return bundle.ErrConflict
		}
		seen[key] = struct{}{}
		if m.Size > bundle.MaxRoleBytes-total {
			return bundle.ErrLimit
		}
		total += m.Size
	}
	return nil
}

func sealExportMembers(ctx context.Context, tx *sql.Tx, owner string, source *bundle.Source, members []bundle.Member) error {
	if err := validateExportMembers(members); err != nil {
		return err
	}
	if exportExpired(source.ExpiresAt) {
		return bundle.ErrExpired
	}
	current, err := loadExportSource(ctx, tx, owner, source.ID)
	if err != nil {
		return err
	}
	if current.State != "resolving" && current.State != "uploading" {
		return bundle.ErrConflict
	}
	members = slices.Clone(members)
	slices.SortFunc(members, func(a, b bundle.Member) int {
		if a.NodeID < b.NodeID {
			return -1
		}
		if a.NodeID > b.NodeID {
			return 1
		}
		return strings.Compare(a.VersionID, b.VersionID)
	})
	hash := ExportMemberHash(members)
	if source.Kind == "upload" && (source.Total != len(members) || source.MemberHash != hash) {
		return bundle.ErrConflict
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO export_members(source_id,node_id,version_id,blob_hash,canonical_json) VALUES(?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	var total int64
	for _, m := range members {
		var nodeID, revision, size int64
		var hash string
		var trash sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT v.node_id,v.blob_hash,v.size,n.revision,n.trashed_at FROM content_versions v JOIN nodes n ON n.id=v.node_id WHERE v.version_id=?`, m.VersionID).Scan(&nodeID, &hash, &size, &revision, &trash)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if trash.Valid {
			return ErrNotFound
		}
		if nodeID != m.NodeID || hash != m.SHA256 || size != m.Size || m.Revision != 0 && m.Revision != revision {
			return bundle.ErrConflict
		}
		raw, e := canonical.Marshal(m)
		if e != nil {
			return e
		}
		if _, e = stmt.ExecContext(ctx, source.ID, m.NodeID, m.VersionID, m.SHA256, raw); e != nil {
			return e
		}
		total += m.Size
	}
	source.State = exportSealedState
	source.Total = len(members)
	source.MemberHash = hash
	source.SourceBytes = total
	source.ExpiresAt = exportDeadline(10 * time.Minute)
	raw, err := canonical.Marshal(source)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE export_sources SET canonical_json=?,state='sealed',expires_at=? WHERE id=?`, raw, source.ExpiresAt, source.ID)
	return err
}

func (s *Store) PutExportChunk(ctx context.Context, owner, id string, index int, members []bundle.Member) error {
	if len(members) > bundle.ChunkMembers {
		return bundle.ErrLimit
	}
	if err := validateExportMembers(members); err != nil {
		return err
	}
	raw, err := canonical.Marshal(members)
	if err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		source, err := loadExportSource(ctx, tx, owner, id)
		if err != nil {
			return err
		}
		if exportExpired(source.ExpiresAt) {
			return bundle.ErrExpired
		}
		if source.Kind != "upload" || index < 0 || index >= (source.Total+999)/1000 {
			return bundle.ErrConflict
		}
		var prior []byte
		err = tx.QueryRowContext(ctx, `SELECT canonical_json FROM export_chunks WHERE source_id=? AND chunk_index=?`, id, index).Scan(&prior)
		if err == nil {
			if bytes.Equal(prior, raw) {
				return nil
			}
			return bundle.ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if source.State != "uploading" {
			return bundle.ErrConflict
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO export_chunks(source_id,chunk_index,canonical_json) VALUES(?,?,?)`, id, index, raw)
		return err
	})
}

func (s *Store) SealExportSource(ctx context.Context, owner, id string) (bundle.Source, error) {
	var source bundle.Source
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		source, err = loadExportSource(ctx, tx, owner, id)
		if err != nil {
			return err
		}
		if exportExpired(source.ExpiresAt) {
			return bundle.ErrExpired
		}
		if source.State == exportSealedState {
			return nil
		}
		if source.State != "uploading" {
			return bundle.ErrConflict
		}
		members := make([]bundle.Member, 0, source.Total)
		for i := range (source.Total + 999) / 1000 {
			var raw []byte
			err = tx.QueryRowContext(ctx, `SELECT canonical_json FROM export_chunks WHERE source_id=? AND chunk_index=?`, id, i).Scan(&raw)
			if errors.Is(err, sql.ErrNoRows) {
				return bundle.ErrConflict
			}
			if err != nil {
				return err
			}
			if len(raw) > 1<<20 {
				return bundle.ErrLimit
			}
			var chunk []bundle.Member
			if err = json.Unmarshal(raw, &chunk, json.RejectUnknownMembers(true)); err != nil {
				return err
			}
			want := min(1000, source.Total-i*1000)
			if len(chunk) != want {
				return bundle.ErrConflict
			}
			members = append(members, chunk...)
		}
		return sealExportMembers(ctx, tx, owner, &source, members)
	})
	return source, err
}

func checkExportVersionsRetained(ctx context.Context, q metadataQuerier, versions []string) error {
	for _, id := range versions {
		var expiry string
		err := q.QueryRowContext(ctx, `SELECT s.expires_at FROM export_sources s JOIN export_members m ON m.source_id=s.id WHERE m.version_id=? ORDER BY s.expires_at DESC LIMIT 1`, id).Scan(&expiry)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("%w until %s; cancel the export or wait for retention cleanup", bundle.ErrRetained, expiry)
	}
	return nil
}
