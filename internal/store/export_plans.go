package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/canonical"
)

func validateExportPolicies(roles []bundle.RolePolicy) error {
	if len(roles) < 1 || len(roles) > 3 {
		return bundle.ErrLimit
	}
	seen := map[string]bool{}
	for _, p := range roles {
		if seen[p.Role] {
			return bundle.ErrConflict
		}
		seen[p.Role] = true
		switch p.Role {
		case "original":
			if p.ProfileFingerprint != "" || p.RecipeSHA256 != "" {
				return bundle.ErrConflict
			}
		case "text":
			if p.RecipeSHA256 != "" || p.ProfileFingerprint != "" && !canonical.IsSHA256Hex(p.ProfileFingerprint) {
				return bundle.ErrConflict
			}
		case "pages":
			if p.ProfileFingerprint != "" || p.RecipeSHA256 != "" && !canonical.IsSHA256Hex(p.RecipeSHA256) {
				return bundle.ErrConflict
			}
		default:
			return bundle.ErrConflict
		}
	}
	return nil
}

func loadExportPlan(ctx context.Context, q metadataQuerier, id string) (bundle.Plan, error) {
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT canonical_json FROM export_plans WHERE id=?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return bundle.Plan{}, ErrNotFound
	}
	if err != nil {
		return bundle.Plan{}, err
	}
	if len(raw) > bundle.MaxMemberBytes {
		return bundle.Plan{}, bundle.ErrLimit
	}
	var plan bundle.Plan
	err = json.Unmarshal(raw, &plan, json.RejectUnknownMembers(true))
	return plan, err
}

func (s *Store) ExportPlan(ctx context.Context, owner, id string) (bundle.Plan, error) {
	var actual string
	err := s.db.QueryRowContext(ctx, `SELECT owner FROM export_plans WHERE id=?`, id).Scan(&actual)
	if errors.Is(err, sql.ErrNoRows) || err == nil && actual != owner {
		return bundle.Plan{}, ErrNotFound
	}
	if err != nil {
		return bundle.Plan{}, err
	}
	p, err := loadExportPlan(ctx, s.db, id)
	if err != nil {
		return p, err
	}
	if exportExpired(p.ExpiresAt) {
		return p, bundle.ErrExpired
	}
	if p.Fingerprint == "" {
		return p, bundle.ErrConflict
	}
	return p, nil
}

