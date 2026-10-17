package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// PhotoImportMember is one already-published source file in an import group.
// The importer supplies the source observation and physical receipt; the
// store decides membership and pairing in one transaction.
type PhotoImportMember struct {
	Name          string
	Role          string
	BlobHash      string
	Size          int64
	MediaType     string
	OriginalPath  string
	OriginalMtime string
	Physical      BlobPhysical
}

// PhotoImportGroup is one same-folder, same-stem unit. DestinationID is a
// virtual folder node and zero means the vault root.
type PhotoImportGroup struct {
	DestinationID int64
	// Successful groups rebase the plan so later groups retain the directory identity.
	DestinationPlan *IngestDirectoryPlan
	Members         []PhotoImportMember
}

// Ambiguity reasons tell the operator why a group was left for manual pairing.
const (
	PhotoImportMultipleRAW    = "multiple_raw"
	PhotoImportSeparatePhotos = "separate_photos"
)

// PhotoImportAmbiguity lists the same-name files an import would not pair
// on its own. The operator pairs them with a revision-checked attach.
type PhotoImportAmbiguity struct {
	Reason string                     `json:"reason"`
	Files  []PhotoImportAmbiguousFile `json:"files"`
}

type PhotoImportAmbiguousFile struct {
	SourcePath string `json:"source_path"`
	NodeID     int64  `json:"node_id"`
	AssetID    string `json:"asset_id,omitzero"`
	Role       string `json:"role"`
}

type PhotoImportResult struct {
	Asset     PhotoAsset
	Nodes     []Node
	Added     bool
	Skipped   bool
	Ambiguity *PhotoImportAmbiguity
}

// PhotoImportRequest is the request JSON of a photo_import operation.
type PhotoImportRequest struct {
	SourceRoot  string `json:"source_root"`
	Destination string `json:"destination"`
}

// PhotoImportReceipt is the progress and final receipt JSON of a
// photo_import operation. Final receipts include every scanned ambiguity.
// Unsupported counts scanned files whose type the import does not read.
type PhotoImportReceipt struct {
	Added       int64                  `json:"added"`
	Skipped     int64                  `json:"skipped"`
	Changed     int64                  `json:"changed"`
	Failed      int64                  `json:"failed"`
	Ambiguous   int64                  `json:"ambiguous"`
	Unsupported int64                  `json:"unsupported"`
	Ambiguities []PhotoImportAmbiguity `json:"ambiguities,omitzero"`
}

func photoImportSourceKey(path string) (folder, stem string) {
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	stem = strings.TrimSuffix(base, filepath.Ext(base))
	if ClassifyPhotoSource(base).Kind == PhotoSourceSidecar {
		source := ClassifyPhotoSource(stem)
		if source.Kind == PhotoSourceRAW || source.Kind == PhotoSourceImage {
			stem = strings.TrimSuffix(stem, filepath.Ext(stem))
		}
	}
	return norm.NFC.String(filepath.Clean(filepath.Dir(clean))), norm.NFC.String(stem)
}

func photoImportSourcePath(path string) string {
	return norm.NFC.String(filepath.Clean(path))
}

// PhotoImportGroupKey returns the grouping identity for one discovered source.
// A video keys on its full path so it never joins a same-name photo.
func PhotoImportGroupKey(path string, kind PhotoSourceKind) string {
	if kind == PhotoSourceVideo {
		return base64.RawURLEncoding.EncodeToString([]byte("video\x00" + filepath.Clean(path)))
	}
	folder, stem := photoImportSourceKey(path)
	return base64.RawURLEncoding.EncodeToString([]byte("photo\x00" + folder + "\x00" + stem))
}

func photoImportMemberRole(member PhotoImportMember) (string, string, string, error) {
	source := ClassifyPhotoSource(member.Name)
	role := member.Role
	if role == "" {
		role = source.Role
	}
	mediaType := member.MediaType
	if mediaType == "" {
		mediaType = source.MediaType
	}
	kind := source.AssetKind
	if role == PhotoRoleRAW || role == PhotoRoleImage || role == PhotoRoleSidecar {
		kind = PhotoKindPhoto
	}
	if role == PhotoRoleVideo {
		kind = PhotoKindVideo
	}
	if !photoRoleValid(role) || kind == "" {
		return "", "", "", fmt.Errorf("%w: unsupported photo import member %q", ErrPhotoNodeNotEligible, member.Name)
	}
	return role, mediaType, kind, nil
}

