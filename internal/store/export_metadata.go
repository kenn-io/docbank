package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/canonical"
)

const metadataExportType = "export_authority"

type metadataExportRecord struct {
	Type          string         `json:"type"`
	Kind          string         `json:"kind"`
	ID            string         `json:"id"`
	Ordinal       int            `json:"ordinal"`
	RetainUntil   string         `json:"retain_until"`
	CanonicalJSON jsontext.Value `json:"canonical_json"`
	Checksum      string         `json:"checksum"`
}

func exportBundleMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	var count int
	if err := q.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM export_sources)+(SELECT count(*) FROM export_plans)`).Scan(&count); err != nil {
		return err
	}
	if count > 32 {
		return bundle.ErrLimit
	}
	emit := func(kind, id string, ordinal int, until string, raw []byte) error {
		if len(raw) > bundle.MaxMemberBytes {
			return bundle.ErrLimit
		}
		return write(metadataExportRecord{Type: metadataExportType, Kind: kind, ID: id, Ordinal: ordinal, RetainUntil: until, CanonicalJSON: raw, Checksum: pageChecksum(raw)})
	}
	ids, err := pageMetadataKeys(ctx, q, `SELECT id FROM export_sources WHERE state='sealed' ORDER BY id`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		var raw []byte
		var until string
		if err = q.QueryRowContext(ctx, `SELECT canonical_json,expires_at FROM export_sources WHERE id=?`, id).Scan(&raw, &until); err != nil {
			return err
		}
		var source bundle.Source
		if err = json.Unmarshal(raw, &source, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		if source.ID != id || source.State != exportSealedState || !canonical.IsSHA256Hex(source.RequestSHA256) || source.Total < 1 || source.Total > bundle.MaxMembers {
			return bundle.ErrConflict
		}
		if _, err = time.Parse(timestampLayout, until); err != nil {
			return fmt.Errorf("export retention timestamp: %w", err)
		}
		if err = emit("source", id, 0, until, raw); err != nil {
			return err
		}
		members := make([]bundle.Member, 0, source.Total)
		err = walkExportMembers(ctx, q, id, func(m bundle.Member) error {
			var nodeID, size int64
			var hash string
			if err := q.QueryRowContext(ctx, `SELECT node_id,blob_hash,size FROM content_versions WHERE version_id=?`, m.VersionID).Scan(&nodeID, &hash, &size); err != nil {
				return err
			}
			if nodeID != m.NodeID || hash != m.SHA256 || size != m.Size {
				return bundle.ErrConflict
			}
			members = append(members, m)
			if len(members) > bundle.MaxMembers {
				return bundle.ErrLimit
			}
			raw, err := canonical.Marshal(m)
			if err != nil {
				return err
			}
			return emit("member", id, len(members)-1, "", raw)
		})
		if err != nil {
			return err
		}
		if err = validateExportMembers(members); err != nil {
			return err
		}
		var sourceBytes int64
		for _, m := range members {
			sourceBytes += m.Size
		}
		if sourceBytes != source.SourceBytes {
			return bundle.ErrConflict
		}
		if len(members) != source.Total || ExportMemberHash(members) != source.MemberHash {
			return bundle.ErrConflict
		}
	}
	ids, err = pageMetadataKeys(ctx, q, `SELECT id FROM export_plans ORDER BY id`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		plan, err := loadExportPlan(ctx, q, id)
		if err != nil {
			return err
		}
		if plan.Fingerprint == "" {
			continue
		}
		if plan.Format != bundle.Format || validateExportPolicies(plan.Roles) != nil || plan.Total < 1 || plan.Total > bundle.MaxMembers || plan.RoleEntries > bundle.MaxRoles || plan.RoleBytes > bundle.MaxRoleBytes {
			return bundle.ErrConflict
		}
		if plan.Total != plan.Source.Total {
			return bundle.ErrConflict
		}
		var raw []byte
		var until, sourceID string
		if err = q.QueryRowContext(ctx, `SELECT canonical_json,expires_at,source_id FROM export_plans WHERE id=?`, id).Scan(&raw, &until, &sourceID); err != nil {
			return err
		}
		if sourceID != plan.Source.ID {
			return bundle.ErrConflict
		}
		var sourceRaw []byte
		if err = q.QueryRowContext(ctx, `SELECT canonical_json FROM export_sources WHERE id=?`, sourceID).Scan(&sourceRaw); err != nil {
			return err
		}
		var source bundle.Source
		if err = json.Unmarshal(sourceRaw, &source); err != nil {
			return err
		}
		if source != plan.Source {
			return bundle.ErrConflict
		}
		if err = emit("plan", id, 0, until, raw); err != nil {
			return err
		}
		var rowCount, firstOrdinal, lastOrdinal int
		if err = q.QueryRowContext(ctx, `SELECT count(*),COALESCE(min(ordinal),-1),COALESCE(max(ordinal),-1) FROM export_documents WHERE plan_id=?`, id).Scan(&rowCount, &firstOrdinal, &lastOrdinal); err != nil {
			return err
		}
		if rowCount != plan.Total || firstOrdinal != 0 || lastOrdinal != plan.Total-1 {
			return bundle.ErrConflict
		}
		index, entries := 0, 0
		roots := make(map[string]struct{})
		var roleBytes int64
		err = walkExportDocumentBytes(ctx, q, id, func(raw []byte) error {
			var d bundle.Document
			if err := json.Unmarshal(raw, &d, json.RejectUnknownMembers(true)); err != nil {
				return err
			}
			var member []byte
			if err := q.QueryRowContext(ctx, `SELECT canonical_json FROM export_members WHERE source_id=? AND node_id=? AND version_id=?`, sourceID, d.NodeID, d.VersionID).Scan(&member); err != nil {
				return err
			}
			var m bundle.Member
			if err := json.Unmarshal(member, &m); err != nil {
				return err
			}
			if m != d.Member {
				return bundle.ErrConflict
			}
			for _, r := range d.Roles {
				if r.Status == exportRoleUnavailable {
					if r.Path != "" || r.SHA256 != "" || r.Size != 0 {
						return bundle.ErrConflict
					}
					continue
				}
				if r.Status != exportRoleAvailable || !canonical.IsSHA256Hex(r.SHA256) || r.Size < 0 {
					return bundle.ErrConflict
				}
				var size int64
				if err := q.QueryRowContext(ctx, `SELECT b.size FROM blobs b JOIN export_role_roots r ON r.blob_hash=b.hash WHERE r.plan_id=? AND r.blob_hash=?`, id, r.SHA256).Scan(&size); err != nil {
					return err
				}
				if size != r.Size {
					return bundle.ErrConflict
				}
				entries++
				roots[r.SHA256] = struct{}{}
				roleBytes += size
			}
			if err := emit("document", id, index, "", raw); err != nil {
				return err
			}
			index++
			return nil
		})
		if err != nil {
			return err
		}
		if index != plan.Total || entries != plan.RoleEntries || roleBytes != plan.RoleBytes {
			return bundle.ErrConflict
		}
		var rootCount int
		if err = q.QueryRowContext(ctx, `SELECT count(*) FROM export_role_roots WHERE plan_id=?`, id).Scan(&rootCount); err != nil {
			return err
		}
		if rootCount != len(roots) {
			return bundle.ErrConflict
		}
		actual, err := exportPlanFingerprint(ctx, q, plan)
		if err != nil {
			return err
		}
		if actual != plan.Fingerprint {
			return bundle.ErrConflict
		}
	}
	return nil
}

func importBundleMetadata(ctx context.Context, tx *sql.Tx, raw jsontext.Value) error {
	var r metadataExportRecord
	if err := decodeMetadataRecord(raw, &r); err != nil {
		return err
	}
	if r.Type != metadataExportType || validateUUIDv4(r.ID) != nil || len(r.CanonicalJSON) > bundle.MaxMemberBytes || pageChecksum(r.CanonicalJSON) != r.Checksum {
		return bundle.ErrConflict
	}
	if r.Kind == "source" || r.Kind == "plan" {
		if _, err := time.Parse(timestampLayout, r.RetainUntil); err != nil {
			return fmt.Errorf("export retention timestamp: %w", err)
		}
	} else if r.RetainUntil != "" {
		return bundle.ErrConflict
	}
	switch r.Kind {
	case "source":
		var s bundle.Source
		if err := json.Unmarshal(r.CanonicalJSON, &s, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		if s.ID != r.ID || s.State != exportSealedState || s.Total < 1 || s.Total > bundle.MaxMembers {
			return bundle.ErrConflict
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO export_sources(id,owner,request_sha256,request_json,canonical_json,state,expires_at) VALUES(?,'',?,'{}',?,'sealed',?)`, r.ID, s.RequestSHA256, []byte(r.CanonicalJSON), r.RetainUntil)
		return err
	case "member":
		var m bundle.Member
		if err := json.Unmarshal(r.CanonicalJSON, &m, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		if err := validateExportMembers([]bundle.Member{m}); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO export_members(source_id,node_id,version_id,blob_hash,canonical_json) VALUES(?,?,?,?,?)`, r.ID, m.NodeID, m.VersionID, m.SHA256, []byte(r.CanonicalJSON))
		return err
	case "plan":
		var p bundle.Plan
		if err := json.Unmarshal(r.CanonicalJSON, &p, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		if p.ID != r.ID || !canonical.IsSHA256Hex(p.Fingerprint) {
			return bundle.ErrConflict
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO export_plans(id,owner,source_id,request_sha256,canonical_json,expires_at) VALUES(?,'',?,'',?,?)`, r.ID, p.Source.ID, []byte(r.CanonicalJSON), r.RetainUntil)
		return err
	case "document":
		if r.Ordinal < 0 || r.Ordinal >= bundle.MaxMembers {
			return bundle.ErrLimit
		}
		var d bundle.Document
		if err := json.Unmarshal(r.CanonicalJSON, &d, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO export_documents(plan_id,ordinal,canonical_json) VALUES(?,?,?)`, r.ID, r.Ordinal, []byte(r.CanonicalJSON)); err != nil {
			return err
		}
		for _, role := range d.Roles {
			if role.Status == exportRoleAvailable {
				if _, err := tx.ExecContext(ctx, `INSERT INTO export_role_roots(plan_id,blob_hash) VALUES(?,?) ON CONFLICT DO NOTHING`, r.ID, role.SHA256); err != nil {
					return err
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown export metadata record", bundle.ErrConflict)
	}
}

func checkExportPurgeRetained(ctx context.Context, q metadataQuerier, request PurgeRequest) error {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT a.content_version_id,a.attachment_id,a.build_id,s.expires_at FROM export_role_roots r JOIN export_plans p ON p.id=r.plan_id JOIN export_sources s ON s.id=p.source_id JOIN rendition_artifacts x ON x.blob_hash=r.blob_hash JOIN rendition_attachments a ON a.build_id=x.build_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	versions, attachments, builds := stringSet(request.ContentVersionIDs), stringSet(request.AttachmentIDs), stringSet(request.BuildIDs)
	for rows.Next() {
		var version, attachment, build, until string
		if err = rows.Scan(&version, &attachment, &build, &until); err != nil {
			return err
		}
		_, v := versions[version]
		_, a := attachments[attachment]
		_, b := builds[build]
		if request.All || v || a || b {
			return fmt.Errorf("%w until %s", bundle.ErrRetained, until)
		}
	}
	return errors.Join(rows.Err(), rows.Close())
}