func (s *Store) CreateExportPlan(ctx context.Context, owner string, r bundle.PlanRequest) (bundle.Plan, error) {
	if owner == "" || validateUUIDv4(r.OperationID) != nil || validateUUIDv4(r.SourceID) != nil || !canonical.IsSHA256Hex(r.MemberHash) {
		return bundle.Plan{}, bundle.ErrConflict
	}
	if err := validateExportPolicies(r.Roles); err != nil {
		return bundle.Plan{}, err
	}
	request, err := canonical.Marshal(r)
	if err != nil {
		return bundle.Plan{}, err
	}
	digest := pageChecksum(request)
	var plan bundle.Plan
	created := false
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var oldDigest, oldOwner string
		e := tx.QueryRowContext(ctx, `SELECT owner,request_sha256 FROM export_plans WHERE id=?`, r.OperationID).Scan(&oldOwner, &oldDigest)
		if e == nil {
			if oldOwner != owner {
				return ErrNotFound
			}
			if digest != oldDigest {
				return bundle.ErrConflict
			}
			plan, e = loadExportPlan(ctx, tx, r.OperationID)
			if e != nil {
				return e
			}
			if exportExpired(plan.ExpiresAt) {
				return bundle.ErrExpired
			}
			if plan.Fingerprint == "" {
				return bundle.ErrConflict
			}
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		source, e := loadExportSource(ctx, tx, owner, r.SourceID)
		if e != nil {
			return e
		}
		if exportExpired(source.ExpiresAt) {
			return bundle.ErrExpired
		}
		if source.State != "sealed" || source.MemberHash != r.MemberHash {
			return bundle.ErrConflict
		}
		var count int
		if e = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM export_sources)+(SELECT count(*) FROM export_plans)`).Scan(&count); e != nil {
			return e
		}
		if count >= 32 {
			return bundle.ErrLimit
		}
		plan = bundle.Plan{Format: bundle.Format, ID: r.OperationID, VaultID: s.vaultID, Toolchain: runtime.Version(), Source: source, Roles: slices.Clone(r.Roles), Total: source.Total, CreatedAt: nowRFC3339(), ExpiresAt: exportDeadline(10 * time.Minute)}
		raw, e := canonical.Marshal(plan)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO export_plans(id,owner,source_id,request_sha256,canonical_json,expires_at) VALUES(?,?,?,?,?,?)`, plan.ID, owner, source.ID, digest, raw, plan.ExpiresAt)
		created = e == nil
		return e
	})
	if err != nil || !created {
		return plan, err
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		index := 0
		csvBytes, directoryBytes := int64(1024), int64(1024)
		err := walkExportMembers(ctx, tx, r.SourceID, func(m bundle.Member) error {
			d, err := resolveExportDocument(ctx, tx, m, r.Roles)
			if err != nil {
				return err
			}
			raw, err := canonical.Marshal(d)
			if err != nil {
				return err
			}
			if len(raw) > bundle.MaxMemberBytes {
				return bundle.ErrLimit
			}
			plan.MetadataBytes += int64(len(raw))
			if plan.MetadataBytes > bundle.MaxMetadataBytes/2-(1<<20) {
				return bundle.ErrLimit
			}
			csvSize, err := bundle.CSVDocumentBytes(d)
			if err != nil {
				return err
			}
			csvBytes += csvSize
			if csvBytes > bundle.MaxMetadataBytes/4 {
				return bundle.ErrLimit
			}
			for _, role := range d.Roles {
				if role.Status == "unavailable" {
					continue
				}
				plan.RoleEntries++
				directoryBytes += int64(len(role.Path) + 74)
				if directoryBytes > bundle.MaxDirectoryBytes {
					return bundle.ErrLimit
				}
				if plan.RoleEntries > bundle.MaxRoles || role.Size > bundle.MaxRoleBytes-plan.RoleBytes {
					return bundle.ErrLimit
				}
				plan.RoleBytes += role.Size
				_, err = tx.ExecContext(ctx, `INSERT INTO export_role_roots(plan_id,blob_hash) VALUES(?,?) ON CONFLICT DO NOTHING`, plan.ID, role.SHA256)
				if err != nil {
					return err
				}
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO export_documents(plan_id,ordinal,canonical_json) VALUES(?,?,?)`, plan.ID, index, raw)
			index++
			return err
		})
		if err != nil {
			return err
		}
		if index != plan.Total {
			return bundle.ErrConflict
		}
		plan.Fingerprint, err = exportPlanFingerprint(ctx, tx, plan)
		if err != nil {
			return err
		}
		raw, err := canonical.Marshal(plan)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE export_plans SET canonical_json=? WHERE id=?`, raw, plan.ID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE export_sources SET expires_at=max(expires_at,?) WHERE id=?`, plan.ExpiresAt, r.SourceID)
		return err
	})
	return plan, err
}

func walkExportMembers(ctx context.Context, q metadataQuerier, id string, visit func(bundle.Member) error) error {
	var lastNode int64
	lastVersion := ""
	for {
		rows, err := q.QueryContext(ctx, `SELECT canonical_json FROM export_members WHERE source_id=? AND (node_id>? OR (node_id=? AND version_id>?)) ORDER BY node_id,version_id LIMIT 250`, id, lastNode, lastNode, lastVersion)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var page []bundle.Member
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&raw); err != nil {
				break
			}
			if len(raw) > bundle.MaxMemberBytes {
				err = bundle.ErrLimit
				break
			}
			var m bundle.Member
			if err = json.Unmarshal(raw, &m, json.RejectUnknownMembers(true)); err != nil {
				break
			}
			page = append(page, m)
		}
		err = errors.Join(err, rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		for _, m := range page {
			if err = visit(m); err != nil {
				return err
			}
			lastNode = m.NodeID
			lastVersion = m.VersionID
		}
		if len(page) < 250 {
			return nil
		}
	}
}

func walkExportDocumentBytes(ctx context.Context, q metadataQuerier, id string, visit func([]byte) error) error {
	for offset := 0; ; offset += 100 {
		rows, err := q.QueryContext(ctx, `SELECT canonical_json FROM export_documents WHERE plan_id=? AND ordinal>=? ORDER BY ordinal LIMIT 100`, id, offset)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var page [][]byte
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&raw); err != nil {
				break
			}
			if len(raw) > bundle.MaxMemberBytes {
				err = bundle.ErrLimit
				break
			}
			page = append(page, raw)
		}
		err = errors.Join(err, rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		for _, raw := range page {
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = visit(raw); err != nil {
				return err
			}
		}
		if len(page) < 100 {
			return nil
		}
	}
}

func (s *Store) WalkExportDocuments(ctx context.Context, id string, visit func(bundle.Document) error) error {
	return walkExportDocumentBytes(ctx, s.db, id, func(raw []byte) error {
		var d bundle.Document
		if err := json.Unmarshal(raw, &d, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		return visit(d)
	})
}

func exportPlanFingerprint(ctx context.Context, q metadataQuerier, p bundle.Plan) (string, error) {
	return bundle.Fingerprint(p, func(visit func(bundle.Document) error) error {
		return walkExportDocumentBytes(ctx, q, p.ID, func(raw []byte) error {
			var d bundle.Document
			if err := json.Unmarshal(raw, &d, json.RejectUnknownMembers(true)); err != nil {
				return err
			}
			return visit(d)
		})
	})
}

func resolveExportDocument(ctx context.Context, tx *sql.Tx, m bundle.Member, policies []bundle.RolePolicy) (bundle.Document, error) {
	d := bundle.Document{Member: m, Roles: []bundle.Role{}}
	var revision int64
	var trash sql.NullString
	var hash string
	var size int64
	err := tx.QueryRowContext(ctx, `SELECT n.name,n.revision,n.trashed_at,v.mime_type,v.blob_hash,v.size FROM nodes n JOIN content_versions v ON v.node_id=n.id WHERE n.id=? AND v.version_id=?`, m.NodeID, m.VersionID).Scan(&d.Name, &revision, &trash, &d.MediaType, &hash, &size)
	if err != nil {
		return d, err
	}
	if trash.Valid {
		return d, ErrNotFound
	}
	if hash != m.SHA256 || size != m.Size || m.Revision != 0 && m.Revision != revision {
		return d, bundle.ErrConflict
	}
	d.Path, err = pathOf(ctx, tx, m.NodeID)
	if err != nil {
		return d, err
	}
	if len(d.Path)+len(d.Name) > bundle.MaxMemberBytes/2 {
		return d, bundle.ErrLimit
	}
	base := fmt.Sprintf("documents/%d/%s/", m.NodeID, m.VersionID)
	for _, policy := range policies {
		roles := []bundle.Role{}
		switch policy.Role {
		case "original":
			roles = append(roles, bundle.Role{Role: "original", Status: "available", Path: base + "original", SHA256: m.SHA256, Size: m.Size, MediaType: d.MediaType})
		case "text":
			var role bundle.Role
			role, err = resolveExportText(ctx, tx, m, policy, base)
			if err == nil {
				roles = append(roles, role)
			}
		case "pages":
			roles, d.Frames, err = resolveExportPages(ctx, tx, m, policy, base)
		}
		if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, bundle.ErrUnavailable) {
			return d, err
		}
		if len(roles) == 0 || err != nil {
			if !policy.AllowUnavailable {
				return d, fmt.Errorf("%w: %s for %d/%s", bundle.ErrUnavailable, policy.Role, m.NodeID, m.VersionID)
			}
			roles = []bundle.Role{{Role: policy.Role, Status: "unavailable"}}
			if policy.Role == "pages" {
				d.Frames = nil
			}
			err = nil
		}
		d.Roles = append(d.Roles, roles...)
	}
	return d, nil
}

func resolveExportText(ctx context.Context, q metadataQuerier, m bundle.Member, p bundle.RolePolicy, base string) (bundle.Role, error) {
	r := bundle.Role{Role: "text", Status: "available", Path: base + "text.md", MediaType: "text/markdown; charset=utf-8"}
	var attachment, build, profile, request, evidence, checksum, source string
	var profileJSON []byte
	err := q.QueryRowContext(ctx, `SELECT a.attachment_id,a.build_id,a.profile_fingerprint,b.rendition_request_fingerprint,b.evidence_lexical_fingerprint,b.source_sha256,x.blob_hash,x.size,x.checksum,p.canonical_profile FROM rendition_heads h JOIN rendition_attachments a ON a.attachment_id=h.attachment_id JOIN rendition_builds b ON b.build_id=a.build_id JOIN rendition_artifacts x ON x.build_id=b.build_id JOIN processing_profiles p ON p.profile_fingerprint=a.profile_fingerprint WHERE h.content_version_id=? AND (?='' OR h.profile_fingerprint=?) AND x.role='sanitized_markdown' AND x.blob_hash=b.markdown_checksum AND length(p.canonical_profile)<=32768 ORDER BY h.profile_fingerprint,x.artifact_id LIMIT 1`, m.VersionID, p.ProfileFingerprint, p.ProfileFingerprint).Scan(&attachment, &build, &profile, &request, &evidence, &source, &r.SHA256, &r.Size, &checksum, &profileJSON)
	if err != nil {
		return r, err
	}
	if checksum != r.SHA256 || source != m.SHA256 {
		return r, bundle.ErrConflict
	}
	r.Recipe, err = canonical.Marshal(struct {
		Attachment string         `json:"attachment_id"`
		Build      string         `json:"build_id"`
		Profile    string         `json:"profile_fingerprint"`
		Request    string         `json:"rendition_request_fingerprint"`
		Evidence   string         `json:"evidence_lexical_fingerprint"`
		Definition jsontext.Value `json:"profile"`
	}{attachment, build, profile, request, evidence, profileJSON})
	return r, err
}

func resolveExportPages(ctx context.Context, q metadataQuerier, m bundle.Member, p bundle.RolePolicy, base string) ([]bundle.Role, []document.PageFrameV1, error) {
	// Bound the retained geometry before the page reader allocates it.
	var length int
	if err := q.QueryRowContext(ctx, `SELECT length(canonical_json) FROM page_documents WHERE version_id=?`, m.VersionID).Scan(&length); err != nil {
		return nil, nil, err
	}
	if length > bundle.MaxMemberBytes/2 {
		return nil, nil, bundle.ErrLimit
	}
	d, err := loadPageDocument(ctx, q, m.VersionID)
	if err != nil {
		return nil, nil, err
	}
	if d.Source.SHA256 != m.SHA256 || d.Source.Size != m.Size {
		return nil, nil, bundle.ErrConflict
	}
	var recipe string
	err = q.QueryRowContext(ctx, `SELECT recipe_sha256 FROM page_images WHERE version_id=? AND (?='' OR recipe_sha256=?) GROUP BY recipe_sha256 HAVING count(*)=? ORDER BY recipe_sha256 LIMIT 1`, m.VersionID, p.RecipeSHA256, p.RecipeSHA256, d.PageCount).Scan(&recipe)
	if err != nil {
		return nil, nil, err
	}
	roles := make([]bundle.Role, 0, d.PageCount)
	var bytes int
	for page := 1; page <= d.PageCount; page++ {
		view, err := loadPageImage(ctx, q, m.VersionID, recipe, page)
		if err != nil {
			return nil, nil, err
		}
		raw, err := canonical.Marshal(view.Recipe.Recipe)
		if err != nil {
			return nil, nil, err
		}
		bytes += len(raw) + 1024
		if bytes > bundle.MaxMemberBytes/2 {
			return nil, nil, bundle.ErrLimit
		}
		roles = append(roles, bundle.Role{Role: "pages", Status: "available", Path: fmt.Sprintf("%spages/%06d.png", base, page), SHA256: view.Image.SHA256, Size: view.Image.Size, MediaType: "image/png", Recipe: raw, Page: &view.Image})
	}
	return roles, d.Frames, nil
}