func photoImportNodeFacts(node Node, member PhotoImportMember, role string) PhotoNodeFacts {
	facts := photoNodeFacts(node)
	source := ClassifyPhotoSource(member.Name)
	if source.Role == role && source.AssetKind != "" {
		facts.MediaFamily = "image"
		if source.Kind == PhotoSourceVideo {
			facts.MediaFamily = "audio_video"
		}
		facts.MediaType = source.MediaType
		facts.Qualifies = role != PhotoRoleSidecar
		facts.AssetKind = source.AssetKind
	}
	return facts
}

// photoImportRoleFilter matches the photo_files rows a member of role can be
// the same file as. Plain nodes count only for sidecars: a lone sidecar was
// imported as a plain file. Ordinary ingestion enrolls a RAW as an image.
func photoImportRoleFilter(role string) (string, []any) {
	switch role {
	case PhotoRoleSidecar:
		return "(pf.node_id IS NULL OR pf.role=?)", []any{role}
	case PhotoRoleRAW:
		return "pf.role IN (?,?)", []any{role, PhotoRoleImage}
	default:
		return "pf.role=?", []any{role}
	}
}

// photoImportSidecarCandidatesQuery finds sidecars by source path first: a
// rescan can supply the outside bytes Docbank accepted before writing a newer
// version, which no longer match the current content.
const photoImportSidecarCandidatesQuery = `SELECT ` + nodeCols + `,p.original_path,COALESCE(pf.source_sha256,'')
 FROM provenance p INDEXED BY provenance_source_path
 JOIN nodes n ON n.id=p.node_id
 LEFT JOIN content_versions cv ON cv.node_id=n.id AND cv.version_id=n.current_version_id
 LEFT JOIN photo_files pf ON pf.node_id=n.id AND pf.asset_id IS NOT NULL
 WHERE p.original_path=? AND n.trashed_at IS NULL AND (pf.node_id IS NULL OR pf.role='sidecar')
 AND NOT EXISTS(SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)
 ORDER BY n.id,p.identity`

func (s *Store) photoImportCurrentDuplicateTx(
	ctx context.Context, tx *sql.Tx, run IngestRun, member PhotoImportMember, role, target string,
	fallback bool, claimed map[int64]bool,
) (Node, bool, error) {
	var node Node
	photoFilter, roleArgs := photoImportRoleFilter(role)
	query, args := `
		SELECT `+nodeCols+`, COALESCE(p.original_path, ''), ''
		FROM `+nodeFrom+`
		LEFT JOIN photo_files pf ON pf.node_id=n.id AND pf.asset_id IS NOT NULL
		LEFT JOIN provenance p ON p.node_id=n.id
		  AND NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)
		WHERE n.trashed_at IS NULL AND `+photoFilter+` AND cv.blob_hash=?
		ORDER BY n.id, p.identity`, slices.Concat(roleArgs, []any{member.BlobHash})
	if role == PhotoRoleSidecar {
		query, args = photoImportSidecarCandidatesQuery, []any{member.OriginalPath}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return Node{}, false, fmt.Errorf("finding duplicate photo member: %w", err)
	}
	defer func() { _ = rows.Close() }()
	source := photoImportSourcePath(member.OriginalPath)
	found, content := false, Node{}
	for rows.Next() {
		var row Node
		var sourcePath, accepted string
		if err := rows.Scan(
			&row.ID, &row.ParentID, &row.Name, &row.Kind, &row.CurrentVersionID,
			&row.BlobHash, &row.MD5, &row.Size, &row.MimeType, &row.Revision,
			&row.CreatedAt, &row.ModifiedAt, &row.TrashedAt, &sourcePath, &accepted,
		); err != nil {
			return Node{}, false, fmt.Errorf("scanning duplicate photo member: %w", err)
		}
		if row.BlobHash != member.BlobHash && accepted != member.BlobHash {
			continue
		}
		// Another member of this group already holds the node, so it can't also be this file.
		if claimed[row.ID] {
			continue
		}
		if sourcePath != "" && photoImportSourcePath(sourcePath) == source {
			node, found = row, true
			break
		}
		if content.ID == 0 {
			content = row
		}
	}
	if err := rows.Err(); err != nil {
		return Node{}, false, fmt.Errorf("reading duplicate photo members: %w", err)
	}
	_ = rows.Close()
	switch {
	case found, !fallback:
	case role != PhotoRoleSidecar && content.ID != 0:
		// RAW and image files skip duplicates by content across folders; only the same source file is a sidecar's duplicate.
		node, found = content, true
	case role == PhotoRoleSidecar && target != "":
		// A moved or re-cased folder changes the path, but the photo already holding these bytes is the same shot.
		if node, found, err = photoImportTargetSidecarTx(ctx, tx, target, member.BlobHash, claimed); err != nil {
			return Node{}, false, err
		}
	}
	if !found {
		return Node{}, false, nil
	}
	if err := s.EnsureBlobTx(tx, member.BlobHash, member.Size, photoImportPhysical(member)...); err != nil {
		return Node{}, false, fmt.Errorf("reconciling duplicate photo content: %w", err)
	}
	// Unchanged outside bytes keep a newer Docbank version current and bind
	// provenance to the version that holds them, while it is retained.
	version := node.CurrentVersionID
	if node.BlobHash != member.BlobHash {
		err := tx.QueryRowContext(ctx, `SELECT version_id FROM content_versions WHERE node_id=? AND blob_hash=?
 ORDER BY node_revision DESC LIMIT 1`, node.ID, member.BlobHash).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			version = ""
		} else if err != nil {
			return Node{}, false, err
		}
	}
	observed, err := s.observePhotoImportSourceTx(ctx, tx, run, node, member, version)
	if err != nil {
		return Node{}, false, err
	}
	return observed, true, nil
}

// photoImportChangedSourceTx finds the live node last imported from the
// member's exact source path and records the member's bytes as its new
// version, so an edited sidecar or JPEG stays one photo member.
func (s *Store) photoImportChangedSourceTx(
	ctx context.Context, tx *sql.Tx, run IngestRun, member PhotoImportMember, role, mediaType string,
	claimed map[int64]bool,
) (Node, bool, error) {
	photoFilter, roleArgs := photoImportRoleFilter(role)
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT n.id FROM nodes n
		JOIN provenance p ON p.node_id=n.id
		  AND NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)
		LEFT JOIN photo_files pf ON pf.node_id=n.id AND pf.asset_id IS NOT NULL
		WHERE n.kind='file' AND n.trashed_at IS NULL AND `+photoFilter+` AND p.original_path=?
		  -- A node shared with another source path keeps its bytes; the edit becomes its own file.
		  AND NOT EXISTS (SELECT 1 FROM provenance other
		    WHERE other.node_id=n.id AND other.original_path IS NOT ?
		      AND NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=other.identity))
		ORDER BY n.id`, append(roleArgs, member.OriginalPath, member.OriginalPath)...)
	if err != nil {
		return Node{}, false, fmt.Errorf("finding changed photo source: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var nodeID int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return Node{}, false, fmt.Errorf("scanning changed photo source: %w", err)
		}
		if !claimed[id] {
			nodeID = id
			break
		}
	}
	if err := rows.Err(); err != nil {
		return Node{}, false, fmt.Errorf("reading changed photo sources: %w", err)
	}
	_ = rows.Close()
	if nodeID == 0 {
		return Node{}, false, nil
	}
	prior, err := nodeByIDTx(tx, nodeID)
	if err != nil {
		return Node{}, false, err
	}
	replaced, _, err := s.replaceContentTx(ctx, tx, prior, prior.Revision,
		member.BlobHash, member.Size, mediaType, photoImportPhysical(member)...)
	if err != nil {
		return Node{}, false, fmt.Errorf("recording changed photo source %q: %w", member.OriginalPath, err)
	}
	if role == PhotoRoleSidecar {
		if _, err := tx.ExecContext(ctx, `UPDATE photo_files SET source_sha256=? WHERE node_id=? AND role='sidecar'`, member.BlobHash, replaced.ID); err != nil {
			return Node{}, false, err
		}
	}
	observed, err := s.observePhotoImportSourceTx(ctx, tx, run, replaced, member, replaced.CurrentVersionID)
	if err != nil {
		return Node{}, false, err
	}
	return observed, true, nil
}

func photoImportPhysical(member PhotoImportMember) []BlobPhysical {
	if member.Physical.Encoding == "" {
		return nil
	}
	return []BlobPhysical{member.Physical}
}

// observePhotoImportSourceTx records that this run read member's source file
// as node.
func (s *Store) observePhotoImportSourceTx(
	ctx context.Context, tx *sql.Tx, run IngestRun, node Node, member PhotoImportMember, versionID string,
) (Node, error) {
	inserted, err := s.ensureIngestRunForMutationTx(ctx, tx, run)
	if err != nil {
		return Node{}, err
	}
	var originalMtime *string
	if member.OriginalMtime != "" {
		originalMtime = &member.OriginalMtime
	}
	fact := metadataProvenance{Type: metadataProvenanceType, NodeID: node.ID,
		IngestID: run.record.ID, OriginalPath: member.OriginalPath, OriginalMTime: originalMtime}
	if err := validateProvenanceFields(fact); err != nil {
		return Node{}, fmt.Errorf("validating photo import observation: %w", err)
	}
	fact.Identity, err = provenanceIdentity(fact)
	if err != nil {
		return Node{}, fmt.Errorf("identifying photo import observation: %w", err)
	}
	return s.observeOperationalIngestTx(ctx, tx, run, node, fact, inserted, versionID)
}

func photoImportTargetSidecarTx(
	ctx context.Context, tx *sql.Tx, assetID, blobHash string, claimed map[int64]bool,
) (Node, bool, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT `+nodeCols+`
		FROM `+nodeFrom+`
		JOIN photo_files pf ON pf.node_id=n.id AND pf.asset_id=? AND pf.role=?
		WHERE n.trashed_at IS NULL AND (cv.blob_hash=? OR pf.source_sha256=?)
		ORDER BY n.id`, assetID, PhotoRoleSidecar, blobHash, blobHash)
	if err != nil {
		return Node{}, false, fmt.Errorf("finding photo sidecar by content: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var node Node
		if err := rows.Scan(
			&node.ID, &node.ParentID, &node.Name, &node.Kind, &node.CurrentVersionID,
			&node.BlobHash, &node.MD5, &node.Size, &node.MimeType, &node.Revision,
			&node.CreatedAt, &node.ModifiedAt, &node.TrashedAt,
		); err != nil {
			return Node{}, false, fmt.Errorf("scanning photo sidecar by content: %w", err)
		}
		if !claimed[node.ID] {
			return node, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return Node{}, false, fmt.Errorf("reading photo sidecars by content: %w", err)
	}
	return Node{}, false, nil
}

// IngestPhotoGroup publishes one group of already-durable bytes and commits
// its nodes, provenance, and photo membership in one logical transaction.
// One RAW and at most one existing photo pair automatically; anything more is
// left unpaired and reported for the operator. A lone sidecar stays a plain
// file until a scan includes its same-name RAW or image.
func (s *Store) IngestPhotoGroup(ctx context.Context, run IngestRun, group PhotoImportGroup) (result PhotoImportResult, retErr error) {
	if err := requireOperationalIngestRun(run); err != nil {
		return result, err
	}
	if len(group.Members) == 0 {
		return result, errors.New("photo import group has no members")
	}
	if group.DestinationID == 0 {
		group.DestinationID = s.RootID()
	}
	var resolution IngestDirectoryResolution
	defer func() {
		if retErr == nil && group.DestinationPlan != nil {
			*group.DestinationPlan = resolution.Rebase(*group.DestinationPlan)
		}
	}()
	retErr = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if group.DestinationPlan != nil {
			destination, resolved, err := s.ensureIngestDirectoryTx(ctx, tx, *group.DestinationPlan)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					return fmt.Errorf("photo import destination was trashed or removed; "+
						"choose the folder again: %w", err)
				}
				return err
			}
			group.DestinationID, resolution = destination.ID, resolved
		}
		count := len(group.Members)
		nodes := make([]Node, count)
		roles := make([]string, count)
		kinds := make([]string, count)
		owners := make([]string, count)
		mediaTypes := make([]string, count)
		added := false
		for i, member := range group.Members {
			role, mediaType, kind, err := photoImportMemberRole(member)
			if err != nil {
				return err
			}
			if role == PhotoRoleVideo {
				if count != 1 {
					return errors.New("video import must have one member")
				}
			}
			if member.BlobHash == "" || member.Size < 0 || member.OriginalPath == "" {
				return fmt.Errorf("photo import member %q lacks a verified source identity", member.Name)
			}
			roles[i], kinds[i], mediaTypes[i] = role, kind, mediaType
		}
		decided := make([]bool, count)
		claimed := make(map[int64]bool, count)
		settle := func(i int, node Node, duplicate bool) error {
			member, role := group.Members[i], roles[i]
			if !duplicate {
				physical := []BlobPhysical(nil)
				if member.Physical.Encoding != "" {
					physical = []BlobPhysical{member.Physical}
				}
				options := ingestFileOptions{observeMembership: true, deferPhotoEnrollment: true}
				name, err := NormalizeName(member.Name)
				if err != nil {
					return err
				}
				// Generic reuse matches by basename alone, so another folder's identical sidecar gets its own name.
				options.exact = role == PhotoRoleSidecar
				if !options.exact {
					_, existingID, reuse, err := resolveIngestNameTx(tx, group.DestinationID, name, member.BlobHash, run.record.SourceKind)
					if err != nil {
						return err
					}
					// Name reuse can hand back a node another member of this group already holds.
					options.exact = reuse && claimed[existingID]
				}
				if options.exact {
					if name, _, _, err = resolveIngestNameTx(tx, group.DestinationID, name, "", run.record.SourceKind); err != nil {
						return err
					}
				}
				receipt, created, _, err := s.ingestFileTx(ctx, tx, run, group.DestinationID,
					name, member.BlobHash, member.Size, mediaTypes[i], member.OriginalPath,
					member.OriginalMtime, options, physical...)
				if err != nil {
					return err
				}
				node = receipt.Node
				added = added || created
			}
			nodes[i], decided[i], claimed[node.ID] = node, true, true
			var err error
			owners[i], _, err = photoAssetOwningNodeTx(ctx, tx, node.ID)
			return err
		}
		// Every member first takes the node it was imported as, so no other member's content match can take it.
		// An edited source becomes that node's new version. RAW and image files then settle by
		// content so sidecars can see which photo the group joins.
		for i, member := range group.Members {
			node, duplicate, err := s.photoImportCurrentDuplicateTx(ctx, tx, run, member, roles[i], "", false, claimed)
			if err != nil {
				return err
			}
			if !duplicate {
				node, duplicate, err = s.photoImportChangedSourceTx(ctx, tx, run, member, roles[i], mediaTypes[i], claimed)
				if err != nil {
					return err
				}
				added = added || duplicate
			}
			if duplicate {
				if err := settle(i, node, true); err != nil {
					return err
				}
			}
		}
		for i, member := range group.Members {
			if decided[i] || roles[i] == PhotoRoleSidecar {
				continue
			}
			node, duplicate, err := s.photoImportCurrentDuplicateTx(ctx, tx, run, member, roles[i], "", true, claimed)
			if err != nil {
				return err
			}
			if err := settle(i, node, duplicate); err != nil {
				return err
			}
		}
		joined := ""
		for i := range owners {
			if !decided[i] || owners[i] == "" || owners[i] == joined {
				continue
			}
			if joined != "" {
				joined = ""
				break
			}
			joined = owners[i]
		}
		for i, member := range group.Members {
			if decided[i] {
				continue
			}
			node, duplicate, err := s.photoImportCurrentDuplicateTx(ctx, tx, run, member, roles[i], joined, true, claimed)
			if err != nil {
				return err
			}
			if err := settle(i, node, duplicate); err != nil {
				return err
			}
		}
		result.Nodes = nodes

		assets := make(map[string]struct{})
		raws := make(map[int64]struct{})
		unownedRAW := false
		for i := range nodes {
			if owners[i] != "" {
				assets[owners[i]] = struct{}{}
			}
			if roles[i] == PhotoRoleRAW {
				raws[nodes[i].ID] = struct{}{}
				unownedRAW = unownedRAW || owners[i] == ""
			}
		}
		for assetID := range assets {
			files, err := loadPhotoFiles(ctx, tx, assetID)
			if err != nil {
				return err
			}
			for _, file := range files {
				if file.Role == PhotoRoleRAW {
					raws[file.NodeID] = struct{}{}
				}
			}
		}
		reason := ""
		switch {
		// RAWs the operator already paired into one photo are settled.
		case len(raws) > 1 && (unownedRAW || len(assets) > 1):
			reason = PhotoImportMultipleRAW
		case len(assets) > 1:
			reason = PhotoImportSeparatePhotos
		}
		if reason != "" {
			created, err := s.importUnpairedPhotosTx(ctx, tx, nodes, roles, kinds, owners)
			if err != nil {
				return err
			}
			result.Ambiguity = photoImportAmbiguity(reason, group.Members, nodes, roles, owners)
			result.Added = added || created
			result.Skipped = !result.Added
			return nil
		}

		targetID := ""
		for assetID := range assets {
			targetID = assetID
		}
		var target PhotoAsset
		if targetID == "" {
			primary := -1
			for i, role := range roles {
				if role == PhotoRoleRAW || primary < 0 && role != PhotoRoleSidecar {
					primary = i
				}
			}
			if primary < 0 {
				result.Added, result.Skipped = added, !added
				return nil
			}
			created, err := s.createPhotoAssetWithReceiptTx(ctx, tx, nodes[primary].ID, roles[primary], kinds[primary], "create")
			if err != nil {
				return err
			}
			owners[primary], target, added = created.ID, created, true
		} else {
			var err error
			if target, err = photoAssetByIDQuery(ctx, tx, targetID); err != nil {
				return err
			}
		}
		attached := false
		for _, sidecars := range []bool{false, true} {
			for i, node := range nodes {
				if (roles[i] == PhotoRoleSidecar) != sidecars {
					continue
				}
				if owners[i] != "" {
					continue
				}
				file := PhotoFile{AssetID: target.ID, NodeID: node.ID, Role: roles[i], CreatedAt: nowRFC3339()}
				if roles[i] == PhotoRoleSidecar {
					sourceID, _, err := photoSidecarSourceTx(ctx, tx, target.ID, group.Members[i].OriginalPath, "", false)
					if err != nil {
						return err
					}
					if sourceID == "" {
						continue
					}
					file.SidecarOfID = &sourceID
				} else if err := validatePhotoFileForAsset(target.Kind, roles[i], photoImportNodeFacts(node, group.Members[i], roles[i])); err != nil {
					return err
				}
				if err := s.insertPhotoFileTx(ctx, tx, file); err != nil {
					return err
				}
				attached = true
			}
		}
		if attached {
			updated, err := commitPhotoAssetTx(ctx, tx, target, target, "import")
			if err != nil {
				return err
			}
			if err := validatePhotoAssetGraph(ctx, tx, updated.ID); err != nil {
				return err
			}
			target = updated
		}
		result.Asset = target
		result.Added = added || attached
		result.Skipped = !result.Added
		return nil
	})
	return result, retErr
}

// importUnpairedPhotosTx gives each unowned RAW, image, or video its own
// photo. Sidecars stay plain files until the operator pairs the group.
func (s *Store) importUnpairedPhotosTx(ctx context.Context, tx *sql.Tx, nodes []Node, roles, kinds, owners []string) (bool, error) {
	created := false
	for i, node := range nodes {
		if owners[i] != "" || roles[i] == PhotoRoleSidecar {
			continue
		}
		asset, err := s.createPhotoAssetWithReceiptTx(ctx, tx, node.ID, roles[i], kinds[i], "create")
		if err != nil {
			return false, err
		}
		owners[i], created = asset.ID, true
	}
	return created, nil
}

func photoSidecarSourceTx(ctx context.Context, q metadataQuerier, assetID, sidecarPath, extension string, vaultNames bool) (string, bool, error) {
	name := filepath.Base(sidecarPath)
	original := strings.TrimSuffix(name, filepath.Ext(name))
	source := ClassifyPhotoSource(original)
	bound := source.Kind == PhotoSourceRAW || source.Kind == PhotoSourceImage
	if !bound && extension != "" {
		original += "." + strings.TrimPrefix(extension, ".")
		bound = true
	}
	rows, err := q.QueryContext(ctx, `SELECT f.file_id,f.role,n.name,COALESCE(p.original_path,'') FROM photo_files f
 JOIN nodes n ON n.id=f.node_id LEFT JOIN provenance p ON p.node_id=n.id
 AND EXISTS (SELECT 1 FROM ingests i WHERE i.id=p.ingest_id AND i.source_kind NOT LIKE 'embedded:%')
 AND NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)
 WHERE f.asset_id=? AND f.role IN ('raw','image') AND n.trashed_at IS NULL ORDER BY f.file_id`, assetID)
	if err != nil {
		return "", bound, err
	}
	defer func() { _ = rows.Close() }()
	id, fallback := "", ""
	for rows.Next() {
		var fileID, role, nodeName, path string
		if err := rows.Scan(&fileID, &role, &nodeName, &path); err != nil {
			return "", bound, err
		}
		if role == PhotoRoleRAW || fallback == "" {
			fallback = fileID
		}
		candidate := nodeName
		if path != "" && !vaultNames {
			if !strings.EqualFold(filepath.Dir(path), filepath.Dir(sidecarPath)) {
				continue
			}
			candidate = filepath.Base(path)
		}
		if bound && strings.EqualFold(norm.NFC.String(candidate), norm.NFC.String(original)) {
			if id != "" && id != fileID {
				return "", true, nil
			}
			id = fileID
		}
	}
	if err := rows.Err(); err != nil {
		return "", bound, err
	}
	if !bound {
		id = fallback
	}
	return id, bound, nil
}

func photoImportAmbiguity(
	reason string, members []PhotoImportMember, nodes []Node, roles, owners []string,
) *PhotoImportAmbiguity {
	ambiguity := &PhotoImportAmbiguity{Reason: reason}
	for i, node := range nodes {
		ambiguity.Files = append(ambiguity.Files, PhotoImportAmbiguousFile{
			SourcePath: members[i].OriginalPath, NodeID: node.ID, AssetID: owners[i], Role: roles[i],
		})
	}
	return ambiguity
}

// PreparePhotoImportDestination admits a stable destination without creating directories.
func (s *Store) PreparePhotoImportDestination(
	ctx context.Context, path string,
) (IngestDirectoryPlan, error) {
	plan, err := s.PrepareIngestDirectory(ctx, path)
	if err != nil {
		return plan, err
	}
	active, err := auditAuthorityActiveTx(ctx, s.db)
	if err != nil {
		return IngestDirectoryPlan{}, err
	}
	if active {
		return IngestDirectoryPlan{}, ErrAuditMutationUnsupported
	}
	return plan, nil
}

// EnsurePhotoImportDestination resolves and creates the admitted path in one
// logical transaction (unlike PreparePhotoImportDestination, which only
// snapshots it) so a directory trashed between resolution and creation can't
// leave this call racing against that removal.
func (s *Store) EnsurePhotoImportDestination(ctx context.Context, path string) (Node, error) {
	segments, err := normalizeIngestDirectorySegments(path, splitPath(path))
	if err != nil {
		return Node{}, err
	}
	var destination Node
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		plan := IngestDirectoryPlan{anchorID: s.rootID, segments: segments}
		destination, _, err = s.ensureIngestDirectoryTx(ctx, tx, plan)
		return err
	})
	return destination, err
}
